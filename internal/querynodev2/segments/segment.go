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

package segments

/*
#cgo pkg-config: milvus_segcore

#include "segcore/collection_c.h"
#include "segcore/plan_c.h"
#include "segcore/reduce_c.h"
*/
import "C"

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/apache/arrow/go/v12/arrow/array"
	"github.com/cockroachdb/errors"
	"github.com/golang/protobuf/proto"
	"go.opentelemetry.io/otel"
	"go.uber.org/atomic"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/msgpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	milvus_storage "github.com/milvus-io/milvus-storage/go/storage"
	"github.com/milvus-io/milvus-storage/go/storage/options"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/proto/segcorepb"
	"github.com/milvus-io/milvus/internal/querynodev2/pkoracle"
	"github.com/milvus-io/milvus/internal/storage"
	typeutil_internal "github.com/milvus-io/milvus/internal/util/typeutil"
	"github.com/milvus-io/milvus/pkg/common"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/metrics"
	"github.com/milvus-io/milvus/pkg/util/merr"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
	"github.com/milvus-io/milvus/pkg/util/timerecord"
	"github.com/milvus-io/milvus/pkg/util/typeutil"
)

type SegmentType = commonpb.SegmentState

const (
	SegmentTypeGrowing = commonpb.SegmentState_Growing
	SegmentTypeSealed  = commonpb.SegmentState_Sealed
)

var ErrSegmentUnhealthy = errors.New("segment unhealthy")

// IndexedFieldInfo contains binlog info of vector field
type IndexedFieldInfo struct {
	FieldBinlog *datapb.FieldBinlog
	IndexInfo   *querypb.FieldIndexInfo
}

type baseSegment struct {
	segmentID    int64
	partitionID  int64
	shard        string
	collectionID int64
	typ          SegmentType
	level        datapb.SegmentLevel
	version      *atomic.Int64

	// the load status of the segment,
	// only transitions below are allowed:
	// 1. LoadStatusMeta <-> LoadStatusMapped
	// 2. LoadStatusMeta <-> LoadStatusInMemory
	loadStatus     *atomic.String
	isLazyLoad     bool
	startPosition  *msgpb.MsgPosition // for growing segment release
	bloomFilterSet *pkoracle.BloomFilterSet
}

func newBaseSegment(id, partitionID, collectionID int64, shard string, typ SegmentType, level datapb.SegmentLevel, version int64, startPosition *msgpb.MsgPosition) baseSegment {
	return baseSegment{
		segmentID:      id,
		partitionID:    partitionID,
		collectionID:   collectionID,
		shard:          shard,
		typ:            typ,
		level:          level,
		version:        atomic.NewInt64(version),
		loadStatus:     atomic.NewString(string(LoadStatusMeta)),
		startPosition:  startPosition,
		bloomFilterSet: pkoracle.NewBloomFilterSet(id, partitionID, typ),
	}
}

// ID returns the identity number.
func (s *baseSegment) ID() int64 {
	return s.segmentID
}

func (s *baseSegment) Collection() int64 {
	return s.collectionID
}

func (s *baseSegment) Partition() int64 {
	return s.partitionID
}

func (s *baseSegment) Shard() string {
	return s.shard
}

func (s *baseSegment) Type() SegmentType {
	return s.typ
}

func (s *baseSegment) Level() datapb.SegmentLevel {
	return s.level
}

func (s *baseSegment) StartPosition() *msgpb.MsgPosition {
	return s.startPosition
}

func (s *baseSegment) Version() int64 {
	return s.version.Load()
}

func (s *baseSegment) CASVersion(old, newVersion int64) bool {
	return s.version.CompareAndSwap(old, newVersion)
}

func (s *baseSegment) LoadStatus() LoadStatus {
	return LoadStatus(s.loadStatus.Load())
}

func (s *baseSegment) IsLazyLoad() bool {
	return s.isLazyLoad
}

func (s *baseSegment) LoadInfo() *querypb.SegmentLoadInfo {
	// TODO
	return nil
}

func (s *baseSegment) UpdateBloomFilter(pks []storage.PrimaryKey) {
	s.bloomFilterSet.UpdateBloomFilter(pks)
}

// MayPkExist returns true if the given PK exists in the PK range and being positive through the bloom filter,
// false otherwise,
// may returns true even the PK doesn't exist actually
func (s *baseSegment) MayPkExist(pk storage.PrimaryKey) bool {
	return s.bloomFilterSet.MayPkExist(pk)
}

type FieldInfo struct {
	datapb.FieldBinlog
	RowCount int64
}

var _ Segment = (*LocalSegment)(nil)

// Segment is a wrapper of the underlying C-structure segment.
type LocalSegment struct {
	baseSegment
	ptrLock sync.RWMutex // protects segmentPtr
	ptr     C.CSegmentInterface

	// cached results, to avoid too many CGO calls
	memSize     *atomic.Int64
	rowNum      *atomic.Int64
	insertCount *atomic.Int64

	lastDeltaTimestamp *atomic.Uint64
	fields             *typeutil.ConcurrentMap[int64, *FieldInfo]
	fieldIndexes       *typeutil.ConcurrentMap[int64, *IndexedFieldInfo]
	space              *milvus_storage.Space
}

func NewSegment(ctx context.Context,
	collection *Collection,
	segmentID int64,
	partitionID int64,
	collectionID int64,
	shard string,
	segmentType SegmentType,
	version int64,
	startPosition *msgpb.MsgPosition,
	deltaPosition *msgpb.MsgPosition,
	level datapb.SegmentLevel,
) (Segment, error) {
	log := log.Ctx(ctx)
	/*
		CStatus
		NewSegment(CCollection collection, uint64_t segment_id, SegmentType seg_type, CSegmentInterface* newSegment);
	*/
	if level == datapb.SegmentLevel_L0 {
		return NewL0Segment(collection, segmentID, partitionID, collectionID, shard, segmentType, version, startPosition, deltaPosition)
	}
	var cSegType C.SegmentType
	switch segmentType {
	case SegmentTypeSealed:
		cSegType = C.Sealed
	case SegmentTypeGrowing:
		cSegType = C.Growing
	default:
		return nil, fmt.Errorf("illegal segment type %d when create segment %d", segmentType, segmentID)
	}

	var newPtr C.CSegmentInterface
	_, err := GetDynamicPool().Submit(func() (any, error) {
		status := C.NewSegment(collection.collectionPtr, cSegType, C.int64_t(segmentID), &newPtr)
		err := HandleCStatus(ctx, &status, "NewSegmentFailed",
			zap.Int64("collectionID", collectionID),
			zap.Int64("partitionID", partitionID),
			zap.Int64("segmentID", segmentID),
			zap.String("segmentType", segmentType.String()))
		return nil, err
	}).Await()
	if err != nil {
		return nil, err
	}

	log.Info("create segment",
		zap.Int64("collectionID", collectionID),
		zap.Int64("partitionID", partitionID),
		zap.Int64("segmentID", segmentID),
		zap.String("segmentType", segmentType.String()),
		zap.String("level", level.String()),
	)

	segment := &LocalSegment{
		baseSegment:        newBaseSegment(segmentID, partitionID, collectionID, shard, segmentType, level, version, startPosition),
		ptr:                newPtr,
		lastDeltaTimestamp: atomic.NewUint64(0),
		fields:             typeutil.NewConcurrentMap[int64, *FieldInfo](),
		fieldIndexes:       typeutil.NewConcurrentMap[int64, *IndexedFieldInfo](),

		memSize:     atomic.NewInt64(-1),
		rowNum:      atomic.NewInt64(-1),
		insertCount: atomic.NewInt64(0),
	}

	if segmentType != SegmentTypeSealed {
		segment.loadStatus.Store(string(LoadStatusInMemory))
	}

	return segment, nil
}

func NewSegmentV2(
	ctx context.Context,
	collection *Collection,
	segmentID int64,
	partitionID int64,
	collectionID int64,
	shard string,
	segmentType SegmentType,
	version int64,
	startPosition *msgpb.MsgPosition,
	deltaPosition *msgpb.MsgPosition,
	storageVersion int64,
	level datapb.SegmentLevel,
) (Segment, error) {
	/*
		CSegmentInterface
		NewSegment(CCollection collection, uint64_t segment_id, SegmentType seg_type);
	*/
	if level == datapb.SegmentLevel_L0 {
		return NewL0Segment(collection, segmentID, partitionID, collectionID, shard, segmentType, version, startPosition, deltaPosition)
	}
	var segmentPtr C.CSegmentInterface
	var status C.CStatus
	switch segmentType {
	case SegmentTypeSealed:
		status = C.NewSegment(collection.collectionPtr, C.Sealed, C.int64_t(segmentID), &segmentPtr)
	case SegmentTypeGrowing:
		status = C.NewSegment(collection.collectionPtr, C.Growing, C.int64_t(segmentID), &segmentPtr)
	default:
		return nil, fmt.Errorf("illegal segment type %d when create segment %d", segmentType, segmentID)
	}

	if err := HandleCStatus(ctx, &status, "NewSegmentFailed"); err != nil {
		return nil, err
	}

	log.Info("create segment",
		zap.Int64("collectionID", collectionID),
		zap.Int64("partitionID", partitionID),
		zap.Int64("segmentID", segmentID),
		zap.String("segmentType", segmentType.String()))

	url, err := typeutil_internal.GetStorageURI(paramtable.Get().CommonCfg.StorageScheme.GetValue(), paramtable.Get().CommonCfg.StoragePathPrefix.GetValue(), segmentID)
	if err != nil {
		return nil, err
	}
	space, err := milvus_storage.Open(url, options.NewSpaceOptionBuilder().SetVersion(storageVersion).Build())
	if err != nil {
		return nil, err
	}

	segment := &LocalSegment{
		baseSegment:        newBaseSegment(segmentID, partitionID, collectionID, shard, segmentType, level, version, startPosition),
		ptr:                segmentPtr,
		lastDeltaTimestamp: atomic.NewUint64(0),
		fields:             typeutil.NewConcurrentMap[int64, *FieldInfo](),
		fieldIndexes:       typeutil.NewConcurrentMap[int64, *IndexedFieldInfo](),
		space:              space,
		memSize:            atomic.NewInt64(-1),
		rowNum:             atomic.NewInt64(-1),
		insertCount:        atomic.NewInt64(0),
	}

	if segmentType != SegmentTypeSealed {
		segment.loadStatus.Store(string(LoadStatusInMemory))
	}

	return segment, nil
}

func (s *LocalSegment) isValid() bool {
	return s.ptr != nil
}

// RLock acquires the `ptrLock` and returns true if the pointer is valid
// Provide ONLY the read lock operations,
// don't make `ptrLock` public to avoid abusing of the mutex.
func (s *LocalSegment) RLock() error {
	s.ptrLock.RLock()
	if !s.isValid() {
		s.ptrLock.RUnlock()
		return merr.WrapErrSegmentNotLoaded(s.ID(), "segment released")
	}
	return nil
}

func (s *LocalSegment) RUnlock() {
	s.ptrLock.RUnlock()
}

func (s *LocalSegment) InsertCount() int64 {
	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()

	return s.insertCount.Load()
}

func (s *LocalSegment) RowNum() int64 {
	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()

	if !s.isValid() {
		log.Warn("segment is not valid", zap.Int64("segmentID", s.ID()))
		return 0
	}

	rowNum := s.rowNum.Load()
	if rowNum < 0 {
		var rowCount C.int64_t
		GetDynamicPool().Submit(func() (any, error) {
			rowCount = C.GetRealCount(s.ptr)
			s.rowNum.Store(int64(rowCount))
			return nil, nil
		}).Await()
		rowNum = int64(rowCount)
	}

	return rowNum
}

func (s *LocalSegment) MemSize() int64 {
	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()

	if !s.isValid() {
		return 0
	}

	memSize := s.memSize.Load()
	if memSize < 0 {
		var cMemSize C.int64_t
		GetDynamicPool().Submit(func() (any, error) {
			cMemSize = C.GetMemoryUsageInBytes(s.ptr)
			s.memSize.Store(int64(cMemSize))
			return nil, nil
		}).Await()

		memSize = int64(cMemSize)
	}
	return memSize
}

func (s *LocalSegment) LastDeltaTimestamp() uint64 {
	return s.lastDeltaTimestamp.Load()
}

func (s *LocalSegment) addIndex(fieldID int64, info *IndexedFieldInfo) {
	s.fieldIndexes.Insert(fieldID, info)
}

func (s *LocalSegment) GetIndex(fieldID int64) *IndexedFieldInfo {
	info, _ := s.fieldIndexes.Get(fieldID)
	return info
}

func (s *LocalSegment) ExistIndex(fieldID int64) bool {
	fieldInfo, ok := s.fieldIndexes.Get(fieldID)
	if !ok {
		return false
	}
	return fieldInfo.IndexInfo != nil && fieldInfo.IndexInfo.EnableIndex
}

func (s *LocalSegment) HasRawData(fieldID int64) bool {
	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()
	if !s.isValid() {
		return false
	}
	ret := C.HasRawData(s.ptr, C.int64_t(fieldID))
	return bool(ret)
}

func (s *LocalSegment) Indexes() []*IndexedFieldInfo {
	var result []*IndexedFieldInfo
	s.fieldIndexes.Range(func(key int64, value *IndexedFieldInfo) bool {
		result = append(result, value)
		return true
	})
	return result
}

func (s *LocalSegment) Search(ctx context.Context, searchReq *SearchRequest) (*SearchResult, error) {
	/*
		CStatus
		Search(void* plan,
			void* placeholder_groups,
			uint64_t* timestamps,
			int num_groups,
			long int* result_ids,
			float* result_distances);
	*/
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("segmentID", s.ID()),
		zap.String("segmentType", s.typ.String()),
	)
	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()

	if s.ptr == nil {
		return nil, merr.WrapErrSegmentNotLoaded(s.segmentID, "segment released")
	}

	traceCtx := ParseCTraceContext(ctx)

	hasIndex := s.ExistIndex(searchReq.searchFieldID)
	log = log.With(zap.Bool("withIndex", hasIndex))
	log.Debug("search segment...")

	var searchResult SearchResult
	var status C.CStatus
	GetSQPool().Submit(func() (any, error) {
		tr := timerecord.NewTimeRecorder("cgoSearch")
		status = C.Search(traceCtx,
			s.ptr,
			searchReq.plan.cSearchPlan,
			searchReq.cPlaceholderGroup,
			C.uint64_t(searchReq.mvccTimestamp),
			&searchResult.cSearchResult,
		)
		metrics.QueryNodeSQSegmentLatencyInCore.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.SearchLabel).Observe(float64(tr.ElapseSpan().Milliseconds()))
		return nil, nil
	}).Await()
	if err := HandleCStatus(ctx, &status, "Search failed",
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("segmentID", s.ID()),
		zap.String("segmentType", s.typ.String())); err != nil {
		return nil, err
	}
	log.Debug("search segment done")
	return &searchResult, nil
}

func (s *LocalSegment) Retrieve(ctx context.Context, plan *RetrievePlan) (*segcorepb.RetrieveResults, error) {
	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()

	if s.ptr == nil {
		return nil, merr.WrapErrSegmentNotLoaded(s.segmentID, "segment released")
	}

	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID()),
		zap.Int64("msgID", plan.msgID),
		zap.String("segmentType", s.typ.String()),
	)

	traceCtx := ParseCTraceContext(ctx)

	maxLimitSize := paramtable.Get().QuotaConfig.MaxOutputSize.GetAsInt64()
	var retrieveResult RetrieveResult
	var status C.CStatus
	GetSQPool().Submit(func() (any, error) {
		ts := C.uint64_t(plan.Timestamp)
		tr := timerecord.NewTimeRecorder("cgoRetrieve")
		status = C.Retrieve(traceCtx,
			s.ptr,
			plan.cRetrievePlan,
			ts,
			&retrieveResult.cRetrieveResult,
			C.int64_t(maxLimitSize))

		metrics.QueryNodeSQSegmentLatencyInCore.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()),
			metrics.QueryLabel).Observe(float64(tr.ElapseSpan().Milliseconds()))
		log.Debug("cgo retrieve done", zap.Duration("timeTaken", tr.ElapseSpan()))
		return nil, nil
	}).Await()

	if err := HandleCStatus(ctx, &status, "Retrieve failed",
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID()),
		zap.Int64("msgID", plan.msgID),
		zap.String("segmentType", s.typ.String())); err != nil {
		return nil, err
	}

	result := new(segcorepb.RetrieveResults)
	if err := HandleCProto(&retrieveResult.cRetrieveResult, result); err != nil {
		return nil, err
	}

	log.Debug("retrieve segment done",
		zap.Int("resultNum", len(result.Offset)),
	)

	// Sort was done by the segcore.
	// sort.Sort(&byPK{result})
	return result, nil
}

func (s *LocalSegment) GetFieldDataPath(index *IndexedFieldInfo, offset int64) (dataPath string, offsetInBinlog int64) {
	offsetInBinlog = offset
	for _, binlog := range index.FieldBinlog.Binlogs {
		if offsetInBinlog < binlog.EntriesNum {
			dataPath = binlog.GetLogPath()
			break
		} else {
			offsetInBinlog -= binlog.EntriesNum
		}
	}
	return dataPath, offsetInBinlog
}

// -------------------------------------------------------------------------------------- interfaces for growing segment
func (s *LocalSegment) preInsert(ctx context.Context, numOfRecords int) (int64, error) {
	/*
		long int
		PreInsert(CSegmentInterface c_segment, long int size);
	*/
	var offset int64
	cOffset := (*C.int64_t)(&offset)

	var status C.CStatus
	GetDynamicPool().Submit(func() (any, error) {
		status = C.PreInsert(s.ptr, C.int64_t(int64(numOfRecords)), cOffset)
		return nil, nil
	}).Await()
	if err := HandleCStatus(ctx, &status, "PreInsert failed"); err != nil {
		return 0, err
	}
	return offset, nil
}

func (s *LocalSegment) Insert(ctx context.Context, rowIDs []int64, timestamps []typeutil.Timestamp, record *segcorepb.InsertRecord) error {
	if s.Type() != SegmentTypeGrowing {
		return fmt.Errorf("unexpected segmentType when segmentInsert, segmentType = %s", s.typ.String())
	}

	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()

	if s.ptr == nil {
		return merr.WrapErrSegmentNotLoaded(s.segmentID, "segment released")
	}

	offset, err := s.preInsert(ctx, len(rowIDs))
	if err != nil {
		return err
	}

	insertRecordBlob, err := proto.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to marshal insert record: %s", err)
	}

	numOfRow := len(rowIDs)
	cOffset := C.int64_t(offset)
	cNumOfRows := C.int64_t(numOfRow)
	cEntityIdsPtr := (*C.int64_t)(&(rowIDs)[0])
	cTimestampsPtr := (*C.uint64_t)(&(timestamps)[0])

	var status C.CStatus

	GetDynamicPool().Submit(func() (any, error) {
		status = C.Insert(s.ptr,
			cOffset,
			cNumOfRows,
			cEntityIdsPtr,
			cTimestampsPtr,
			(*C.uint8_t)(unsafe.Pointer(&insertRecordBlob[0])),
			(C.uint64_t)(len(insertRecordBlob)),
		)
		return nil, nil
	}).Await()
	if err := HandleCStatus(ctx, &status, "Insert failed"); err != nil {
		return err
	}

	s.insertCount.Add(int64(numOfRow))
	s.rowNum.Store(-1)
	s.memSize.Store(-1)
	return nil
}

func (s *LocalSegment) Delete(ctx context.Context, primaryKeys []storage.PrimaryKey, timestamps []typeutil.Timestamp) error {
	/*
		CStatus
		Delete(CSegmentInterface c_segment,
		           long int reserved_offset,
		           long size,
		           const long* primary_keys,
		           const unsigned long* timestamps);
	*/

	if len(primaryKeys) == 0 {
		return nil
	}

	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()

	if s.ptr == nil {
		return merr.WrapErrSegmentNotLoaded(s.segmentID, "segment released")
	}

	cOffset := C.int64_t(0) // depre
	cSize := C.int64_t(len(primaryKeys))
	cTimestampsPtr := (*C.uint64_t)(&(timestamps)[0])

	ids := &schemapb.IDs{}
	pkType := primaryKeys[0].Type()
	switch pkType {
	case schemapb.DataType_Int64:
		int64Pks := make([]int64, len(primaryKeys))
		for index, pk := range primaryKeys {
			int64Pks[index] = pk.(*storage.Int64PrimaryKey).Value
		}
		ids.IdField = &schemapb.IDs_IntId{
			IntId: &schemapb.LongArray{
				Data: int64Pks,
			},
		}
	case schemapb.DataType_VarChar:
		varCharPks := make([]string, len(primaryKeys))
		for index, entity := range primaryKeys {
			varCharPks[index] = entity.(*storage.VarCharPrimaryKey).Value
		}
		ids.IdField = &schemapb.IDs_StrId{
			StrId: &schemapb.StringArray{
				Data: varCharPks,
			},
		}
	default:
		return fmt.Errorf("invalid data type of primary keys")
	}

	dataBlob, err := proto.Marshal(ids)
	if err != nil {
		return fmt.Errorf("failed to marshal ids: %s", err)
	}
	var status C.CStatus
	GetDynamicPool().Submit(func() (any, error) {
		status = C.Delete(s.ptr,
			cOffset,
			cSize,
			(*C.uint8_t)(unsafe.Pointer(&dataBlob[0])),
			(C.uint64_t)(len(dataBlob)),
			cTimestampsPtr,
		)
		return nil, nil
	}).Await()

	if err := HandleCStatus(ctx, &status, "Delete failed"); err != nil {
		return err
	}

	s.rowNum.Store(-1)
	s.lastDeltaTimestamp.Store(timestamps[len(timestamps)-1])

	return nil
}

// -------------------------------------------------------------------------------------- interfaces for sealed segment
func (s *LocalSegment) LoadMultiFieldData(ctx context.Context, rowCount int64, fields []*datapb.FieldBinlog) error {
	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()

	if s.ptr == nil {
		return merr.WrapErrSegmentNotLoaded(s.segmentID, "segment released")
	}

	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID()),
	)

	loadFieldDataInfo, err := newLoadFieldDataInfo(ctx)
	defer deleteFieldDataInfo(loadFieldDataInfo)
	if err != nil {
		return err
	}

	for _, field := range fields {
		fieldID := field.FieldID
		err = loadFieldDataInfo.appendLoadFieldInfo(ctx, fieldID, rowCount)
		if err != nil {
			return err
		}

		for _, binlog := range field.Binlogs {
			err = loadFieldDataInfo.appendLoadFieldDataPath(ctx, fieldID, binlog)
			if err != nil {
				return err
			}
		}

		loadFieldDataInfo.appendMMapDirPath(paramtable.Get().QueryNodeCfg.MmapDirPath.GetValue())
	}

	var status C.CStatus
	GetLoadPool().Submit(func() (any, error) {
		if paramtable.Get().CommonCfg.EnableStorageV2.GetAsBool() {
			uri, err := typeutil_internal.GetStorageURI(paramtable.Get().CommonCfg.StorageScheme.GetValue(), paramtable.Get().CommonCfg.StoragePathPrefix.GetValue(), s.segmentID)
			if err != nil {
				return nil, err
			}

			loadFieldDataInfo.appendURI(uri)
			loadFieldDataInfo.appendStorageVersion(s.space.GetCurrentVersion())
			status = C.LoadFieldDataV2(s.ptr, loadFieldDataInfo.cLoadFieldDataInfo)
		} else {
			status = C.LoadFieldData(s.ptr, loadFieldDataInfo.cLoadFieldDataInfo)
		}
		return nil, nil
	}).Await()
	if err := HandleCStatus(ctx, &status, "LoadMultiFieldData failed",
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID())); err != nil {
		return err
	}

	s.insertCount.Store(rowCount)
	log.Info("load mutil field done",
		zap.Int64("row count", rowCount),
		zap.Int64("segmentID", s.ID()))

	return nil
}

type loadOptions struct {
	LoadStatus LoadStatus
}

func newLoadOptions() *loadOptions {
	return &loadOptions{
		LoadStatus: LoadStatusInMemory,
	}
}

type loadOption func(*loadOptions)

func WithLoadStatus(loadStatus LoadStatus) loadOption {
	return func(options *loadOptions) {
		options.LoadStatus = loadStatus
	}
}

func (s *LocalSegment) LoadFieldData(ctx context.Context, fieldID int64, rowCount int64, field *datapb.FieldBinlog, opts ...loadOption) error {
	options := newLoadOptions()
	for _, opt := range opts {
		opt(options)
	}

	if field != nil {
		s.fields.Insert(fieldID, &FieldInfo{
			FieldBinlog: *field,
			RowCount:    rowCount,
		})
	}

	if options.LoadStatus == LoadStatusMeta {
		return nil
	}

	s.loadStatus.Store(string(options.LoadStatus))

	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()

	ctx, sp := otel.Tracer(typeutil.QueryNodeRole).Start(ctx, fmt.Sprintf("LoadFieldData-%d-%d", s.segmentID, fieldID))
	defer sp.End()

	if s.ptr == nil {
		return merr.WrapErrSegmentNotLoaded(s.segmentID, "segment released")
	}

	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID()),
		zap.Int64("fieldID", fieldID),
		zap.Int64("rowCount", rowCount),
	)
	log.Info("start loading field data for field")

	loadFieldDataInfo, err := newLoadFieldDataInfo(ctx)
	defer deleteFieldDataInfo(loadFieldDataInfo)
	if err != nil {
		return err
	}

	err = loadFieldDataInfo.appendLoadFieldInfo(ctx, fieldID, rowCount)
	if err != nil {
		return err
	}

	if field != nil {
		for _, binlog := range field.Binlogs {
			err = loadFieldDataInfo.appendLoadFieldDataPath(ctx, fieldID, binlog)
			if err != nil {
				return err
			}
		}
	}

	mmapEnabled := options.LoadStatus == LoadStatusMapped
	loadFieldDataInfo.appendMMapDirPath(paramtable.Get().QueryNodeCfg.MmapDirPath.GetValue())
	loadFieldDataInfo.enableMmap(fieldID, mmapEnabled)

	var status C.CStatus
	GetLoadPool().Submit(func() (any, error) {
		log.Info("submitted loadFieldData task to dy pool")
		if paramtable.Get().CommonCfg.EnableStorageV2.GetAsBool() {
			uri, err := typeutil_internal.GetStorageURI(paramtable.Get().CommonCfg.StorageScheme.GetValue(), paramtable.Get().CommonCfg.StoragePathPrefix.GetValue(), s.segmentID)
			if err != nil {
				return nil, err
			}

			loadFieldDataInfo.appendURI(uri)
			loadFieldDataInfo.appendStorageVersion(s.space.GetCurrentVersion())
			status = C.LoadFieldDataV2(s.ptr, loadFieldDataInfo.cLoadFieldDataInfo)
		} else {
			status = C.LoadFieldData(s.ptr, loadFieldDataInfo.cLoadFieldDataInfo)
		}
		return nil, nil
	}).Await()
	if err := HandleCStatus(ctx, &status, "LoadFieldData failed",
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID()),
		zap.Int64("fieldID", fieldID)); err != nil {
		return err
	}

	s.insertCount.Store(rowCount)
	log.Info("load field done")

	return nil
}

func (s *LocalSegment) LoadDeltaData2(ctx context.Context, schema *schemapb.CollectionSchema) error {
	deleteReader, err := s.space.ScanDelete()
	if err != nil {
		return err
	}
	if !deleteReader.Schema().HasField(common.TimeStampFieldName) {
		return fmt.Errorf("can not read timestamp field in space")
	}
	pkFieldSchema, err := typeutil.GetPrimaryFieldSchema(schema)
	if err != nil {
		return err
	}
	ids := &schemapb.IDs{}
	var pkint64s []int64
	var pkstrings []string
	var tss []int64
	for deleteReader.Next() {
		rec := deleteReader.Record()
		indices := rec.Schema().FieldIndices(common.TimeStampFieldName)
		tss = append(tss, rec.Column(indices[0]).(*array.Int64).Int64Values()...)
		indices = rec.Schema().FieldIndices(pkFieldSchema.Name)
		switch pkFieldSchema.DataType {
		case schemapb.DataType_Int64:
			pkint64s = append(pkint64s, rec.Column(indices[0]).(*array.Int64).Int64Values()...)
		case schemapb.DataType_VarChar:
			columnData := rec.Column(indices[0]).(*array.String)
			for i := 0; i < columnData.Len(); i++ {
				pkstrings = append(pkstrings, columnData.Value(i))
			}
		default:
			return fmt.Errorf("unknown data type %v", pkFieldSchema.DataType)
		}
	}
	if err := deleteReader.Err(); err != nil && err != io.EOF {
		return err
	}

	switch pkFieldSchema.DataType {
	case schemapb.DataType_Int64:
		ids.IdField = &schemapb.IDs_IntId{
			IntId: &schemapb.LongArray{
				Data: pkint64s,
			},
		}
	case schemapb.DataType_VarChar:
		ids.IdField = &schemapb.IDs_StrId{
			StrId: &schemapb.StringArray{
				Data: pkstrings,
			},
		}
	default:
		return fmt.Errorf("unknown data type %v", pkFieldSchema.DataType)
	}

	idsBlob, err := proto.Marshal(ids)
	if err != nil {
		return err
	}

	if len(tss) == 0 {
		return nil
	}

	loadInfo := C.CLoadDeletedRecordInfo{
		timestamps:        unsafe.Pointer(&tss[0]),
		primary_keys:      (*C.uint8_t)(unsafe.Pointer(&idsBlob[0])),
		primary_keys_size: C.uint64_t(len(idsBlob)),
		row_count:         C.int64_t(len(tss)),
	}
	/*
		CStatus
		LoadDeletedRecord(CSegmentInterface c_segment, CLoadDeletedRecordInfo deleted_record_info)
	*/
	var status C.CStatus
	GetDynamicPool().Submit(func() (any, error) {
		status = C.LoadDeletedRecord(s.ptr, loadInfo)
		return nil, nil
	}).Await()

	if err := HandleCStatus(ctx, &status, "LoadDeletedRecord failed"); err != nil {
		return err
	}

	log.Info("load deleted record done",
		zap.Int("rowNum", len(tss)),
		zap.String("segmentType", s.Type().String()))
	return nil
}

func (s *LocalSegment) AddFieldDataInfo(ctx context.Context, rowCount int64, fields []*datapb.FieldBinlog) error {
	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()

	if s.ptr == nil {
		return merr.WrapErrSegmentNotLoaded(s.segmentID, "segment released")
	}

	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID()),
		zap.Int64("row count", rowCount),
	)

	loadFieldDataInfo, err := newLoadFieldDataInfo(ctx)
	defer deleteFieldDataInfo(loadFieldDataInfo)
	if err != nil {
		return err
	}

	for _, field := range fields {
		fieldID := field.FieldID
		err = loadFieldDataInfo.appendLoadFieldInfo(ctx, fieldID, rowCount)
		if err != nil {
			return err
		}

		for _, binlog := range field.Binlogs {
			err = loadFieldDataInfo.appendLoadFieldDataPath(ctx, fieldID, binlog)
			if err != nil {
				return err
			}
		}
	}

	var status C.CStatus
	GetLoadPool().Submit(func() (any, error) {
		status = C.AddFieldDataInfoForSealed(s.ptr, loadFieldDataInfo.cLoadFieldDataInfo)
		return nil, nil
	}).Await()
	if err := HandleCStatus(ctx, &status, "AddFieldDataInfo failed",
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID())); err != nil {
		return err
	}

	log.Info("add field data info done")
	return nil
}

func (s *LocalSegment) LoadDeltaData(ctx context.Context, deltaData *storage.DeleteData) error {
	pks, tss := deltaData.Pks, deltaData.Tss
	rowNum := deltaData.RowCount

	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()

	if s.ptr == nil {
		return merr.WrapErrSegmentNotLoaded(s.segmentID, "segment released")
	}

	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID()),
	)

	pkType := pks[0].Type()
	ids := &schemapb.IDs{}
	switch pkType {
	case schemapb.DataType_Int64:
		int64Pks := make([]int64, len(pks))
		for index, pk := range pks {
			int64Pks[index] = pk.(*storage.Int64PrimaryKey).Value
		}
		ids.IdField = &schemapb.IDs_IntId{
			IntId: &schemapb.LongArray{
				Data: int64Pks,
			},
		}
	case schemapb.DataType_VarChar:
		varCharPks := make([]string, len(pks))
		for index, pk := range pks {
			varCharPks[index] = pk.(*storage.VarCharPrimaryKey).Value
		}
		ids.IdField = &schemapb.IDs_StrId{
			StrId: &schemapb.StringArray{
				Data: varCharPks,
			},
		}
	default:
		return fmt.Errorf("invalid data type of primary keys")
	}

	idsBlob, err := proto.Marshal(ids)
	if err != nil {
		return err
	}

	loadInfo := C.CLoadDeletedRecordInfo{
		timestamps:        unsafe.Pointer(&tss[0]),
		primary_keys:      (*C.uint8_t)(unsafe.Pointer(&idsBlob[0])),
		primary_keys_size: C.uint64_t(len(idsBlob)),
		row_count:         C.int64_t(rowNum),
	}
	/*
		CStatus
		LoadDeletedRecord(CSegmentInterface c_segment, CLoadDeletedRecordInfo deleted_record_info)
	*/
	var status C.CStatus
	GetDynamicPool().Submit(func() (any, error) {
		status = C.LoadDeletedRecord(s.ptr, loadInfo)
		return nil, nil
	}).Await()

	if err := HandleCStatus(ctx, &status, "LoadDeletedRecord failed",
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID())); err != nil {
		return err
	}

	s.rowNum.Store(-1)
	s.lastDeltaTimestamp.Store(tss[len(tss)-1])

	log.Info("load deleted record done",
		zap.Int64("rowNum", rowNum),
		zap.String("segmentType", s.Type().String()))
	return nil
}

func (s *LocalSegment) LoadIndex(ctx context.Context, indexInfo *querypb.FieldIndexInfo, fieldType schemapb.DataType, opts ...loadOption) error {
	options := newLoadOptions()
	for _, opt := range opts {
		opt(options)
	}

	old := s.GetIndex(indexInfo.GetFieldID())
	// the index loaded
	if old != nil && old.IndexInfo.GetIndexID() == indexInfo.GetIndexID() {
		log.Warn("index already loaded")
		return nil
	}

	ctx, sp := otel.Tracer(typeutil.QueryNodeRole).Start(ctx, fmt.Sprintf("LoadIndex-%d-%d", s.segmentID, indexInfo.GetFieldID()))
	defer sp.End()

	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID()),
		zap.Int64("fieldID", indexInfo.GetFieldID()),
		zap.Int64("indexID", indexInfo.GetIndexID()),
	)

	loadIndexInfo, err := newLoadIndexInfo(ctx)
	if err != nil {
		return err
	}
	defer deleteLoadIndexInfo(loadIndexInfo)

	if paramtable.Get().CommonCfg.EnableStorageV2.GetAsBool() {
		uri, err := typeutil_internal.GetStorageURI(paramtable.Get().CommonCfg.StorageScheme.GetValue(), paramtable.Get().CommonCfg.StoragePathPrefix.GetValue(), s.segmentID)
		if err != nil {
			return err
		}

		loadIndexInfo.appendStorageInfo(uri, indexInfo.IndexStoreVersion)
	}
	err = loadIndexInfo.appendLoadIndexInfo(ctx, indexInfo, s.collectionID, s.partitionID, s.segmentID, fieldType)
	if err != nil {
		if loadIndexInfo.cleanLocalData(ctx) != nil {
			log.Warn("failed to clean cached data on disk after append index failed",
				zap.Int64("buildID", indexInfo.BuildID),
				zap.Int64("index version", indexInfo.IndexVersion))
		}
		return err
	}
	if s.Type() != SegmentTypeSealed {
		errMsg := fmt.Sprintln("updateSegmentIndex failed, illegal segment type ", s.typ, "segmentID = ", s.ID())
		return errors.New(errMsg)
	}

	err = s.UpdateIndexInfo(ctx, indexInfo, loadIndexInfo)
	if err != nil {
		return err
	}

	if !typeutil.IsVectorType(fieldType) || s.HasRawData(indexInfo.GetFieldID()) {
		return nil
	}
	s.WarmupChunkCache(ctx, indexInfo.GetFieldID())
	return nil
}

func (s *LocalSegment) UpdateIndexInfo(ctx context.Context, indexInfo *querypb.FieldIndexInfo, info *LoadIndexInfo) error {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID()),
		zap.Int64("fieldID", indexInfo.FieldID),
	)
	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()

	if s.ptr == nil {
		return merr.WrapErrSegmentNotLoaded(s.segmentID, "segment released")
	}

	var status C.CStatus
	GetDynamicPool().Submit(func() (any, error) {
		status = C.UpdateSealedSegmentIndex(s.ptr, info.cLoadIndexInfo)
		return nil, nil
	}).Await()

	if err := HandleCStatus(ctx, &status, "UpdateSealedSegmentIndex failed",
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID()),
		zap.Int64("fieldID", indexInfo.FieldID)); err != nil {
		return err
	}

	s.addIndex(indexInfo.GetFieldID(), &IndexedFieldInfo{
		FieldBinlog: &datapb.FieldBinlog{
			FieldID: indexInfo.GetFieldID(),
		},
		IndexInfo: indexInfo,
	})
	log.Info("updateSegmentIndex done")
	return nil
}

func (s *LocalSegment) WarmupChunkCache(ctx context.Context, fieldID int64) {
	log := log.Ctx(ctx).With(
		zap.Int64("collectionID", s.Collection()),
		zap.Int64("partitionID", s.Partition()),
		zap.Int64("segmentID", s.ID()),
		zap.Int64("fieldID", fieldID),
	)
	s.ptrLock.RLock()
	defer s.ptrLock.RUnlock()

	if s.ptr == nil {
		return
	}

	var status C.CStatus

	warmingUp := strings.ToLower(paramtable.Get().QueryNodeCfg.ChunkCacheWarmingUp.GetValue())
	switch warmingUp {
	case "sync":
		GetLoadPool().Submit(func() (any, error) {
			cFieldID := C.int64_t(fieldID)
			status = C.WarmupChunkCache(s.ptr, cFieldID)
			if err := HandleCStatus(ctx, &status, "warming up chunk cache failed"); err != nil {
				log.Warn("warming up chunk cache synchronously failed", zap.Error(err))
				return nil, err
			}
			log.Info("warming up chunk cache synchronously done")
			return nil, nil
		}).Await()
	case "async":
		GetLoadPool().Submit(func() (any, error) {
			s.ptrLock.RLock()
			defer s.ptrLock.RUnlock()
			if s.ptr == nil {
				return nil, nil
			}
			cFieldID := C.int64_t(fieldID)
			status = C.WarmupChunkCache(s.ptr, cFieldID)
			if err := HandleCStatus(ctx, &status, ""); err != nil {
				log.Warn("warming up chunk cache asynchronously failed", zap.Error(err))
				return nil, err
			}
			log.Info("warming up chunk cache asynchronously done")
			return nil, nil
		})
	default:
		// no warming up
	}
}

func (s *LocalSegment) UpdateFieldRawDataSize(ctx context.Context, numRows int64, fieldBinlog *datapb.FieldBinlog) error {
	var status C.CStatus
	fieldID := fieldBinlog.FieldID
	fieldDataSize := int64(0)
	for _, binlog := range fieldBinlog.GetBinlogs() {
		fieldDataSize += binlog.LogSize
	}
	GetDynamicPool().Submit(func() (any, error) {
		status = C.UpdateFieldRawDataSize(s.ptr, C.int64_t(fieldID), C.int64_t(numRows), C.int64_t(fieldDataSize))
		return nil, nil
	}).Await()

	if err := HandleCStatus(ctx, &status, "updateFieldRawDataSize failed"); err != nil {
		return err
	}

	log.Ctx(ctx).Info("updateFieldRawDataSize done", zap.Int64("segmentID", s.ID()))

	return nil
}

type ReleaseScope int

const (
	ReleaseScopeAll ReleaseScope = iota
	ReleaseScopeData
)

type releaseOptions struct {
	Scope ReleaseScope
}

func newReleaseOptions() *releaseOptions {
	return &releaseOptions{
		Scope: ReleaseScopeAll,
	}
}

type releaseOption func(*releaseOptions)

func WithReleaseScope(scope ReleaseScope) releaseOption {
	return func(options *releaseOptions) {
		options.Scope = scope
	}
}

func (s *LocalSegment) Release(opts ...releaseOption) {
	options := newReleaseOptions()
	for _, opt := range opts {
		opt(options)
	}

	/*
		void
		deleteSegment(CSegmentInterface segment);
	*/
	// wait all read ops finished
	var ptr C.CSegmentInterface

	s.ptrLock.Lock()
	ptr = s.ptr
	s.ptr = nil
	if options.Scope == ReleaseScopeData {
		s.loadStatus.Store(string(LoadStatusMeta))
	}
	s.ptrLock.Unlock()

	if ptr == nil {
		return
	}
	if options.Scope == ReleaseScopeData {
		C.ClearSegmentData(ptr)
		return
	}

	C.DeleteSegment(ptr)

	metrics.QueryNodeNumEntities.WithLabelValues(
		fmt.Sprint(paramtable.GetNodeID()),
		fmt.Sprint(s.Collection()),
		fmt.Sprint(s.Partition()),
		s.Type().String(),
		strconv.FormatInt(int64(len(s.Indexes())), 10),
	).Sub(float64(s.InsertCount()))

	localDiskUsage, err := GetLocalUsedSize(context.Background(), paramtable.Get().LocalStorageCfg.Path.GetValue())
	// ignore error here, shall not block releasing
	if err == nil {
		metrics.QueryNodeDiskUsedSize.WithLabelValues(fmt.Sprint(paramtable.GetNodeID())).Set(float64(localDiskUsage) / 1024 / 1024) // in MB
	}

	log.Info("delete segment from memory",
		zap.Int64("collectionID", s.collectionID),
		zap.Int64("partitionID", s.partitionID),
		zap.Int64("segmentID", s.ID()),
		zap.String("segmentType", s.typ.String()),
		zap.Int64("insertCount", s.InsertCount()),
	)
}
