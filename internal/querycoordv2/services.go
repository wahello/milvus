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

package querycoordv2

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/milvus-io/milvus-proto/go-api/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/milvuspb"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/metrics"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/querycoordv2/job"
	"github.com/milvus-io/milvus/internal/querycoordv2/meta"
	"github.com/milvus-io/milvus/internal/querycoordv2/utils"
	"github.com/milvus-io/milvus/internal/util/errorutil"
	"github.com/milvus-io/milvus/internal/util/metricsinfo"
	"github.com/milvus-io/milvus/internal/util/paramtable"
	"github.com/milvus-io/milvus/internal/util/timerecord"
	"github.com/milvus-io/milvus/internal/util/typeutil"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

var (
	successStatus = utils.WrapStatus(commonpb.ErrorCode_Success, "")
)

func (s *Server) ShowCollections(ctx context.Context, req *querypb.ShowCollectionsRequest) (*querypb.ShowCollectionsResponse, error) {
	log.Ctx(ctx).Info("show collections request received", zap.Int64s("collections", req.GetCollectionIDs()))

	if s.status.Load() != commonpb.StateCode_Healthy {
		msg := "failed to show collections"
		log.Warn(msg, zap.Error(ErrNotHealthy))
		return &querypb.ShowCollectionsResponse{
			Status: utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, ErrNotHealthy),
		}, nil
	}

	isGetAll := false
	collectionSet := typeutil.NewUniqueSet(req.GetCollectionIDs()...)
	if len(req.GetCollectionIDs()) == 0 {
		for _, collection := range s.meta.GetAllCollections() {
			collectionSet.Insert(collection.GetCollectionID())
		}
		for _, partition := range s.meta.GetAllPartitions() {
			collectionSet.Insert(partition.GetCollectionID())
		}
		isGetAll = true
	}
	collections := collectionSet.Collect()

	resp := &querypb.ShowCollectionsResponse{
		Status:                successStatus,
		CollectionIDs:         make([]int64, 0, len(collectionSet)),
		InMemoryPercentages:   make([]int64, 0, len(collectionSet)),
		QueryServiceAvailable: make([]bool, 0, len(collectionSet)),
	}
	for _, collectionID := range collections {
		log := log.With(zap.Int64("collectionID", collectionID))

		percentage := s.meta.CollectionManager.GetLoadPercentage(collectionID)
		if percentage < 0 {
			if isGetAll {
				// The collection is released during this,
				// ignore it
				continue
			}
			err := fmt.Errorf("collection %d has not been loaded to memory or load failed", collectionID)
			log.Warn("show collection failed", zap.Error(err))
			return &querypb.ShowCollectionsResponse{
				Status: utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, err.Error()),
			}, nil
		}
		resp.CollectionIDs = append(resp.CollectionIDs, collectionID)
		resp.InMemoryPercentages = append(resp.InMemoryPercentages, int64(percentage))
		resp.QueryServiceAvailable = append(resp.QueryServiceAvailable, s.checkAnyReplicaAvailable(collectionID))
	}

	return resp, nil
}

func (s *Server) ShowPartitions(ctx context.Context, req *querypb.ShowPartitionsRequest) (*querypb.ShowPartitionsResponse, error) {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", req.GetCollectionID()),
	)

	log.Info("show partitions request received", zap.Int64s("partitions", req.GetPartitionIDs()))

	if s.status.Load() != commonpb.StateCode_Healthy {
		msg := "failed to show partitions"
		log.Warn(msg, zap.Error(ErrNotHealthy))
		return &querypb.ShowPartitionsResponse{
			Status: utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, ErrNotHealthy),
		}, nil
	}

	// TODO(yah01): now, for load collection, the percentage of partition is equal to the percentage of collection,
	// we can calculates the real percentage of partitions
	partitions := req.GetPartitionIDs()
	percentages := make([]int64, 0)
	isReleased := false
	switch s.meta.GetLoadType(req.GetCollectionID()) {
	case querypb.LoadType_LoadCollection:
		percentage := s.meta.GetLoadPercentage(req.GetCollectionID())
		if percentage < 0 {
			isReleased = true
			break
		}

		if len(partitions) == 0 {
			var err error
			partitions, err = s.broker.GetPartitions(ctx, req.GetCollectionID())
			if err != nil {
				msg := "failed to show partitions"
				log.Warn(msg, zap.Error(err))
				return &querypb.ShowPartitionsResponse{
					Status: utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, err),
				}, nil
			}
		}
		for range partitions {
			percentages = append(percentages, int64(percentage))
		}

	case querypb.LoadType_LoadPartition:
		if len(partitions) == 0 {
			partitions = lo.Map(s.meta.GetPartitionsByCollection(req.GetCollectionID()), func(partition *meta.Partition, _ int) int64 {
				return partition.GetPartitionID()
			})
		}
		for _, partitionID := range partitions {
			partition := s.meta.GetPartition(partitionID)
			if partition == nil {
				isReleased = true
				break
			}
			percentages = append(percentages, int64(partition.LoadPercentage))
		}

	default:
		isReleased = true
	}

	if isReleased {
		msg := fmt.Sprintf("collection %v has not been loaded into QueryNode", req.GetCollectionID())
		log.Warn(msg)
		return &querypb.ShowPartitionsResponse{
			Status: utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg),
		}, nil
	}

	return &querypb.ShowPartitionsResponse{
		Status:              successStatus,
		PartitionIDs:        partitions,
		InMemoryPercentages: percentages,
	}, nil
}

func (s *Server) LoadCollection(ctx context.Context, req *querypb.LoadCollectionRequest) (*commonpb.Status, error) {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", req.GetCollectionID()),
	)

	log.Info("load collection request received",
		zap.Any("schema", req.Schema),
		zap.Int32("replicaNumber", req.ReplicaNumber),
		zap.Int64s("fieldIndexes", lo.Values(req.GetFieldIndexID())),
	)
	metrics.QueryCoordLoadCount.WithLabelValues(metrics.TotalLabel).Inc()

	if s.status.Load() != commonpb.StateCode_Healthy {
		msg := "failed to load collection"
		log.Warn(msg, zap.Error(ErrNotHealthy))
		metrics.QueryCoordLoadCount.WithLabelValues(metrics.FailLabel).Inc()
		return utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, ErrNotHealthy), nil
	}

	loadJob := job.NewLoadCollectionJob(ctx,
		req,
		s.dist,
		s.meta,
		s.targetMgr,
		s.broker,
		s.nodeMgr,
	)
	s.jobScheduler.Add(loadJob)
	err := loadJob.Wait()
	if err != nil && !errors.Is(err, job.ErrCollectionLoaded) {
		msg := "failed to load collection"
		log.Warn(msg, zap.Error(err))
		metrics.QueryCoordLoadCount.WithLabelValues(metrics.FailLabel).Inc()
		return utils.WrapStatus(errCode(err), msg, err), nil
	}

	metrics.QueryCoordLoadCount.WithLabelValues(metrics.SuccessLabel).Inc()
	return successStatus, nil
}

func (s *Server) ReleaseCollection(ctx context.Context, req *querypb.ReleaseCollectionRequest) (*commonpb.Status, error) {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", req.GetCollectionID()),
	)

	log.Info("release collection request received")
	metrics.QueryCoordReleaseCount.WithLabelValues(metrics.TotalLabel).Inc()
	tr := timerecord.NewTimeRecorder("release-collection")

	if s.status.Load() != commonpb.StateCode_Healthy {
		msg := "failed to release collection"
		log.Warn(msg, zap.Error(ErrNotHealthy))
		metrics.QueryCoordReleaseCount.WithLabelValues(metrics.FailLabel).Inc()
		return utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, ErrNotHealthy), nil
	}

	releaseJob := job.NewReleaseCollectionJob(ctx,
		req,
		s.dist,
		s.meta,
		s.targetMgr,
	)
	s.jobScheduler.Add(releaseJob)
	err := releaseJob.Wait()
	if err != nil {
		msg := "failed to release collection"
		log.Error(msg, zap.Error(err))
		metrics.QueryCoordReleaseCount.WithLabelValues(metrics.FailLabel).Inc()
		return utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, err), nil
	}

	log.Info("collection released")
	metrics.QueryCoordReleaseCount.WithLabelValues(metrics.SuccessLabel).Inc()
	metrics.QueryCoordReleaseLatency.WithLabelValues().Observe(float64(tr.ElapseSpan().Milliseconds()))
	return successStatus, nil
}

func (s *Server) LoadPartitions(ctx context.Context, req *querypb.LoadPartitionsRequest) (*commonpb.Status, error) {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", req.GetCollectionID()),
	)

	log.Info("received load partitions request",
		zap.Any("schema", req.Schema),
		zap.Int32("replicaNumber", req.ReplicaNumber),
		zap.Int64s("partitions", req.GetPartitionIDs()))
	metrics.QueryCoordLoadCount.WithLabelValues(metrics.TotalLabel).Inc()

	if s.status.Load() != commonpb.StateCode_Healthy {
		msg := "failed to load partitions"
		log.Warn(msg, zap.Error(ErrNotHealthy))
		metrics.QueryCoordLoadCount.WithLabelValues(metrics.FailLabel).Inc()
		return utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, ErrNotHealthy), nil
	}

	loadJob := job.NewLoadPartitionJob(ctx,
		req,
		s.dist,
		s.meta,
		s.targetMgr,
		s.broker,
		s.nodeMgr,
	)
	s.jobScheduler.Add(loadJob)
	err := loadJob.Wait()
	if err != nil && !errors.Is(err, job.ErrCollectionLoaded) {
		msg := "failed to load partitions"
		log.Warn(msg, zap.Error(err))
		metrics.QueryCoordLoadCount.WithLabelValues(metrics.FailLabel).Inc()
		return utils.WrapStatus(errCode(err), msg, err), nil
	}

	metrics.QueryCoordLoadCount.WithLabelValues(metrics.SuccessLabel).Inc()
	return successStatus, nil
}

func (s *Server) ReleasePartitions(ctx context.Context, req *querypb.ReleasePartitionsRequest) (*commonpb.Status, error) {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", req.GetCollectionID()),
	)

	log.Info("release partitions", zap.Int64s("partitions", req.GetPartitionIDs()))
	metrics.QueryCoordReleaseCount.WithLabelValues(metrics.TotalLabel).Inc()

	if s.status.Load() != commonpb.StateCode_Healthy {
		msg := "failed to release partitions"
		log.Warn(msg, zap.Error(ErrNotHealthy))
		metrics.QueryCoordReleaseCount.WithLabelValues(metrics.FailLabel).Inc()
		return utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, ErrNotHealthy), nil
	}

	if len(req.GetPartitionIDs()) == 0 {
		msg := "partitions is empty"
		log.Warn(msg)
		metrics.QueryCoordReleaseCount.WithLabelValues(metrics.FailLabel).Inc()
		return utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg), nil
	}

	tr := timerecord.NewTimeRecorder("release-partitions")
	releaseJob := job.NewReleasePartitionJob(ctx,
		req,
		s.dist,
		s.meta,
		s.targetMgr,
	)
	s.jobScheduler.Add(releaseJob)
	err := releaseJob.Wait()
	if err != nil {
		msg := "failed to release partitions"
		log.Error(msg, zap.Error(err))
		metrics.QueryCoordReleaseCount.WithLabelValues(metrics.FailLabel).Inc()
		return utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, err), nil
	}

	metrics.QueryCoordReleaseCount.WithLabelValues(metrics.SuccessLabel).Inc()
	metrics.QueryCoordReleaseLatency.WithLabelValues().Observe(float64(tr.ElapseSpan().Milliseconds()))
	return successStatus, nil
}

func (s *Server) GetPartitionStates(ctx context.Context, req *querypb.GetPartitionStatesRequest) (*querypb.GetPartitionStatesResponse, error) {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", req.GetCollectionID()),
	)

	log.Info("get partition states", zap.Int64s("partitions", req.GetPartitionIDs()))

	if s.status.Load() != commonpb.StateCode_Healthy {
		msg := "failed to get partition states"
		log.Warn(msg, zap.Error(ErrNotHealthy))
		return &querypb.GetPartitionStatesResponse{
			Status: utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, ErrNotHealthy),
		}, nil
	}

	msg := "partition not loaded"
	notLoadResp := &querypb.GetPartitionStatesResponse{
		Status: utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg),
	}

	states := make([]*querypb.PartitionStates, 0, len(req.GetPartitionIDs()))
	switch s.meta.GetLoadType(req.GetCollectionID()) {
	case querypb.LoadType_LoadCollection:
		collection := s.meta.GetCollection(req.GetCollectionID())
		state := querypb.PartitionState_PartialInMemory
		if collection.LoadPercentage >= 100 {
			state = querypb.PartitionState_InMemory
		}
		releasedPartitions := typeutil.NewUniqueSet(collection.GetReleasedPartitions()...)
		for _, partition := range req.GetPartitionIDs() {
			if releasedPartitions.Contain(partition) {
				log.Warn(msg)
				return notLoadResp, nil
			}
			states = append(states, &querypb.PartitionStates{
				PartitionID: partition,
				State:       state,
			})
		}

	case querypb.LoadType_LoadPartition:
		for _, partitionID := range req.GetPartitionIDs() {
			partition := s.meta.GetPartition(partitionID)
			if partition == nil {
				log.Warn(msg, zap.Int64("partition", partitionID))
				return notLoadResp, nil
			}
			state := querypb.PartitionState_PartialInMemory
			if partition.LoadPercentage >= 100 {
				state = querypb.PartitionState_InMemory
			}
			states = append(states, &querypb.PartitionStates{
				PartitionID: partitionID,
				State:       state,
			})
		}

	default:
		log.Warn(msg)
		return notLoadResp, nil
	}

	return &querypb.GetPartitionStatesResponse{
		Status:                successStatus,
		PartitionDescriptions: states,
	}, nil
}

func (s *Server) GetSegmentInfo(ctx context.Context, req *querypb.GetSegmentInfoRequest) (*querypb.GetSegmentInfoResponse, error) {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", req.GetCollectionID()),
	)

	log.Info("get segment info", zap.Int64s("segments", req.GetSegmentIDs()))

	if s.status.Load() != commonpb.StateCode_Healthy {
		msg := "failed to get segment info"
		log.Warn(msg, zap.Error(ErrNotHealthy))
		return &querypb.GetSegmentInfoResponse{
			Status: utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, ErrNotHealthy),
		}, nil
	}

	infos := make([]*querypb.SegmentInfo, 0, len(req.GetSegmentIDs()))
	if len(req.GetSegmentIDs()) == 0 {
		infos = s.getCollectionSegmentInfo(req.GetCollectionID())
	} else {
		for _, segmentID := range req.GetSegmentIDs() {
			segments := s.dist.SegmentDistManager.Get(segmentID)
			if len(segments) == 0 {
				msg := fmt.Sprintf("segment %v not found in any node", segmentID)
				log.Warn(msg, zap.Int64("segment", segmentID))
				return &querypb.GetSegmentInfoResponse{
					Status: utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg),
				}, nil
			}
			info := &querypb.SegmentInfo{}
			utils.MergeMetaSegmentIntoSegmentInfo(info, segments...)
			infos = append(infos, info)
		}
	}

	return &querypb.GetSegmentInfoResponse{
		Status: successStatus,
		Infos:  infos,
	}, nil
}

func (s *Server) LoadBalance(ctx context.Context, req *querypb.LoadBalanceRequest) (*commonpb.Status, error) {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", req.GetCollectionID()),
	)

	log.Info("load balance request received",
		zap.Int64s("source", req.GetSourceNodeIDs()),
		zap.Int64s("dest", req.GetDstNodeIDs()),
		zap.Int64s("segments", req.GetSealedSegmentIDs()))

	if s.status.Load() != commonpb.StateCode_Healthy {
		msg := "failed to load balance"
		log.Warn(msg, zap.Error(ErrNotHealthy))
		return utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, ErrNotHealthy), nil
	}

	// Verify request
	if len(req.GetSourceNodeIDs()) != 1 {
		msg := "source nodes can only contain 1 node"
		log.Warn(msg, zap.Int("source-nodes-num", len(req.GetSourceNodeIDs())))
		return utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg), nil
	}
	if s.meta.CollectionManager.GetLoadPercentage(req.GetCollectionID()) < 100 {
		msg := "can't balance segments of not fully loaded collection"
		log.Warn(msg)
		return utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg), nil
	}
	srcNode := req.GetSourceNodeIDs()[0]
	replica := s.meta.ReplicaManager.GetByCollectionAndNode(req.GetCollectionID(), srcNode)
	if replica == nil {
		msg := "source node not found in any replica"
		log.Warn(msg)
		return utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg), nil
	}
	for _, dstNode := range req.GetDstNodeIDs() {
		if !replica.Nodes.Contain(dstNode) {
			msg := "destination nodes have to be in the same replica of source node"
			log.Warn(msg)
			return utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg), nil
		}
	}

	err := s.balanceSegments(ctx, req, replica)
	if err != nil {
		msg := "failed to balance segments"
		log.Warn(msg, zap.Error(err))
		return utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, err), nil
	}
	return successStatus, nil
}

func (s *Server) ShowConfigurations(ctx context.Context, req *internalpb.ShowConfigurationsRequest) (*internalpb.ShowConfigurationsResponse, error) {
	log := log.Ctx(ctx)

	log.Info("show configurations request received", zap.String("pattern", req.GetPattern()))

	if s.status.Load() != commonpb.StateCode_Healthy {
		msg := "failed to show configurations"
		log.Warn(msg, zap.Error(ErrNotHealthy))
		return &internalpb.ShowConfigurationsResponse{
			Status: utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, ErrNotHealthy),
		}, nil
	}
	configList := make([]*commonpb.KeyValuePair, 0)
	for key, value := range Params.GetComponentConfigurations(ctx, "querycoord", req.Pattern) {
		configList = append(configList,
			&commonpb.KeyValuePair{
				Key:   key,
				Value: value,
			})
	}

	return &internalpb.ShowConfigurationsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
			Reason:    "",
		},
		Configuations: configList,
	}, nil
}

func (s *Server) GetMetrics(ctx context.Context, req *milvuspb.GetMetricsRequest) (*milvuspb.GetMetricsResponse, error) {
	log := log.Ctx(ctx)

	log.Debug("get metrics request received",
		zap.String("metricType", req.GetRequest()))

	if s.status.Load() != commonpb.StateCode_Healthy {
		msg := "failed to get metrics"
		log.Warn(msg, zap.Error(ErrNotHealthy))
		return &milvuspb.GetMetricsResponse{
			Status: utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, ErrNotHealthy),
		}, nil
	}

	resp := &milvuspb.GetMetricsResponse{
		Status: successStatus,
		ComponentName: metricsinfo.ConstructComponentName(typeutil.QueryCoordRole,
			paramtable.GetNodeID()),
	}

	metricType, err := metricsinfo.ParseMetricType(req.GetRequest())
	if err != nil {
		msg := "failed to parse metric type"
		log.Warn(msg, zap.Error(err))
		resp.Status = utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, err)
		return resp, nil
	}

	if metricType != metricsinfo.SystemInfoMetrics {
		msg := "invalid metric type"
		err := errors.New(metricsinfo.MsgUnimplementedMetric)
		log.Warn(msg, zap.Error(err))
		resp.Status = utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, err)
		return resp, nil
	}

	resp.Response, err = s.getSystemInfoMetrics(ctx, req)
	if err != nil {
		msg := "failed to get system info metrics"
		log.Warn(msg, zap.Error(err))
		resp.Status = utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, err)
		return resp, nil
	}

	return resp, nil
}

func (s *Server) GetReplicas(ctx context.Context, req *milvuspb.GetReplicasRequest) (*milvuspb.GetReplicasResponse, error) {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", req.GetCollectionID()),
	)

	log.Info("get replicas request received", zap.Bool("with-shard-nodes", req.GetWithShardNodes()))

	if s.status.Load() != commonpb.StateCode_Healthy {
		msg := "failed to get replicas"
		log.Warn(msg, zap.Error(ErrNotHealthy))
		return &milvuspb.GetReplicasResponse{
			Status: utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, ErrNotHealthy),
		}, nil
	}

	resp := &milvuspb.GetReplicasResponse{
		Status:   successStatus,
		Replicas: make([]*milvuspb.ReplicaInfo, 0),
	}

	replicas := s.meta.ReplicaManager.GetByCollection(req.GetCollectionID())
	if len(replicas) == 0 {
		msg := "failed to get replicas, collection not loaded"
		log.Warn(msg)
		resp.Status = utils.WrapStatus(commonpb.ErrorCode_MetaFailed, msg)
		return resp, nil
	}

	for _, replica := range replicas {
		info, err := s.fillReplicaInfo(replica, req.GetWithShardNodes())
		if err != nil {
			msg := "failed to get replica info"
			log.Warn(msg,
				zap.Int64("replica", replica.GetID()),
				zap.Error(err))
			resp.Status = utils.WrapStatus(commonpb.ErrorCode_MetaFailed, msg, err)
		}
		resp.Replicas = append(resp.Replicas, info)
	}
	return resp, nil
}

func (s *Server) GetShardLeaders(ctx context.Context, req *querypb.GetShardLeadersRequest) (*querypb.GetShardLeadersResponse, error) {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", req.GetCollectionID()),
	)

	log.Info("get shard leaders request received")
	if s.status.Load() != commonpb.StateCode_Healthy {
		msg := "failed to get shard leaders"
		log.Warn(msg, zap.Error(ErrNotHealthy))
		return &querypb.GetShardLeadersResponse{
			Status: utils.WrapStatus(commonpb.ErrorCode_UnexpectedError, msg, ErrNotHealthy),
		}, nil
	}

	resp := &querypb.GetShardLeadersResponse{
		Status: successStatus,
	}

	if s.meta.CollectionManager.GetLoadPercentage(req.GetCollectionID()) < 100 {
		msg := fmt.Sprintf("collection %v is not fully loaded", req.GetCollectionID())
		log.Warn(msg)
		resp.Status = utils.WrapStatus(commonpb.ErrorCode_NoReplicaAvailable, msg)
		return resp, nil
	}

	channels := s.targetMgr.GetDmChannelsByCollection(req.GetCollectionID(), meta.CurrentTarget)
	if len(channels) == 0 {
		msg := "failed to get channels"
		log.Warn(msg, zap.Error(meta.ErrCollectionNotFound))
		resp.Status = utils.WrapStatus(commonpb.ErrorCode_MetaFailed, msg, meta.ErrCollectionNotFound)
		return resp, nil
	}

	currentTargets := s.targetMgr.GetHistoricalSegmentsByCollection(req.GetCollectionID(), meta.CurrentTarget)
	for _, channel := range channels {
		log := log.With(zap.String("channel", channel.GetChannelName()))

		leaders := s.dist.LeaderViewManager.GetLeadersByShard(channel.GetChannelName())
		ids := make([]int64, 0, len(leaders))
		addrs := make([]string, 0, len(leaders))

		var channelErr error

		// In a replica, a shard is available, if and only if:
		// 1. The leader is online
		// 2. All QueryNodes in the distribution are online
		// 3. The last heartbeat response time is within HeartbeatAvailableInterval for all QueryNodes(include leader) in the distribution
		// 4. All segments of the shard in target should be in the distribution
		for _, leader := range leaders {
			log := log.With(zap.Int64("leaderID", leader.ID))
			info := s.nodeMgr.Get(leader.ID)

			// Check whether leader is online
			err := checkNodeAvailable(leader.ID, info)
			if err != nil {
				log.Info("leader is not available", zap.Error(err))
				multierr.AppendInto(&channelErr, fmt.Errorf("leader not available: %w", err))
				continue
			}
			// Check whether QueryNodes are online and available
			isAvailable := true
			for _, version := range leader.Segments {
				info := s.nodeMgr.Get(version.GetNodeID())
				err = checkNodeAvailable(version.GetNodeID(), info)
				if err != nil {
					log.Info("leader is not available due to QueryNode unavailable", zap.Error(err))
					isAvailable = false
					multierr.AppendInto(&channelErr, err)
					break
				}
			}

			// Avoid iterating all segments if any QueryNode unavailable
			if !isAvailable {
				continue
			}

			// Check whether segments are fully loaded
			for segmentID, info := range currentTargets {
				if info.GetInsertChannel() != leader.Channel {
					continue
				}

				_, exist := leader.Segments[segmentID]
				if !exist {
					log.Info("leader is not available due to lack of segment", zap.Int64("segmentID", segmentID))
					multierr.AppendInto(&channelErr, WrapErrLackSegment(segmentID))
					isAvailable = false
					break
				}
			}
			if !isAvailable {
				continue
			}

			ids = append(ids, info.ID())
			addrs = append(addrs, info.Addr())
		}

		if len(ids) == 0 {
			msg := fmt.Sprintf("channel %s is not available in any replica", channel.GetChannelName())
			log.Warn(msg, zap.Error(channelErr))
			resp.Status = utils.WrapStatus(commonpb.ErrorCode_NoReplicaAvailable, msg, channelErr)
			resp.Shards = nil
			return resp, nil
		}

		resp.Shards = append(resp.Shards, &querypb.ShardLeadersList{
			ChannelName: channel.GetChannelName(),
			NodeIds:     ids,
			NodeAddrs:   addrs,
		})
	}

	return resp, nil
}

func (s *Server) CheckHealth(ctx context.Context, req *milvuspb.CheckHealthRequest) (*milvuspb.CheckHealthResponse, error) {
	if s.status.Load() != commonpb.StateCode_Healthy {
		reason := errorutil.UnHealthReason("querycoord", s.session.ServerID, "querycoord is unhealthy")
		return &milvuspb.CheckHealthResponse{IsHealthy: false, Reasons: []string{reason}}, nil
	}

	group, ctx := errgroup.WithContext(ctx)
	errReasons := make([]string, 0, len(s.nodeMgr.GetAll()))

	mu := &sync.Mutex{}
	for _, node := range s.nodeMgr.GetAll() {
		node := node
		group.Go(func() error {
			resp, err := s.cluster.GetComponentStates(ctx, node.ID())
			isHealthy, reason := errorutil.UnHealthReasonWithComponentStatesOrErr("querynode", node.ID(), resp, err)
			if !isHealthy {
				mu.Lock()
				defer mu.Unlock()
				errReasons = append(errReasons, reason)
			}
			return err
		})
	}

	err := group.Wait()
	if err != nil || len(errReasons) != 0 {
		return &milvuspb.CheckHealthResponse{IsHealthy: false, Reasons: errReasons}, nil
	}

	return &milvuspb.CheckHealthResponse{IsHealthy: true, Reasons: errReasons}, nil
}
