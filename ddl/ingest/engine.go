// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ingest

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/pingcap/tidb/br/pkg/lightning/backend"
	"github.com/pingcap/tidb/br/pkg/lightning/backend/kv"
	"github.com/pingcap/tidb/br/pkg/lightning/common"
	"github.com/pingcap/tidb/br/pkg/lightning/config"
	tidbkv "github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/util/logutil"
	"go.uber.org/zap"
)

// Engine is the interface for the engine that can be used to write key-value pairs.
type Engine interface {
	Flush() error
	ImportAndClean() error
	Clean()
	CreateWriter(id int, unique bool) (Writer, error)
}

// Writer is the interface for the writer that can be used to write key-value pairs.
type Writer interface {
	WriteRow(key, idxVal []byte, handle tidbkv.Handle) error
	LockForWrite() (unlock func())
	Close() error
	Flushed() bool
}

// engineInfo is the engine for one index reorg task, each task will create several new writers under the
// Opened Engine. Note engineInfo is not thread safe.
type engineInfo struct {
	ctx          context.Context
	jobID        int64
	indexID      int64
	openedEngine *backend.OpenedEngine
	uuid         uuid.UUID
	cfg          *backend.EngineConfig
	writerCount  int
	writerCache  sync.Map
	memRoot      MemRoot
	flushLock    *sync.RWMutex
	flushing     atomic.Bool
}

// newEngineInfo create a new engineInfo struct.
func newEngineInfo(ctx context.Context, jobID, indexID int64, cfg *backend.EngineConfig,
	en *backend.OpenedEngine, uuid uuid.UUID, memRoot MemRoot) *engineInfo {
	return &engineInfo{
		ctx:          ctx,
		jobID:        jobID,
		indexID:      indexID,
		cfg:          cfg,
		openedEngine: en,
		uuid:         uuid,
		writerCache:  sync.Map{},
		memRoot:      memRoot,
		flushLock:    &sync.RWMutex{},
	}
}

// Flush imports all the key-values in engine to the storage.
func (ei *engineInfo) Flush() error {
	err := ei.openedEngine.Flush(ei.ctx)
	if err != nil {
		logutil.BgLogger().Error(LitErrFlushEngineErr, zap.Error(err),
			zap.Int64("job ID", ei.jobID), zap.Int64("index ID", ei.indexID))
		return err
	}
	return nil
}

// Clean closes the engine and removes the local intermediate files.
func (ei *engineInfo) Clean() {
	if ei.openedEngine == nil {
		return
	}
	indexEngine := ei.openedEngine
	closedEngine, err := indexEngine.Close(ei.ctx)
	if err != nil {
		logutil.BgLogger().Error(LitErrCloseEngineErr, zap.Error(err),
			zap.Int64("job ID", ei.jobID), zap.Int64("index ID", ei.indexID))
		return
	}
	ei.openedEngine = nil
	err = ei.closeWriters()
	if err != nil {
		logutil.BgLogger().Error(LitErrCloseWriterErr, zap.Error(err),
			zap.Int64("job ID", ei.jobID), zap.Int64("index ID", ei.indexID))
	}
	// Here the local intermediate files will be removed.
	err = closedEngine.Cleanup(ei.ctx)
	if err != nil {
		logutil.BgLogger().Error(LitErrCleanEngineErr, zap.Error(err),
			zap.Int64("job ID", ei.jobID), zap.Int64("index ID", ei.indexID))
	}
}

// ImportAndClean imports the engine data to TiKV and cleans up the local intermediate files.
func (ei *engineInfo) ImportAndClean() error {
	// Close engine and finish local tasks of lightning.
	logutil.BgLogger().Info(LitInfoCloseEngine, zap.Int64("job ID", ei.jobID), zap.Int64("index ID", ei.indexID))
	indexEngine := ei.openedEngine
	closeEngine, err1 := indexEngine.Close(ei.ctx)
	if err1 != nil {
		logutil.BgLogger().Error(LitErrCloseEngineErr, zap.Error(err1),
			zap.Int64("job ID", ei.jobID), zap.Int64("index ID", ei.indexID))
		return err1
	}
	ei.openedEngine = nil
	err := ei.closeWriters()
	if err != nil {
		logutil.BgLogger().Error(LitErrCloseWriterErr, zap.Error(err),
			zap.Int64("job ID", ei.jobID), zap.Int64("index ID", ei.indexID))
		return err
	}

	// Ingest data to TiKV.
	logutil.BgLogger().Info(LitInfoStartImport, zap.Int64("job ID", ei.jobID),
		zap.Int64("index ID", ei.indexID),
		zap.String("split region size", strconv.FormatInt(int64(config.SplitRegionSize), 10)))
	err = closeEngine.Import(ei.ctx, int64(config.SplitRegionSize), int64(config.SplitRegionKeys))
	if err != nil {
		logutil.BgLogger().Error(LitErrIngestDataErr, zap.Error(err),
			zap.Int64("job ID", ei.jobID), zap.Int64("index ID", ei.indexID))
		return err
	}

	// Clean up the engine local workspace.
	err = closeEngine.Cleanup(ei.ctx)
	if err != nil {
		logutil.BgLogger().Error(LitErrCloseEngineErr, zap.Error(err),
			zap.Int64("job ID", ei.jobID), zap.Int64("index ID", ei.indexID))
		return err
	}
	return nil
}

// writerContext is used to keep a lightning local writer for each backfill worker.
type writerContext struct {
	ctx         context.Context
	unique      bool
	lWrite      backend.EngineWriter
	fLock       *sync.RWMutex
	flushStatus backend.ChunkFlushStatus
}

// CreateWriter creates a new writerContext.
func (ei *engineInfo) CreateWriter(id int, unique bool) (Writer, error) {
	wCtx, err := ei.newWriterContext(id, unique)
	if err != nil {
		logutil.BgLogger().Error(LitErrCreateContextFail, zap.Error(err),
			zap.Int64("job ID", ei.jobID), zap.Int64("index ID", ei.indexID),
			zap.Int("worker ID", id))
		return nil, err
	}
	return wCtx, err
}

// newWriterContext will get worker local writer from engine info writer cache first, if exists.
// If local writer not exist, then create new one and store it into engine info writer cache.
// note: operate ei.writeCache map is not thread safe please make sure there is sync mechanism to
// make sure the safe.
func (ei *engineInfo) newWriterContext(writerID int, unique bool) (*writerContext, error) {
	wc, exist := ei.writerCache.Load(writerID)
	if !exist {
		ei.memRoot.RefreshConsumption()
		ok := ei.memRoot.CheckConsume(StructSizeWriterCtx)
		if !ok {
			return nil, genEngineAllocMemFailedErr(ei.memRoot, ei.jobID, ei.indexID)
		}
		ei.flushLock.Lock()
		lWrite, err := ei.openedEngine.LocalWriter(ei.ctx, &backend.LocalWriterConfig{LocalWriterID: writerID})
		if err != nil {
			ei.flushLock.Unlock()
			return nil, err
		}
		ei.flushLock.Unlock()
		wc = &writerContext{
			ctx:    ei.ctx,
			unique: unique,
			lWrite: lWrite,
			fLock:  ei.flushLock,
		}
		// Cache the local writer.
		ei.writerCache.Store(writerID, wc)

		ei.memRoot.Consume(StructSizeWriterCtx)
		logutil.BgLogger().Info(LitInfoCreateWrite, zap.Int64("job ID", ei.jobID),
			zap.Int64("index ID", ei.indexID), zap.Int("writer ID", writerID),
			zap.Int64("allocate memory", StructSizeWriterCtx),
			zap.Int64("current memory usage", ei.memRoot.CurrentUsage()),
			zap.Int64("max memory quota", ei.memRoot.MaxMemoryQuota()))
	}
	return wc.(*writerContext), nil
}

func (ei *engineInfo) closeWriters() error {
	var firstErr error
	ei.writerCache.Range(func(key, value interface{}) bool {
		w := value.(*writerContext)
		_, err := w.lWrite.Close(ei.ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
		ei.writerCache.Delete(key)
		return true
	})
	return firstErr
}

// WriteRow Write one row into local writer buffer.
func (wCtx *writerContext) WriteRow(key, idxVal []byte, handle tidbkv.Handle) error {
	kvs := make([]common.KvPair, 1)
	kvs[0].Key = key
	kvs[0].Val = idxVal
	if wCtx.unique {
		kvs[0].RowID = handle.Encoded()
	}
	row := kv.MakeRowsFromKvPairs(kvs)
	return wCtx.lWrite.AppendRows(wCtx.ctx, nil, row)
}

// LockForWrite locks the local writer for write.
func (wCtx *writerContext) LockForWrite() (unlock func()) {
	wCtx.fLock.RLock()
	return func() {
		wCtx.fLock.RUnlock()
	}
}

// Close closes the local writer.
func (wCtx *writerContext) Close() error {
	flushStatus, err := wCtx.lWrite.Close(wCtx.ctx)
	if err != nil {
		return err
	}
	wCtx.flushStatus = flushStatus
	return nil
}

// Flushed returns the flush status of the local writer.
func (wCtx *writerContext) Flushed() bool {
	if wCtx.flushStatus == nil {
		return false
	}
	return wCtx.flushStatus.Flushed()
}
