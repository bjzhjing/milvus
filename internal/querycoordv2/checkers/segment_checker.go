// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package checkers

import (
	"context"
	"time"

	"github.com/samber/lo"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/querycoordv2/balance"
	"github.com/milvus-io/milvus/internal/querycoordv2/meta"
	. "github.com/milvus-io/milvus/internal/querycoordv2/params"
	"github.com/milvus-io/milvus/internal/querycoordv2/session"
	"github.com/milvus-io/milvus/internal/querycoordv2/task"
	"github.com/milvus-io/milvus/internal/querycoordv2/utils"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/util/typeutil"
)

type SegmentChecker struct {
	baseChecker
	meta      *meta.Meta
	dist      *meta.DistributionManager
	targetMgr *meta.TargetManager
	balancer  balance.Balance
	nodeMgr   *session.NodeManager
}

func NewSegmentChecker(
	meta *meta.Meta,
	dist *meta.DistributionManager,
	targetMgr *meta.TargetManager,
	balancer balance.Balance,
	nodeMgr *session.NodeManager,
) *SegmentChecker {
	return &SegmentChecker{
		meta:      meta,
		dist:      dist,
		targetMgr: targetMgr,
		balancer:  balancer,
		nodeMgr:   nodeMgr,
	}
}

func (c *SegmentChecker) Description() string {
	return "SegmentChecker checks the lack of segments, or some segments are redundant"
}

func (c *SegmentChecker) readyToCheck(collectionID int64) bool {
	metaExist := (c.meta.GetCollection(collectionID) != nil)
	targetExist := c.targetMgr.IsNextTargetExist(collectionID) || c.targetMgr.IsCurrentTargetExist(collectionID)

	return metaExist && targetExist
}

func (c *SegmentChecker) Check(ctx context.Context) []task.Task {
	collectionIDs := c.meta.CollectionManager.GetAll()
	tasks := make([]task.Task, 0)
	for _, cid := range collectionIDs {
		if c.readyToCheck(cid) {
			replicas := c.meta.ReplicaManager.GetByCollection(cid)
			for _, r := range replicas {
				tasks = append(tasks, c.checkReplica(ctx, r)...)
			}
		}
	}

	// find already released segments which are not contained in target
	segments := c.dist.SegmentDistManager.GetAll()
	released := utils.FilterReleased(segments, collectionIDs)
	tasks = append(tasks, c.createSegmentReduceTasks(ctx, released, -1, querypb.DataScope_Historical)...)
	task.SetPriority(task.TaskPriorityNormal, tasks...)
	return tasks
}

func (c *SegmentChecker) checkReplica(ctx context.Context, replica *meta.Replica) []task.Task {
	log := log.Ctx(ctx).WithRateGroup("qcv2.SegmentChecker", 1, 60).With(
		zap.Int64("collectionID", replica.CollectionID),
		zap.Int64("replicaID", replica.ID))
	ret := make([]task.Task, 0)

	// get channel dist by replica (ch -> node list), cause more then one delegator may exists during channel balance.
	// if more than one delegator exist, load/release segment may causes chaos, so we can skip it until channel balance finished.
	dist := c.dist.ChannelDistManager.GetChannelDistByReplica(replica)
	for ch, nodes := range dist {
		if len(nodes) > 1 {
			log.Info("skip check segment due to two shard leader exists",
				zap.String("channelName", ch))
			return ret
		}
	}

	// compare with targets to find the lack and redundancy of segments
	lacks, redundancies := c.getHistoricalSegmentDiff(replica.GetCollectionID(), replica.GetID())
	tasks := c.createSegmentLoadTasks(ctx, lacks, replica)
	task.SetReason("lacks of segment", tasks...)
	ret = append(ret, tasks...)

	redundancies = c.filterSegmentInUse(replica, redundancies)
	tasks = c.createSegmentReduceTasks(ctx, redundancies, replica.GetID(), querypb.DataScope_Historical)
	task.SetReason("segment not exists in target", tasks...)
	ret = append(ret, tasks...)

	// compare inner dists to find repeated loaded segments
	redundancies = c.findRepeatedHistoricalSegments(replica.GetID())
	redundancies = c.filterExistedOnLeader(replica, redundancies)
	tasks = c.createSegmentReduceTasks(ctx, redundancies, replica.GetID(), querypb.DataScope_Historical)
	task.SetReason("redundancies of segment", tasks...)
	ret = append(ret, tasks...)

	// compare with target to find the lack and redundancy of segments
	_, redundancies = c.getStreamingSegmentDiff(replica.GetCollectionID(), replica.GetID())
	tasks = c.createSegmentReduceTasks(ctx, redundancies, replica.GetID(), querypb.DataScope_Streaming)
	task.SetReason("streaming segment not exists in target", tasks...)
	ret = append(ret, tasks...)

	return ret
}

// GetStreamingSegmentDiff get streaming segment diff between leader view and target
func (c *SegmentChecker) getStreamingSegmentDiff(collectionID int64,
	replicaID int64) (toLoad []*datapb.SegmentInfo, toRelease []*meta.Segment) {
	replica := c.meta.Get(replicaID)
	if replica == nil {
		log.Info("replica does not exist, skip it")
		return
	}

	log := log.Ctx(context.TODO()).WithRateGroup("qcv2.SegmentChecker", 1, 60).With(
		zap.Int64("collectionID", collectionID),
		zap.Int64("replicaID", replica.ID))

	leaders := c.dist.ChannelDistManager.GetShardLeadersByReplica(replica)
	//	distMgr.LeaderViewManager.
	for channelName, node := range leaders {
		view := c.dist.LeaderViewManager.GetLeaderShardView(node, channelName)
		if view == nil {
			log.Info("leaderView is not ready, skip", zap.String("channelName", channelName), zap.Int64("node", node))
			continue
		}
		targetVersion := c.targetMgr.GetCollectionTargetVersion(collectionID, meta.CurrentTarget)
		if view.TargetVersion != targetVersion {
			// before shard delegator update it's readable version, skip release segment
			log.RatedInfo(20, "before shard delegator update it's readable version, skip release segment",
				zap.String("channelName", channelName),
				zap.Int64("nodeID", node),
				zap.Int64("leaderVersion", view.TargetVersion),
				zap.Int64("currentVersion", targetVersion),
			)
			continue
		}

		nextTargetSegmentIDs := c.targetMgr.GetStreamingSegmentsByCollection(collectionID, meta.NextTarget)
		currentTargetSegmentIDs := c.targetMgr.GetStreamingSegmentsByCollection(collectionID, meta.CurrentTarget)
		currentTargetChannelMap := c.targetMgr.GetDmChannelsByCollection(collectionID, meta.CurrentTarget)

		// get segment which exist on leader view, but not on current target and next target
		for _, segment := range view.GrowingSegments {
			if !currentTargetSegmentIDs.Contain(segment.GetID()) && !nextTargetSegmentIDs.Contain(segment.GetID()) {
				if channel, ok := currentTargetChannelMap[segment.InsertChannel]; ok {
					timestampInSegment := segment.GetStartPosition().GetTimestamp()
					timestampInTarget := channel.GetSeekPosition().GetTimestamp()
					// filter toRelease which seekPosition is newer than next target dmChannel
					if timestampInSegment < timestampInTarget {
						log.Info("growing segment not exist in target, so release it",
							zap.Int64("segmentID", segment.GetID()),
						)
						toRelease = append(toRelease, segment)
					}
				}
			}
		}
	}

	return
}

// GetHistoricalSegmentDiff get historical segment diff between target and dist
func (c *SegmentChecker) getHistoricalSegmentDiff(
	collectionID int64,
	replicaID int64) (toLoad []*datapb.SegmentInfo, toRelease []*meta.Segment) {
	replica := c.meta.Get(replicaID)
	if replica == nil {
		log.Info("replica does not exist, skip it")
		return
	}
	dist := c.getHistoricalSegmentsDist(replica)
	distMap := typeutil.NewUniqueSet()
	for _, s := range dist {
		distMap.Insert(s.GetID())
	}

	nextTargetMap := c.targetMgr.GetHistoricalSegmentsByCollection(collectionID, meta.NextTarget)
	currentTargetMap := c.targetMgr.GetHistoricalSegmentsByCollection(collectionID, meta.CurrentTarget)

	// Segment which exist on next target, but not on dist
	for segmentID, segment := range nextTargetMap {
		if !distMap.Contain(segmentID) {
			toLoad = append(toLoad, segment)
		}
	}

	// get segment which exist on dist, but not on current target and next target
	for _, segment := range dist {
		_, existOnCurrent := currentTargetMap[segment.GetID()]
		_, existOnNext := nextTargetMap[segment.GetID()]

		if !existOnNext && !existOnCurrent {
			toRelease = append(toRelease, segment)
		}
	}

	return
}

func (c *SegmentChecker) getHistoricalSegmentsDist(replica *meta.Replica) []*meta.Segment {
	ret := make([]*meta.Segment, 0)
	for _, node := range replica.GetNodes() {
		ret = append(ret, c.dist.SegmentDistManager.GetByCollectionAndNode(replica.CollectionID, node)...)
	}
	return ret
}

func (c *SegmentChecker) findRepeatedHistoricalSegments(replicaID int64) []*meta.Segment {
	segments := make([]*meta.Segment, 0)
	replica := c.meta.Get(replicaID)
	if replica == nil {
		log.Info("replica does not exist, skip it")
		return segments
	}
	dist := c.getHistoricalSegmentsDist(replica)
	versions := make(map[int64]*meta.Segment)
	for _, s := range dist {
		maxVer, ok := versions[s.GetID()]
		if !ok {
			versions[s.GetID()] = s
			continue
		}
		if maxVer.Version <= s.Version {
			segments = append(segments, maxVer)
			versions[s.GetID()] = s
		} else {
			segments = append(segments, s)
		}
	}

	return segments
}

func (c *SegmentChecker) filterExistedOnLeader(replica *meta.Replica, segments []*meta.Segment) []*meta.Segment {
	filtered := make([]*meta.Segment, 0, len(segments))
	for _, s := range segments {
		leaderID, ok := c.dist.ChannelDistManager.GetShardLeader(replica, s.GetInsertChannel())
		if !ok {
			continue
		}

		view := c.dist.LeaderViewManager.GetLeaderShardView(leaderID, s.GetInsertChannel())
		seg, ok := view.Segments[s.GetID()]
		if ok && seg.NodeID == s.Node {
			// if this segment is serving on leader, do not remove it for search available
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered
}

func (c *SegmentChecker) filterSegmentInUse(replica *meta.Replica, segments []*meta.Segment) []*meta.Segment {
	filtered := make([]*meta.Segment, 0, len(segments))
	for _, s := range segments {
		leaderID, ok := c.dist.ChannelDistManager.GetShardLeader(replica, s.GetInsertChannel())
		if !ok {
			continue
		}

		view := c.dist.LeaderViewManager.GetLeaderShardView(leaderID, s.GetInsertChannel())
		currentTargetVersion := c.targetMgr.GetCollectionTargetVersion(s.CollectionID, meta.CurrentTarget)
		partition := c.meta.CollectionManager.GetPartition(s.PartitionID)
		if partition != nil && view.TargetVersion != currentTargetVersion {
			// leader view version hasn't been updated, segment maybe still in use
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered
}

func (c *SegmentChecker) createSegmentLoadTasks(ctx context.Context, segments []*datapb.SegmentInfo, replica *meta.Replica) []task.Task {
	if len(segments) == 0 {
		return nil
	}
	packedSegments := make([]*meta.Segment, 0, len(segments))
	for _, s := range segments {
		if len(c.dist.LeaderViewManager.GetLeadersByShard(s.GetInsertChannel())) == 0 {
			continue
		}
		packedSegments = append(packedSegments, &meta.Segment{SegmentInfo: s})
	}
	outboundNodes := c.meta.ResourceManager.CheckOutboundNodes(replica)
	availableNodes := lo.Filter(replica.Replica.GetNodes(), func(node int64, _ int) bool {
		stop, err := c.nodeMgr.IsStoppingNode(node)
		if err != nil {
			return false
		}
		return !outboundNodes.Contain(node) && !stop
	})
	plans := c.balancer.AssignSegment(replica.CollectionID, packedSegments, availableNodes)
	for i := range plans {
		plans[i].ReplicaID = replica.GetID()
	}
	return balance.CreateSegmentTasksFromPlans(ctx, c.ID(), Params.QueryCoordCfg.SegmentTaskTimeout.GetAsDuration(time.Millisecond), plans)
}

func (c *SegmentChecker) createSegmentReduceTasks(ctx context.Context, segments []*meta.Segment, replicaID int64, scope querypb.DataScope) []task.Task {
	ret := make([]task.Task, 0, len(segments))
	for _, s := range segments {
		action := task.NewSegmentActionWithScope(s.Node, task.ActionTypeReduce, s.GetInsertChannel(), s.GetID(), scope)
		task, err := task.NewSegmentTask(
			ctx,
			Params.QueryCoordCfg.SegmentTaskTimeout.GetAsDuration(time.Millisecond),
			c.ID(),
			s.GetCollectionID(),
			replicaID,
			action,
		)

		if err != nil {
			log.Warn("create segment reduce task failed",
				zap.Int64("collection", s.GetCollectionID()),
				zap.Int64("replica", replicaID),
				zap.String("channel", s.GetInsertChannel()),
				zap.Int64("from", s.Node),
				zap.Error(err),
			)
			continue
		}

		ret = append(ret, task)
	}
	return ret
}
