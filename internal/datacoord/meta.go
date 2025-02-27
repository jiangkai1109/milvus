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

// Package datacoord contains core functions in datacoord
package datacoord

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/golang/protobuf/proto"
	"github.com/samber/lo"
	"go.uber.org/zap"
	"golang.org/x/exp/maps"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/msgpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/internal/metastore"
	"github.com/milvus-io/milvus/internal/metastore/model"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/util/segmentutil"
	"github.com/milvus-io/milvus/pkg/common"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/metrics"
	"github.com/milvus-io/milvus/pkg/util/lock"
	"github.com/milvus-io/milvus/pkg/util/merr"
	"github.com/milvus-io/milvus/pkg/util/metautil"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
	"github.com/milvus-io/milvus/pkg/util/timerecord"
	"github.com/milvus-io/milvus/pkg/util/tsoutil"
	"github.com/milvus-io/milvus/pkg/util/typeutil"
)

type meta struct {
	sync.RWMutex
	ctx            context.Context
	catalog        metastore.DataCoordCatalog
	collections    map[UniqueID]*collectionInfo // collection id to collection info
	segments       *SegmentsInfo                // segment id to segment info
	channelCPLocks *lock.KeyLock[string]
	channelCPs     *typeutil.ConcurrentMap[string, *msgpb.MsgPosition] // vChannel -> channel checkpoint/see position
	chunkManager   storage.ChunkManager

	// collectionIndexes records which indexes are on the collection
	// collID -> indexID -> index
	indexes map[UniqueID]map[UniqueID]*model.Index
	// buildID2Meta records the meta information of the segment
	// buildID -> segmentIndex
	buildID2SegmentIndex map[UniqueID]*model.SegmentIndex
}

// A local cache of segment metric update. Must call commit() to take effect.
type segMetricMutation struct {
	stateChange       map[string]map[string]int // segment state, seg level -> state change count (to increase or decrease).
	rowCountChange    int64                     // Change in # of rows.
	rowCountAccChange int64                     // Total # of historical added rows, accumulated.
}

type collectionInfo struct {
	ID             int64
	Schema         *schemapb.CollectionSchema
	Partitions     []int64
	StartPositions []*commonpb.KeyDataPair
	Properties     map[string]string
	CreatedAt      Timestamp
}

// NewMeta creates meta from provided `kv.TxnKV`
func newMeta(ctx context.Context, catalog metastore.DataCoordCatalog, chunkManager storage.ChunkManager) (*meta, error) {
	mt := &meta{
		ctx:                  ctx,
		catalog:              catalog,
		collections:          make(map[UniqueID]*collectionInfo),
		segments:             NewSegmentsInfo(),
		channelCPLocks:       lock.NewKeyLock[string](),
		channelCPs:           typeutil.NewConcurrentMap[string, *msgpb.MsgPosition](),
		chunkManager:         chunkManager,
		indexes:              make(map[UniqueID]map[UniqueID]*model.Index),
		buildID2SegmentIndex: make(map[UniqueID]*model.SegmentIndex),
	}
	err := mt.reloadFromKV()
	if err != nil {
		return nil, err
	}
	return mt, nil
}

// reloadFromKV loads meta from KV storage
func (m *meta) reloadFromKV() error {
	record := timerecord.NewTimeRecorder("datacoord")
	segments, err := m.catalog.ListSegments(m.ctx)
	if err != nil {
		return err
	}
	metrics.DataCoordNumCollections.WithLabelValues().Set(0)
	metrics.DataCoordNumSegments.Reset()
	numStoredRows := int64(0)
	for _, segment := range segments {
		// segments from catalog.ListSegments will not have logPath
		m.segments.SetSegment(segment.ID, NewSegmentInfo(segment))
		metrics.DataCoordNumSegments.WithLabelValues(segment.GetState().String(), segment.GetLevel().String()).Inc()
		if segment.State == commonpb.SegmentState_Flushed {
			numStoredRows += segment.NumOfRows

			insertFileNum := 0
			for _, fieldBinlog := range segment.GetBinlogs() {
				insertFileNum += len(fieldBinlog.GetBinlogs())
			}
			metrics.FlushedSegmentFileNum.WithLabelValues(metrics.InsertFileLabel).Observe(float64(insertFileNum))

			statFileNum := 0
			for _, fieldBinlog := range segment.GetStatslogs() {
				statFileNum += len(fieldBinlog.GetBinlogs())
			}
			metrics.FlushedSegmentFileNum.WithLabelValues(metrics.StatFileLabel).Observe(float64(statFileNum))

			deleteFileNum := 0
			for _, filedBinlog := range segment.GetDeltalogs() {
				deleteFileNum += len(filedBinlog.GetBinlogs())
			}
			metrics.FlushedSegmentFileNum.WithLabelValues(metrics.DeleteFileLabel).Observe(float64(deleteFileNum))
		}
	}
	metrics.DataCoordNumStoredRowsCounter.WithLabelValues().Add(float64(numStoredRows))

	channelCPs, err := m.catalog.ListChannelCheckpoint(m.ctx)
	if err != nil {
		return err
	}
	for vChannel, pos := range channelCPs {
		// for 2.2.2 issue https://github.com/milvus-io/milvus/issues/22181
		pos.ChannelName = vChannel
		m.channelCPs.Insert(vChannel, pos)
	}

	// load field indexes
	fieldIndexes, err := m.catalog.ListIndexes(m.ctx)
	if err != nil {
		log.Error("DataCoord meta reloadFromKV load field indexes fail", zap.Error(err))
		return err
	}
	for _, fieldIndex := range fieldIndexes {
		m.updateCollectionIndex(fieldIndex)
	}
	segmentIndexes, err := m.catalog.ListSegmentIndexes(m.ctx)
	if err != nil {
		log.Error("DataCoord meta reloadFromKV load segment indexes fail", zap.Error(err))
		return err
	}
	for _, segIdx := range segmentIndexes {
		m.updateSegmentIndex(segIdx)
		metrics.FlushedSegmentFileNum.WithLabelValues(metrics.IndexFileLabel).Observe(float64(len(segIdx.IndexFileKeys)))
	}
	log.Info("DataCoord meta reloadFromKV done", zap.Duration("duration", record.ElapseSpan()))
	return nil
}

// AddCollection adds a collection into meta
// Note that collection info is just for caching and will not be set into etcd from datacoord
func (m *meta) AddCollection(collection *collectionInfo) {
	log.Debug("meta update: add collection", zap.Int64("collectionID", collection.ID))
	m.Lock()
	defer m.Unlock()
	m.collections[collection.ID] = collection
	metrics.DataCoordNumCollections.WithLabelValues().Set(float64(len(m.collections)))
	log.Info("meta update: add collection - complete", zap.Int64("collectionID", collection.ID))
}

// GetCollection returns collection info with provided collection id from local cache
func (m *meta) GetCollection(collectionID UniqueID) *collectionInfo {
	m.RLock()
	defer m.RUnlock()
	collection, ok := m.collections[collectionID]
	if !ok {
		return nil
	}
	return collection
}

func (m *meta) GetClonedCollectionInfo(collectionID UniqueID) *collectionInfo {
	m.RLock()
	defer m.RUnlock()

	coll, ok := m.collections[collectionID]
	if !ok {
		return nil
	}

	clonedProperties := make(map[string]string)
	maps.Copy(clonedProperties, coll.Properties)
	cloneColl := &collectionInfo{
		ID:             coll.ID,
		Schema:         proto.Clone(coll.Schema).(*schemapb.CollectionSchema),
		Partitions:     coll.Partitions,
		StartPositions: common.CloneKeyDataPairs(coll.StartPositions),
		Properties:     clonedProperties,
	}

	return cloneColl
}

// GetSegmentsChanPart returns segments organized in Channel-Partition dimension with selector applied
func (m *meta) GetSegmentsChanPart(selector SegmentInfoSelector) []*chanPartSegments {
	m.RLock()
	defer m.RUnlock()
	mDimEntry := make(map[string]*chanPartSegments)

	for _, segmentInfo := range m.segments.segments {
		if !selector(segmentInfo) {
			continue
		}

		cloned := segmentInfo.Clone()

		dim := fmt.Sprintf("%d-%s", cloned.PartitionID, cloned.InsertChannel)
		entry, ok := mDimEntry[dim]
		if !ok {
			entry = &chanPartSegments{
				collectionID: cloned.CollectionID,
				partitionID:  cloned.PartitionID,
				channelName:  cloned.InsertChannel,
			}
			mDimEntry[dim] = entry
		}
		entry.segments = append(entry.segments, cloned)
	}

	result := make([]*chanPartSegments, 0, len(mDimEntry))
	for _, entry := range mDimEntry {
		result = append(result, entry)
	}
	return result
}

// GetNumRowsOfCollection returns total rows count of segments belongs to provided collection
func (m *meta) GetNumRowsOfCollection(collectionID UniqueID) int64 {
	m.RLock()
	defer m.RUnlock()
	var ret int64
	segments := m.segments.GetSegments()
	for _, segment := range segments {
		if isSegmentHealthy(segment) && segment.GetCollectionID() == collectionID {
			ret += segment.GetNumOfRows()
		}
	}
	return ret
}

// GetCollectionBinlogSize returns the total binlog size and binlog size of collections.
func (m *meta) GetCollectionBinlogSize() (int64, map[UniqueID]int64) {
	m.RLock()
	defer m.RUnlock()
	collectionBinlogSize := make(map[UniqueID]int64)
	collectionRowsNum := make(map[UniqueID]map[commonpb.SegmentState]int64)
	segments := m.segments.GetSegments()
	var total int64
	for _, segment := range segments {
		segmentSize := segment.getSegmentSize()
		if isSegmentHealthy(segment) {
			total += segmentSize
			collectionBinlogSize[segment.GetCollectionID()] += segmentSize
			metrics.DataCoordStoredBinlogSize.WithLabelValues(
				fmt.Sprint(segment.GetCollectionID()), fmt.Sprint(segment.GetID())).Set(float64(segmentSize))
			if _, ok := collectionRowsNum[segment.GetCollectionID()]; !ok {
				collectionRowsNum[segment.GetCollectionID()] = make(map[commonpb.SegmentState]int64)
			}
			collectionRowsNum[segment.GetCollectionID()][segment.GetState()] += segment.GetNumOfRows()
		}
	}
	for collection, statesRows := range collectionRowsNum {
		for state, rows := range statesRows {
			metrics.DataCoordNumStoredRows.WithLabelValues(fmt.Sprint(collection), state.String()).Set(float64(rows))
		}
	}
	return total, collectionBinlogSize
}

// AddSegment records segment info, persisting info into kv store
func (m *meta) AddSegment(ctx context.Context, segment *SegmentInfo) error {
	log := log.Ctx(ctx)
	log.Info("meta update: adding segment - Start", zap.Int64("segmentID", segment.GetID()))
	m.Lock()
	defer m.Unlock()
	if err := m.catalog.AddSegment(m.ctx, segment.SegmentInfo); err != nil {
		log.Error("meta update: adding segment failed",
			zap.Int64("segmentID", segment.GetID()),
			zap.Error(err))
		return err
	}
	m.segments.SetSegment(segment.GetID(), segment)

	metrics.DataCoordNumSegments.WithLabelValues(segment.GetState().String(), segment.GetLevel().String()).Inc()
	log.Info("meta update: adding segment - complete", zap.Int64("segmentID", segment.GetID()))
	return nil
}

// DropSegment remove segment with provided id, etcd persistence also removed
func (m *meta) DropSegment(segmentID UniqueID) error {
	log.Debug("meta update: dropping segment", zap.Int64("segmentID", segmentID))
	m.Lock()
	defer m.Unlock()
	segment := m.segments.GetSegment(segmentID)
	if segment == nil {
		log.Warn("meta update: dropping segment failed - segment not found",
			zap.Int64("segmentID", segmentID))
		return nil
	}
	if err := m.catalog.DropSegment(m.ctx, segment.SegmentInfo); err != nil {
		log.Warn("meta update: dropping segment failed",
			zap.Int64("segmentID", segmentID),
			zap.Error(err))
		return err
	}
	metrics.DataCoordNumSegments.WithLabelValues(segment.GetState().String(), segment.GetLevel().String()).Dec()
	m.segments.DropSegment(segmentID)
	log.Info("meta update: dropping segment - complete",
		zap.Int64("segmentID", segmentID))
	return nil
}

// GetHealthySegment returns segment info with provided id
// if not segment is found, nil will be returned
func (m *meta) GetHealthySegment(segID UniqueID) *SegmentInfo {
	m.RLock()
	defer m.RUnlock()
	segment := m.segments.GetSegment(segID)
	if segment != nil && isSegmentHealthy(segment) {
		return segment
	}
	return nil
}

// GetSegment returns segment info with provided id
// include the unhealthy segment
// if not segment is found, nil will be returned
func (m *meta) GetSegment(segID UniqueID) *SegmentInfo {
	m.RLock()
	defer m.RUnlock()
	return m.segments.GetSegment(segID)
}

// GetAllSegmentsUnsafe returns all segments
func (m *meta) GetAllSegmentsUnsafe() []*SegmentInfo {
	m.RLock()
	defer m.RUnlock()
	return m.segments.GetSegments()
}

// SetState setting segment with provided ID state
func (m *meta) SetState(segmentID UniqueID, targetState commonpb.SegmentState) error {
	log.Debug("meta update: setting segment state",
		zap.Int64("segmentID", segmentID),
		zap.Any("target state", targetState))
	m.Lock()
	defer m.Unlock()
	curSegInfo := m.segments.GetSegment(segmentID)
	if curSegInfo == nil {
		log.Warn("meta update: setting segment state - segment not found",
			zap.Int64("segmentID", segmentID),
			zap.Any("target state", targetState))
		// idempotent drop
		if targetState == commonpb.SegmentState_Dropped {
			return nil
		}
		return fmt.Errorf("segment is not exist with ID = %d", segmentID)
	}
	// Persist segment updates first.
	clonedSegment := curSegInfo.Clone()
	metricMutation := &segMetricMutation{
		stateChange: make(map[string]map[string]int),
	}
	if clonedSegment != nil && isSegmentHealthy(clonedSegment) {
		// Update segment state and prepare segment metric update.
		updateSegStateAndPrepareMetrics(clonedSegment, targetState, metricMutation)
		if err := m.catalog.AlterSegments(m.ctx, []*datapb.SegmentInfo{clonedSegment.SegmentInfo}); err != nil {
			log.Warn("meta update: setting segment state - failed to alter segments",
				zap.Int64("segmentID", segmentID),
				zap.String("target state", targetState.String()),
				zap.Error(err))
			return err
		}
		// Apply segment metric update after successful meta update.
		metricMutation.commit()
		// Update in-memory meta.
		m.segments.SetState(segmentID, targetState)
	}
	log.Info("meta update: setting segment state - complete",
		zap.Int64("segmentID", segmentID),
		zap.String("target state", targetState.String()))
	return nil
}

func (m *meta) UpdateSegment(segmentID int64, operators ...SegmentOperator) error {
	m.Lock()
	defer m.Unlock()
	info := m.segments.GetSegment(segmentID)
	if info == nil {
		log.Warn("meta update: UpdateSegment - segment not found",
			zap.Int64("segmentID", segmentID))

		return merr.WrapErrSegmentNotFound(segmentID)
	}
	// Persist segment updates first.
	cloned := info.Clone()

	var updated bool
	for _, operator := range operators {
		updated = updated || operator(cloned)
	}

	if !updated {
		log.Warn("meta update:UpdateSegmnt skipped, no update",
			zap.Int64("segmentID", segmentID),
		)
		return nil
	}

	if err := m.catalog.AlterSegments(m.ctx, []*datapb.SegmentInfo{cloned.SegmentInfo}); err != nil {
		log.Warn("meta update: update segment - failed to alter segments",
			zap.Int64("segmentID", segmentID),
			zap.Error(err))
		return err
	}
	// Update in-memory meta.
	m.segments.SetSegment(segmentID, cloned)

	log.Info("meta update: update segment - complete",
		zap.Int64("segmentID", segmentID))
	return nil
}

// UnsetIsImporting removes the `isImporting` flag of a segment.
func (m *meta) UnsetIsImporting(segmentID UniqueID) error {
	log.Debug("meta update: unsetting isImport state of segment",
		zap.Int64("segmentID", segmentID))
	m.Lock()
	defer m.Unlock()
	curSegInfo := m.segments.GetSegment(segmentID)
	if curSegInfo == nil {
		return fmt.Errorf("segment not found %d", segmentID)
	}
	// Persist segment updates first.
	clonedSegment := curSegInfo.Clone()
	clonedSegment.IsImporting = false
	if isSegmentHealthy(clonedSegment) {
		if err := m.catalog.AlterSegments(m.ctx, []*datapb.SegmentInfo{clonedSegment.SegmentInfo}); err != nil {
			log.Warn("meta update: unsetting isImport state of segment - failed to unset segment isImporting state",
				zap.Int64("segmentID", segmentID),
				zap.Error(err))
			return err
		}
	}
	// Update in-memory meta.
	m.segments.SetIsImporting(segmentID, false)
	log.Info("meta update: unsetting isImport state of segment - complete",
		zap.Int64("segmentID", segmentID))
	return nil
}

type updateSegmentPack struct {
	meta     *meta
	segments map[int64]*SegmentInfo
	// for update etcd binlog paths
	increments map[int64]metastore.BinlogsIncrement
	// for update segment metric after alter segments
	metricMutation *segMetricMutation
}

func (p *updateSegmentPack) Get(segmentID int64) *SegmentInfo {
	if segment, ok := p.segments[segmentID]; ok {
		return segment
	}

	segment := p.meta.segments.GetSegment(segmentID)
	if segment == nil || !isSegmentHealthy(segment) {
		log.Warn("meta update: get segment failed - segment not found",
			zap.Int64("segmentID", segmentID),
			zap.Bool("segment nil", segment == nil),
			zap.Bool("segment unhealthy", !isSegmentHealthy(segment)))
		return nil
	}

	p.segments[segmentID] = segment.Clone()
	return p.segments[segmentID]
}

type UpdateOperator func(*updateSegmentPack) bool

func CreateL0Operator(collectionID, partitionID, segmentID int64, channel string) UpdateOperator {
	return func(modPack *updateSegmentPack) bool {
		segment := modPack.meta.segments.GetSegment(segmentID)
		if segment == nil {
			log.Info("meta update: add new l0 segment",
				zap.Int64("collectionID", collectionID),
				zap.Int64("partitionID", partitionID),
				zap.Int64("segmentID", segmentID))

			modPack.segments[segmentID] = &SegmentInfo{
				SegmentInfo: &datapb.SegmentInfo{
					ID:            segmentID,
					CollectionID:  collectionID,
					PartitionID:   partitionID,
					InsertChannel: channel,
					NumOfRows:     0,
					State:         commonpb.SegmentState_Flushed,
					Level:         datapb.SegmentLevel_L0,
				},
			}
			modPack.metricMutation.addNewSeg(commonpb.SegmentState_Flushed, datapb.SegmentLevel_L0, 0)
		}
		return true
	}
}

func UpdateStorageVersionOperator(segmentID int64, version int64) UpdateOperator {
	return func(modPack *updateSegmentPack) bool {
		segment := modPack.Get(segmentID)
		if segment == nil {
			log.Info("meta update: update storage version - segment not found",
				zap.Int64("segmentID", segmentID))
			return false
		}

		segment.StorageVersion = version
		return true
	}
}

// Set status of segment
// and record dropped time when change segment status to dropped
func UpdateStatusOperator(segmentID int64, status commonpb.SegmentState) UpdateOperator {
	return func(modPack *updateSegmentPack) bool {
		segment := modPack.Get(segmentID)
		if segment == nil {
			log.Warn("meta update: update status failed - segment not found",
				zap.Int64("segmentID", segmentID),
				zap.String("status", status.String()))
			return false
		}

		updateSegStateAndPrepareMetrics(segment, status, modPack.metricMutation)
		if status == commonpb.SegmentState_Dropped {
			segment.DroppedAt = uint64(time.Now().UnixNano())
		}
		return true
	}
}

func UpdateCompactedOperator(segmentID int64) UpdateOperator {
	return func(modPack *updateSegmentPack) bool {
		segment := modPack.Get(segmentID)
		if segment == nil {
			log.Warn("meta update: update binlog failed - segment not found",
				zap.Int64("segmentID", segmentID))
			return false
		}
		segment.Compacted = true
		return true
	}
}

// update binlogs in segmentInfo
func UpdateBinlogsOperator(segmentID int64, binlogs, statslogs, deltalogs []*datapb.FieldBinlog) UpdateOperator {
	return func(modPack *updateSegmentPack) bool {
		segment := modPack.Get(segmentID)
		if segment == nil {
			log.Warn("meta update: update binlog failed - segment not found",
				zap.Int64("segmentID", segmentID))
			return false
		}

		segment.Binlogs = mergeFieldBinlogs(segment.GetBinlogs(), binlogs)
		segment.Statslogs = mergeFieldBinlogs(segment.GetStatslogs(), statslogs)
		segment.Deltalogs = mergeFieldBinlogs(segment.GetDeltalogs(), deltalogs)
		modPack.increments[segmentID] = metastore.BinlogsIncrement{
			Segment: segment.SegmentInfo,
		}
		return true
	}
}

// update startPosition
func UpdateStartPosition(startPositions []*datapb.SegmentStartPosition) UpdateOperator {
	return func(modPack *updateSegmentPack) bool {
		for _, pos := range startPositions {
			if len(pos.GetStartPosition().GetMsgID()) == 0 {
				continue
			}
			s := modPack.Get(pos.GetSegmentID())
			if s == nil {
				continue
			}

			s.StartPosition = pos.GetStartPosition()
		}
		return true
	}
}

// update segment checkpoint and num rows
// if was importing segment
// only update rows.
func UpdateCheckPointOperator(segmentID int64, importing bool, checkpoints []*datapb.CheckPoint) UpdateOperator {
	return func(modPack *updateSegmentPack) bool {
		segment := modPack.Get(segmentID)
		if segment == nil {
			log.Warn("meta update: update checkpoint failed - segment not found",
				zap.Int64("segmentID", segmentID))
			return false
		}

		if importing {
			segment.NumOfRows = segment.currRows
		} else {
			for _, cp := range checkpoints {
				if cp.SegmentID != segmentID {
					// Don't think this is gonna to happen, ignore for now.
					log.Warn("checkpoint in segment is not same as flush segment to update, igreo", zap.Int64("current", segmentID), zap.Int64("checkpoint segment", cp.SegmentID))
					continue
				}

				if segment.DmlPosition != nil && segment.DmlPosition.Timestamp >= cp.Position.Timestamp {
					log.Warn("checkpoint in segment is larger than reported", zap.Any("current", segment.GetDmlPosition()), zap.Any("reported", cp.GetPosition()))
					// segment position in etcd is larger than checkpoint, then dont change it
					continue
				}

				segment.NumOfRows = cp.NumOfRows
				segment.DmlPosition = cp.GetPosition()
			}
		}

		count := segmentutil.CalcRowCountFromBinLog(segment.SegmentInfo)
		if count != segment.currRows && count > 0 {
			log.Info("check point reported inconsistent with bin log row count",
				zap.Int64("current rows (wrong)", segment.currRows),
				zap.Int64("segment bin log row count (correct)", count))
			segment.NumOfRows = count
		}
		return true
	}
}

// updateSegmentsInfo update segment infos
// will exec all operators, and update all changed segments
func (m *meta) UpdateSegmentsInfo(operators ...UpdateOperator) error {
	m.Lock()
	defer m.Unlock()
	updatePack := &updateSegmentPack{
		meta:       m,
		segments:   make(map[int64]*SegmentInfo),
		increments: make(map[int64]metastore.BinlogsIncrement),
		metricMutation: &segMetricMutation{
			stateChange: make(map[string]map[string]int),
		},
	}

	for _, operator := range operators {
		ok := operator(updatePack)
		if !ok {
			return nil
		}
	}

	segments := lo.MapToSlice(updatePack.segments, func(_ int64, segment *SegmentInfo) *datapb.SegmentInfo { return segment.SegmentInfo })
	increments := lo.Values(updatePack.increments)

	if err := m.catalog.AlterSegments(m.ctx, segments, increments...); err != nil {
		log.Error("meta update: update flush segments info - failed to store flush segment info into Etcd",
			zap.Error(err))
		return err
	}
	// Apply metric mutation after a successful meta update.
	updatePack.metricMutation.commit()
	// update memory status
	for id, s := range updatePack.segments {
		m.segments.SetSegment(id, s)
	}
	log.Info("meta update: update flush segments info - update flush segments info successfully")
	return nil
}

// UpdateDropChannelSegmentInfo updates segment checkpoints and binlogs before drop
// reusing segment info to pass segment id, binlogs, statslog, deltalog, start position and checkpoint
func (m *meta) UpdateDropChannelSegmentInfo(channel string, segments []*SegmentInfo) error {
	log.Debug("meta update: update drop channel segment info",
		zap.String("channel", channel))
	m.Lock()
	defer m.Unlock()

	// Prepare segment metric mutation.
	metricMutation := &segMetricMutation{
		stateChange: make(map[string]map[string]int),
	}
	modSegments := make(map[UniqueID]*SegmentInfo)
	// save new segments flushed from buffer data
	for _, seg2Drop := range segments {
		var segment *SegmentInfo
		segment, metricMutation = m.mergeDropSegment(seg2Drop)
		if segment != nil {
			modSegments[seg2Drop.GetID()] = segment
		}
	}
	// set existed segments of channel to Dropped
	for _, seg := range m.segments.segments {
		if seg.InsertChannel != channel {
			continue
		}
		_, ok := modSegments[seg.ID]
		// seg inf mod segments are all in dropped state
		if !ok {
			clonedSeg := seg.Clone()
			updateSegStateAndPrepareMetrics(clonedSeg, commonpb.SegmentState_Dropped, metricMutation)
			modSegments[seg.ID] = clonedSeg
		}
	}
	err := m.batchSaveDropSegments(channel, modSegments)
	if err != nil {
		log.Warn("meta update: update drop channel segment info failed",
			zap.String("channel", channel),
			zap.Error(err))
	} else {
		log.Info("meta update: update drop channel segment info - complete",
			zap.String("channel", channel))
		// Apply segment metric mutation on successful meta update.
		metricMutation.commit()
	}
	return err
}

// mergeDropSegment merges drop segment information with meta segments
func (m *meta) mergeDropSegment(seg2Drop *SegmentInfo) (*SegmentInfo, *segMetricMutation) {
	metricMutation := &segMetricMutation{
		stateChange: make(map[string]map[string]int),
	}

	segment := m.segments.GetSegment(seg2Drop.ID)
	// healthy check makes sure the Idempotence
	if segment == nil || !isSegmentHealthy(segment) {
		log.Warn("UpdateDropChannel skipping nil or unhealthy", zap.Bool("is nil", segment == nil),
			zap.Bool("isHealthy", isSegmentHealthy(segment)))
		return nil, metricMutation
	}

	clonedSegment := segment.Clone()
	updateSegStateAndPrepareMetrics(clonedSegment, commonpb.SegmentState_Dropped, metricMutation)

	currBinlogs := clonedSegment.GetBinlogs()

	getFieldBinlogs := func(id UniqueID, binlogs []*datapb.FieldBinlog) *datapb.FieldBinlog {
		for _, binlog := range binlogs {
			if id == binlog.GetFieldID() {
				return binlog
			}
		}
		return nil
	}
	// binlogs
	for _, tBinlogs := range seg2Drop.GetBinlogs() {
		fieldBinlogs := getFieldBinlogs(tBinlogs.GetFieldID(), currBinlogs)
		if fieldBinlogs == nil {
			currBinlogs = append(currBinlogs, tBinlogs)
		} else {
			fieldBinlogs.Binlogs = append(fieldBinlogs.Binlogs, tBinlogs.Binlogs...)
		}
	}
	clonedSegment.Binlogs = currBinlogs
	// statlogs
	currStatsLogs := clonedSegment.GetStatslogs()
	for _, tStatsLogs := range seg2Drop.GetStatslogs() {
		fieldStatsLog := getFieldBinlogs(tStatsLogs.GetFieldID(), currStatsLogs)
		if fieldStatsLog == nil {
			currStatsLogs = append(currStatsLogs, tStatsLogs)
		} else {
			fieldStatsLog.Binlogs = append(fieldStatsLog.Binlogs, tStatsLogs.Binlogs...)
		}
	}
	clonedSegment.Statslogs = currStatsLogs
	// deltalogs
	clonedSegment.Deltalogs = append(clonedSegment.Deltalogs, seg2Drop.GetDeltalogs()...)

	// start position
	if seg2Drop.GetStartPosition() != nil {
		clonedSegment.StartPosition = seg2Drop.GetStartPosition()
	}
	// checkpoint
	if seg2Drop.GetDmlPosition() != nil {
		clonedSegment.DmlPosition = seg2Drop.GetDmlPosition()
	}
	clonedSegment.currRows = seg2Drop.currRows
	clonedSegment.NumOfRows = seg2Drop.currRows
	return clonedSegment, metricMutation
}

// batchSaveDropSegments saves drop segments info with channel removal flag
// since the channel unwatching operation is not atomic here
// ** the removal flag is always with last batch
// ** the last batch must contains at least one segment
//  1. when failure occurs between batches, failover mechanism will continue with the earliest  checkpoint of this channel
//     since the flag is not marked so DataNode can re-consume the drop collection msg
//  2. when failure occurs between save meta and unwatch channel, the removal flag shall be check before let datanode watch this channel
func (m *meta) batchSaveDropSegments(channel string, modSegments map[int64]*SegmentInfo) error {
	var modSegIDs []int64
	for k := range modSegments {
		modSegIDs = append(modSegIDs, k)
	}
	log.Info("meta update: batch save drop segments",
		zap.Int64s("drop segments", modSegIDs))
	segments := make([]*datapb.SegmentInfo, 0)
	for _, seg := range modSegments {
		segments = append(segments, seg.SegmentInfo)
	}
	err := m.catalog.SaveDroppedSegmentsInBatch(m.ctx, segments)
	if err != nil {
		return err
	}

	if err = m.catalog.MarkChannelDeleted(m.ctx, channel); err != nil {
		return err
	}

	// update memory info
	for id, segment := range modSegments {
		m.segments.SetSegment(id, segment)
	}

	return nil
}

// GetSegmentsByChannel returns all segment info which insert channel equals provided `dmlCh`
func (m *meta) GetSegmentsByChannel(channel string) []*SegmentInfo {
	return m.SelectSegments(func(segment *SegmentInfo) bool {
		return isSegmentHealthy(segment) && segment.InsertChannel == channel
	})
}

// GetSegmentsOfCollection get all segments of collection
func (m *meta) GetSegmentsOfCollection(collectionID UniqueID) []*SegmentInfo {
	return m.SelectSegments(func(segment *SegmentInfo) bool {
		return isSegmentHealthy(segment) && segment.GetCollectionID() == collectionID
	})
}

// GetSegmentsIDOfCollection returns all segment ids which collection equals to provided `collectionID`
func (m *meta) GetSegmentsIDOfCollection(collectionID UniqueID) []UniqueID {
	segments := m.SelectSegments(func(segment *SegmentInfo) bool {
		return isSegmentHealthy(segment) && segment.CollectionID == collectionID
	})

	return lo.Map(segments, func(segment *SegmentInfo, _ int) int64 {
		return segment.ID
	})
}

// GetSegmentsIDOfCollection returns all segment ids which collection equals to provided `collectionID`
func (m *meta) GetSegmentsIDOfCollectionWithDropped(collectionID UniqueID) []UniqueID {
	segments := m.SelectSegments(func(segment *SegmentInfo) bool {
		return segment != nil &&
			segment.GetState() != commonpb.SegmentState_SegmentStateNone &&
			segment.GetState() != commonpb.SegmentState_NotExist &&
			segment.CollectionID == collectionID
	})

	return lo.Map(segments, func(segment *SegmentInfo, _ int) int64 {
		return segment.ID
	})
}

// GetSegmentsIDOfPartition returns all segments ids which collection & partition equals to provided `collectionID`, `partitionID`
func (m *meta) GetSegmentsIDOfPartition(collectionID, partitionID UniqueID) []UniqueID {
	segments := m.SelectSegments(func(segment *SegmentInfo) bool {
		return isSegmentHealthy(segment) &&
			segment.CollectionID == collectionID &&
			segment.PartitionID == partitionID
	})

	return lo.Map(segments, func(segment *SegmentInfo, _ int) int64 {
		return segment.ID
	})
}

// GetSegmentsIDOfPartition returns all segments ids which collection & partition equals to provided `collectionID`, `partitionID`
func (m *meta) GetSegmentsIDOfPartitionWithDropped(collectionID, partitionID UniqueID) []UniqueID {
	segments := m.SelectSegments(func(segment *SegmentInfo) bool {
		return segment.GetState() != commonpb.SegmentState_SegmentStateNone &&
			segment.GetState() != commonpb.SegmentState_NotExist &&
			segment.CollectionID == collectionID &&
			segment.PartitionID == partitionID
	})

	return lo.Map(segments, func(segment *SegmentInfo, _ int) int64 {
		return segment.ID
	})
}

// GetNumRowsOfPartition returns row count of segments belongs to provided collection & partition
func (m *meta) GetNumRowsOfPartition(collectionID UniqueID, partitionID UniqueID) int64 {
	m.RLock()
	defer m.RUnlock()
	var ret int64
	segments := m.segments.GetSegments()
	for _, segment := range segments {
		if isSegmentHealthy(segment) && segment.CollectionID == collectionID && segment.PartitionID == partitionID {
			ret += segment.NumOfRows
		}
	}
	return ret
}

// GetUnFlushedSegments get all segments which state is not `Flushing` nor `Flushed`
func (m *meta) GetUnFlushedSegments() []*SegmentInfo {
	return m.SelectSegments(func(segment *SegmentInfo) bool {
		return segment.GetState() == commonpb.SegmentState_Growing || segment.GetState() == commonpb.SegmentState_Sealed
	})
}

// GetFlushingSegments get all segments which state is `Flushing`
func (m *meta) GetFlushingSegments() []*SegmentInfo {
	return m.SelectSegments(func(segment *SegmentInfo) bool {
		return segment.GetState() == commonpb.SegmentState_Flushing
	})
}

// SelectSegments select segments with selector
func (m *meta) SelectSegments(selector SegmentInfoSelector) []*SegmentInfo {
	m.RLock()
	defer m.RUnlock()
	var ret []*SegmentInfo
	segments := m.segments.GetSegments()
	for _, info := range segments {
		if selector(info) {
			ret = append(ret, info)
		}
	}
	return ret
}

// AddAllocation add allocation in segment
func (m *meta) AddAllocation(segmentID UniqueID, allocation *Allocation) error {
	log.Debug("meta update: add allocation",
		zap.Int64("segmentID", segmentID),
		zap.Any("allocation", allocation))
	m.Lock()
	defer m.Unlock()
	curSegInfo := m.segments.GetSegment(segmentID)
	if curSegInfo == nil {
		// TODO: Error handling.
		log.Error("meta update: add allocation failed - segment not found", zap.Int64("segmentID", segmentID))
		return errors.New("meta update: add allocation failed - segment not found")
	}
	// As we use global segment lastExpire to guarantee data correctness after restart
	// there is no need to persist allocation to meta store, only update allocation in-memory meta.
	m.segments.AddAllocation(segmentID, allocation)
	log.Info("meta update: add allocation - complete", zap.Int64("segmentID", segmentID))
	return nil
}

// SetAllocations set Segment allocations, will overwrite ALL original allocations
// Note that allocations is not persisted in KV store
func (m *meta) SetAllocations(segmentID UniqueID, allocations []*Allocation) {
	m.Lock()
	defer m.Unlock()
	m.segments.SetAllocations(segmentID, allocations)
}

// SetCurrentRows set current row count for segment with provided `segmentID`
// Note that currRows is not persisted in KV store
func (m *meta) SetCurrentRows(segmentID UniqueID, rows int64) {
	m.Lock()
	defer m.Unlock()
	m.segments.SetCurrentRows(segmentID, rows)
}

// SetLastExpire set lastExpire time for segment
// Note that last is not necessary to store in KV meta
func (m *meta) SetLastExpire(segmentID UniqueID, lastExpire uint64) {
	m.Lock()
	defer m.Unlock()
	clonedSegment := m.segments.GetSegment(segmentID).Clone()
	clonedSegment.LastExpireTime = lastExpire
	m.segments.SetSegment(segmentID, clonedSegment)
}

// SetLastFlushTime set LastFlushTime for segment with provided `segmentID`
// Note that lastFlushTime is not persisted in KV store
func (m *meta) SetLastFlushTime(segmentID UniqueID, t time.Time) {
	m.Lock()
	defer m.Unlock()
	m.segments.SetFlushTime(segmentID, t)
}

// SetSegmentCompacting sets compaction state for segment
func (m *meta) SetSegmentCompacting(segmentID UniqueID, compacting bool) {
	m.Lock()
	defer m.Unlock()

	m.segments.SetIsCompacting(segmentID, compacting)
}

func (m *meta) CompleteCompactionMutation(plan *datapb.CompactionPlan, result *datapb.CompactionPlanResult) ([]*SegmentInfo, *segMetricMutation, error) {
	m.Lock()
	defer m.Unlock()

	log := log.With(zap.Int64("planID", plan.GetPlanID()), zap.String("type", plan.GetType().String()))

	metricMutation := &segMetricMutation{stateChange: make(map[string]map[string]int)}
	var compactFromSegIDs []int64
	var latestCompactFromSegments []*SegmentInfo
	for _, segmentBinlogs := range plan.GetSegmentBinlogs() {
		segment := m.segments.GetSegment(segmentBinlogs.GetSegmentID())
		if segment == nil {
			return nil, nil, merr.WrapErrSegmentNotFound(segmentBinlogs.GetSegmentID())
		}

		cloned := segment.Clone()
		cloned.DroppedAt = uint64(time.Now().UnixNano())
		cloned.Compacted = true

		latestCompactFromSegments = append(latestCompactFromSegments, cloned)
		compactFromSegIDs = append(compactFromSegIDs, cloned.GetID())

		// metrics mutation for compaction from segments
		updateSegStateAndPrepareMetrics(cloned, commonpb.SegmentState_Dropped, metricMutation)
	}

	logIDsFromPlan := make(map[int64]struct{})
	for _, segBinlogs := range plan.GetSegmentBinlogs() {
		for _, fieldBinlog := range segBinlogs.GetDeltalogs() {
			for _, binlog := range fieldBinlog.GetBinlogs() {
				logIDsFromPlan[binlog.GetLogID()] = struct{}{}
			}
		}
	}

	// MixCompaction / MergeCompaction will generates one and only one segment
	compactToSegment := result.GetSegments()[0]

	// copy new deltalogs in compactFrom segments to compactTo segments.
	// TODO: Not needed when enable L0 segments.
	newDeltalogs, err := m.copyNewDeltalogs(latestCompactFromSegments, logIDsFromPlan, compactToSegment.GetSegmentID())
	if err != nil {
		return nil, nil, err
	}
	if len(newDeltalogs) > 0 {
		compactToSegment.Deltalogs = append(compactToSegment.GetDeltalogs(), &datapb.FieldBinlog{Binlogs: newDeltalogs})
	}

	getMinPosition := func(positions []*msgpb.MsgPosition) *msgpb.MsgPosition {
		var minPos *msgpb.MsgPosition
		for _, pos := range positions {
			if minPos == nil ||
				pos != nil && pos.GetTimestamp() < minPos.GetTimestamp() {
				minPos = pos
			}
		}
		return minPos
	}

	compactToSegmentInfo := NewSegmentInfo(
		&datapb.SegmentInfo{
			ID:            compactToSegment.GetSegmentID(),
			CollectionID:  latestCompactFromSegments[0].CollectionID,
			PartitionID:   latestCompactFromSegments[0].PartitionID,
			InsertChannel: plan.GetChannel(),
			NumOfRows:     compactToSegment.NumOfRows,
			State:         commonpb.SegmentState_Flushed,
			MaxRowNum:     latestCompactFromSegments[0].MaxRowNum,
			Binlogs:       compactToSegment.GetInsertLogs(),
			Statslogs:     compactToSegment.GetField2StatslogPaths(),
			Deltalogs:     compactToSegment.GetDeltalogs(),

			CreatedByCompaction: true,
			CompactionFrom:      compactFromSegIDs,
			LastExpireTime:      plan.GetStartTime(),
			Level:               datapb.SegmentLevel_L1,

			StartPosition: getMinPosition(lo.Map(latestCompactFromSegments, func(info *SegmentInfo, _ int) *msgpb.MsgPosition {
				return info.GetStartPosition()
			})),
			DmlPosition: getMinPosition(lo.Map(latestCompactFromSegments, func(info *SegmentInfo, _ int) *msgpb.MsgPosition {
				return info.GetDmlPosition()
			})),
		})

	// L1 segment with NumRows=0 will be discarded, so no need to change the metric
	if compactToSegmentInfo.GetNumOfRows() > 0 {
		// metrics mutation for compactTo segments
		metricMutation.addNewSeg(compactToSegmentInfo.GetState(), compactToSegmentInfo.GetLevel(), compactToSegmentInfo.GetNumOfRows())
	} else {
		compactToSegmentInfo.State = commonpb.SegmentState_Dropped
	}

	log = log.With(
		zap.String("channel", plan.GetChannel()),
		zap.Int64("partitionID", compactToSegmentInfo.GetPartitionID()),
		zap.Int64("compactTo segmentID", compactToSegmentInfo.GetID()),
		zap.Int64("compactTo segment numRows", compactToSegmentInfo.GetNumOfRows()),
		zap.Any("compactFrom segments(to be updated as dropped)", compactFromSegIDs),
	)

	log.Debug("meta update: prepare for meta mutation - complete")
	compactFromInfos := lo.Map(latestCompactFromSegments, func(info *SegmentInfo, _ int) *datapb.SegmentInfo {
		return info.SegmentInfo
	})

	log.Debug("meta update: alter meta store for compaction updates",
		zap.Int("binlog count", len(compactToSegmentInfo.GetBinlogs())),
		zap.Int("statslog count", len(compactToSegmentInfo.GetStatslogs())),
		zap.Int("deltalog count", len(compactToSegmentInfo.GetDeltalogs())),
	)
	if err := m.catalog.AlterSegments(m.ctx, append(compactFromInfos, compactToSegmentInfo.SegmentInfo),
		metastore.BinlogsIncrement{Segment: compactToSegmentInfo.SegmentInfo},
	); err != nil {
		log.Warn("fail to alter segments and new segment", zap.Error(err))
		return nil, nil, err
	}

	lo.ForEach(latestCompactFromSegments, func(info *SegmentInfo, _ int) {
		m.segments.SetSegment(info.GetID(), info)
	})
	m.segments.SetSegment(compactToSegmentInfo.GetID(), compactToSegmentInfo)

	log.Info("meta update: alter in memory meta after compaction - complete")
	return []*SegmentInfo{compactToSegmentInfo}, metricMutation, nil
}

func (m *meta) copyNewDeltalogs(latestCompactFromInfos []*SegmentInfo, logIDsInPlan map[int64]struct{}, toSegment int64) ([]*datapb.Binlog, error) {
	newBinlogs := []*datapb.Binlog{}
	for _, seg := range latestCompactFromInfos {
		for _, fieldLog := range seg.GetDeltalogs() {
			for _, l := range fieldLog.GetBinlogs() {
				if _, ok := logIDsInPlan[l.GetLogID()]; !ok {
					fromKey := metautil.BuildDeltaLogPath(m.chunkManager.RootPath(), seg.CollectionID, seg.PartitionID, seg.ID, l.GetLogID())
					toKey := metautil.BuildDeltaLogPath(m.chunkManager.RootPath(), seg.CollectionID, seg.PartitionID, toSegment, l.GetLogID())
					log.Warn("found new deltalog in compactFrom segment, copying it...",
						zap.Any("deltalog", l),
						zap.Int64("copyFrom segmentID", seg.GetID()),
						zap.Int64("copyTo segmentID", toSegment),
						zap.String("copyFrom key", fromKey),
						zap.String("copyTo key", toKey),
					)

					blob, err := m.chunkManager.Read(m.ctx, fromKey)
					if err != nil {
						return nil, err
					}

					if err := m.chunkManager.Write(m.ctx, toKey, blob); err != nil {
						return nil, err
					}
					newBinlogs = append(newBinlogs, l)
				}
			}
		}
	}
	return newBinlogs, nil
}

// buildSegment utility function for compose datapb.SegmentInfo struct with provided info
func buildSegment(collectionID UniqueID, partitionID UniqueID, segmentID UniqueID, channelName string, isImporting bool) *SegmentInfo {
	info := &datapb.SegmentInfo{
		ID:            segmentID,
		CollectionID:  collectionID,
		PartitionID:   partitionID,
		InsertChannel: channelName,
		NumOfRows:     0,
		State:         commonpb.SegmentState_Growing,
		IsImporting:   isImporting,
	}
	return NewSegmentInfo(info)
}

func isSegmentHealthy(segment *SegmentInfo) bool {
	return segment != nil &&
		segment.GetState() != commonpb.SegmentState_SegmentStateNone &&
		segment.GetState() != commonpb.SegmentState_NotExist &&
		segment.GetState() != commonpb.SegmentState_Dropped
}

func (m *meta) HasSegments(segIDs []UniqueID) (bool, error) {
	m.RLock()
	defer m.RUnlock()

	for _, segID := range segIDs {
		if _, ok := m.segments.segments[segID]; !ok {
			return false, fmt.Errorf("segment is not exist with ID = %d", segID)
		}
	}
	return true, nil
}

func (m *meta) GetCompactionTo(segmentID int64) *SegmentInfo {
	m.RLock()
	defer m.RUnlock()

	segments := m.segments.GetSegments()
	for _, segment := range segments {
		parents := typeutil.NewUniqueSet(segment.GetCompactionFrom()...)
		if parents.Contain(segmentID) {
			return segment
		}
	}
	return nil
}

// UpdateChannelCheckpoint updates and saves channel checkpoint.
func (m *meta) UpdateChannelCheckpoint(vChannel string, pos *msgpb.MsgPosition) error {
	if pos == nil || pos.GetMsgID() == nil {
		return fmt.Errorf("channelCP is nil, vChannel=%s", vChannel)
	}

	m.channelCPLocks.Lock(vChannel)
	defer m.channelCPLocks.Unlock(vChannel)

	oldPosition, ok := m.channelCPs.Get(vChannel)
	if !ok || oldPosition.Timestamp < pos.Timestamp {
		err := m.catalog.SaveChannelCheckpoint(m.ctx, vChannel, pos)
		if err != nil {
			return err
		}
		m.channelCPs.Insert(vChannel, pos)
		ts, _ := tsoutil.ParseTS(pos.Timestamp)
		log.Info("UpdateChannelCheckpoint done",
			zap.String("vChannel", vChannel),
			zap.Uint64("ts", pos.GetTimestamp()),
			zap.ByteString("msgID", pos.GetMsgID()),
			zap.Time("time", ts))
		metrics.DataCoordCheckpointUnixSeconds.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), vChannel).
			Set(float64(ts.Unix()))
	}
	return nil
}

func (m *meta) GetChannelCheckpoint(vChannel string) *msgpb.MsgPosition {
	m.channelCPLocks.Lock(vChannel)
	defer m.channelCPLocks.Unlock(vChannel)
	v, ok := m.channelCPs.Get(vChannel)
	if !ok {
		return nil
	}
	return proto.Clone(v).(*msgpb.MsgPosition)
}

func (m *meta) DropChannelCheckpoint(vChannel string) error {
	m.channelCPLocks.Lock(vChannel)
	defer m.channelCPLocks.Unlock(vChannel)
	err := m.catalog.DropChannelCheckpoint(m.ctx, vChannel)
	if err != nil {
		return err
	}
	m.channelCPs.Remove(vChannel)
	log.Debug("DropChannelCheckpoint done", zap.String("vChannel", vChannel))
	return nil
}

func (m *meta) GcConfirm(ctx context.Context, collectionID, partitionID UniqueID) bool {
	return m.catalog.GcConfirm(ctx, collectionID, partitionID)
}

func (m *meta) GetCompactableSegmentGroupByCollection() map[int64][]*SegmentInfo {
	allSegs := m.SelectSegments(func(segment *SegmentInfo) bool {
		return isSegmentHealthy(segment) &&
			isFlush(segment) && // sealed segment
			!segment.isCompacting && // not compacting now
			!segment.GetIsImporting() // not importing now
	})

	ret := make(map[int64][]*SegmentInfo)
	for _, seg := range allSegs {
		if _, ok := ret[seg.CollectionID]; !ok {
			ret[seg.CollectionID] = make([]*SegmentInfo, 0)
		}

		ret[seg.CollectionID] = append(ret[seg.CollectionID], seg)
	}

	return ret
}

func (m *meta) GetEarliestStartPositionOfGrowingSegments(label *CompactionGroupLabel) *msgpb.MsgPosition {
	segments := m.SelectSegments(func(segment *SegmentInfo) bool {
		return segment.GetState() == commonpb.SegmentState_Growing &&
			segment.GetCollectionID() == label.CollectionID &&
			segment.GetPartitionID() == label.PartitionID &&
			segment.GetInsertChannel() == label.Channel
	})

	earliest := &msgpb.MsgPosition{Timestamp: math.MaxUint64}
	for _, seg := range segments {
		if earliest == nil || earliest.GetTimestamp() > seg.GetStartPosition().GetTimestamp() {
			earliest = seg.GetStartPosition()
		}
	}
	return earliest
}

// addNewSeg update metrics update for a new segment.
func (s *segMetricMutation) addNewSeg(state commonpb.SegmentState, level datapb.SegmentLevel, rowCount int64) {
	if _, ok := s.stateChange[level.String()]; !ok {
		s.stateChange[level.String()] = make(map[string]int)
	}
	s.stateChange[level.String()][state.String()] += 1

	s.rowCountChange += rowCount
	s.rowCountAccChange += rowCount
}

// commit persists all updates in current segMetricMutation, should and must be called AFTER segment state change
// has persisted in Etcd.
func (s *segMetricMutation) commit() {
	for level, submap := range s.stateChange {
		for state, change := range submap {
			metrics.DataCoordNumSegments.WithLabelValues(state, level).Add(float64(change))
		}
	}
	metrics.DataCoordNumStoredRowsCounter.WithLabelValues().Add(float64(s.rowCountAccChange))
}

// append updates current segMetricMutation when segment state change happens.
func (s *segMetricMutation) append(oldState, newState commonpb.SegmentState, level datapb.SegmentLevel, rowCountUpdate int64) {
	if oldState != newState {
		if _, ok := s.stateChange[level.String()]; !ok {
			s.stateChange[level.String()] = make(map[string]int)
		}
		s.stateChange[level.String()][oldState.String()] -= 1
		s.stateChange[level.String()][newState.String()] += 1
	}
	// Update # of rows on new flush operations and drop operations.
	if isFlushState(newState) && !isFlushState(oldState) {
		// If new flush.
		s.rowCountChange += rowCountUpdate
		s.rowCountAccChange += rowCountUpdate
	} else if newState == commonpb.SegmentState_Dropped && oldState != newState {
		// If new drop.
		s.rowCountChange -= rowCountUpdate
	}
}

func isFlushState(state commonpb.SegmentState) bool {
	return state == commonpb.SegmentState_Flushing || state == commonpb.SegmentState_Flushed
}

// updateSegStateAndPrepareMetrics updates a segment's in-memory state and prepare for the corresponding metric update.
func updateSegStateAndPrepareMetrics(segToUpdate *SegmentInfo, targetState commonpb.SegmentState, metricMutation *segMetricMutation) {
	log.Debug("updating segment state and updating metrics",
		zap.Int64("segmentID", segToUpdate.GetID()),
		zap.String("old state", segToUpdate.GetState().String()),
		zap.String("new state", targetState.String()),
		zap.Int64("# of rows", segToUpdate.GetNumOfRows()))
	metricMutation.append(segToUpdate.GetState(), targetState, segToUpdate.GetLevel(), segToUpdate.GetNumOfRows())
	segToUpdate.State = targetState
}
