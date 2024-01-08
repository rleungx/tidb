// Copyright 2020 PingCAP, Inc.
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

package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/distsql"
	"github.com/pingcap/tidb/executor/mpperr"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/parser/model"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/mathutil"
	"github.com/pingcap/tidb/util/memory"
	"github.com/pingcap/tipb/go-tipb"
	"go.uber.org/zap"
)

// For mpp err recovery, hold at most 4 * MaxChunkSize rows.
const mppErrRecoveryHoldChkCap = 4

func useMPPExecution(ctx sessionctx.Context, tr *plannercore.PhysicalTableReader) bool {
	if !ctx.GetSessionVars().IsMPPAllowed() {
		return false
	}
	_, ok := tr.GetTablePlan().(*plannercore.PhysicalExchangeSender)
	return ok
}

func getMPPQueryID(ctx sessionctx.Context) uint64 {
	mppQueryInfo := &ctx.GetSessionVars().StmtCtx.MPPQueryInfo
	mppQueryInfo.QueryID.CompareAndSwap(0, plannercore.AllocMPPQueryID())
	return mppQueryInfo.QueryID.Load()
}

func getMPPQueryTS(ctx sessionctx.Context) uint64 {
	mppQueryInfo := &ctx.GetSessionVars().StmtCtx.MPPQueryInfo
	mppQueryInfo.QueryTS.CompareAndSwap(0, uint64(time.Now().UnixNano()))
	return mppQueryInfo.QueryTS.Load()
}

func allocMPPGatherID(ctx sessionctx.Context) uint64 {
	mppQueryInfo := &ctx.GetSessionVars().StmtCtx.MPPQueryInfo
	return mppQueryInfo.AllocatedGatherID.Add(1)
}

// MPPGather dispatch MPP tasks and read data from root tasks.
type MPPGather struct {
	// following fields are construct needed
	baseExecutor
	is           infoschema.InfoSchema
	originalPlan plannercore.PhysicalPlan
	startTS      uint64
	mppQueryID   kv.MPPQueryID

	mppReqs []*kv.MPPDispatchRequest

	respIter distsql.SelectResult

	memTracker *memory.Tracker

	// For virtual column.
	columns                    []*model.ColumnInfo
	virtualColumnIndex         []int
	virtualColumnRetFieldTypes []*types.FieldType

	// For UnionScan.
	table    table.Table
	kvRanges []kv.KeyRange
	dummy    bool

	gatherID uint64

	// mppErrRecovery is designed for the recovery of MPP errors.
	// Basic idea:
	// 1. It attempts to hold the results of MPP. During the holding process, if an error occurs, it starts error recovery.
	//    If the recovery is successful, it discards held results and reconstructs the respIter, then re-executes the MPP task.
	//    If the recovery fails, an error is reported directly.
	// 2. If the held MPP results exceed the capacity, will starts returning results to caller.
	//    Once the results start being returned, error recovery cannot be performed anymore.
	mppErrRecovery *mpperr.RecoveryHandler
	// Only for MemLimit err recovery for now.
	// AutoScaler use this value as hint to scale out CN.
	nodeCnt int
}

func (e *MPPGather) appendMPPDispatchReq(pf *plannercore.Fragment) error {
	dagReq, err := constructDAGReq(e.ctx, []plannercore.PhysicalPlan{pf.ExchangeSender}, kv.TiFlash)
	if err != nil {
		return errors.Trace(err)
	}
	for i := range pf.ExchangeSender.Schema().Columns {
		dagReq.OutputOffsets = append(dagReq.OutputOffsets, uint32(i))
	}
	if !pf.IsRoot {
		dagReq.EncodeType = tipb.EncodeType_TypeCHBlock
	} else {
		dagReq.EncodeType = tipb.EncodeType_TypeChunk
	}
	rgName := e.base().ctx.GetSessionVars().ResourceGroupName
	if !variable.EnableResourceControl.Load() {
		rgName = ""
	}
	for _, mppTask := range pf.ExchangeSender.Tasks {
		if mppTask.PartitionTableIDs != nil {
			err = updateExecutorTableID(context.Background(), dagReq.RootExecutor, true, mppTask.PartitionTableIDs)
		} else if !mppTask.IsDisaggregatedTiFlashStaticPrune {
			// If isDisaggregatedTiFlashStaticPrune is true, it means this TableScan is under PartitionUnoin,
			// tableID in TableScan is already the physical table id of this partition, no need to update again.
			err = updateExecutorTableID(context.Background(), dagReq.RootExecutor, true, []int64{mppTask.TableID})
		}
		if err != nil {
			return errors.Trace(err)
		}
		pbData, err := dagReq.Marshal()
		if err != nil {
			return errors.Trace(err)
		}

		logutil.BgLogger().Info("Dispatch mpp task", zap.Uint64("timestamp", mppTask.StartTs),
			zap.Int64("ID", mppTask.ID), zap.Uint64("QueryTs", mppTask.MppQueryID.QueryTs), zap.Uint64("LocalQueryId", mppTask.MppQueryID.LocalQueryID),
			zap.Uint64("ServerID", mppTask.MppQueryID.ServerID), zap.String("address", mppTask.Meta.GetAddress()),
			zap.String("plan", plannercore.ToString(pf.ExchangeSender)),
			zap.Int64("mpp-version", mppTask.MppVersion.ToInt64()),
			zap.String("exchange-compression-mode", pf.ExchangeSender.CompressionMode.Name()),
			zap.String("ResourceGroup", rgName),
		)
		if mppTask.GatherID != e.gatherID {
			return errors.Errorf("unexpected gather id for mpp task, expect %v, got %v", e.gatherID, mppTask.GatherID)
		}
		req := &kv.MPPDispatchRequest{
			Data:              pbData,
			Meta:              mppTask.Meta,
			GatherID:          mppTask.GatherID,
			ID:                mppTask.ID,
			IsRoot:            pf.IsRoot,
			Timeout:           10,
			SchemaVar:         e.is.SchemaMetaVersion(),
			StartTs:           e.startTS,
			MppQueryID:        mppTask.MppQueryID,
			State:             kv.MppTaskReady,
			ResourceGroupName: rgName,
		}
		e.mppReqs = append(e.mppReqs, req)
	}
	return nil
}

func collectPlanIDS(plan plannercore.PhysicalPlan, ids []int) []int {
	ids = append(ids, plan.ID())
	for _, child := range plan.Children() {
		ids = collectPlanIDS(child, ids)
	}
	return ids
}

func (e *MPPGather) setupRespIter(ctx context.Context, isRecoverying bool) error {
	if isRecoverying {
		// If we are trying to recovery from MPP error, needs to cleanup some resources.
		// Sanity check.
		if e.dummy {
			return errors.New("should not reset mpp resp iter for dummy table")
		}

		if e.respIter == nil {
			return errors.New("mpp resp iter should already be setup")
		}

		if err := e.respIter.Close(); err != nil {
			return err
		}
	}

	// TODO: Move the construct tasks logic to planner, so we can see the explain results.
	sender := e.originalPlan.(*plannercore.PhysicalExchangeSender)
	e.gatherID = allocMPPGatherID(e.ctx)
	planIDs := collectPlanIDS(e.originalPlan, nil)
	frags, kvRanges, nodeInfo, err := plannercore.GenerateRootMPPTasks(e.ctx, e.gatherID, e.startTS, e.mppQueryID, sender, e.is)
	if err != nil {
		return errors.Trace(err)
	}
	e.kvRanges = kvRanges

	if e.dummy {
		return nil
	}

	e.mppReqs = e.mppReqs[:0]
	for _, frag := range frags {
		if err = e.appendMPPDispatchReq(frag); err != nil {
			return errors.Trace(err)
		}
	}
	failpoint.Inject("checkTotalMPPTasks", func(val failpoint.Value) {
		if val.(int) != len(e.mppReqs) {
			failpoint.Return(errors.Errorf("The number of tasks is not right, expect %d tasks but actually there are %d tasks", val.(int), len(e.mppReqs)))
		}
	})
	if e.respIter, err = distsql.DispatchMPPTasks(ctx, e.ctx, e.mppReqs, e.retFieldTypes, planIDs, e.id, e.startTS, e.mppQueryID, e.memTracker); err != nil {
		return errors.Trace(err)
	}
	if e.nodeCnt = len(nodeInfo); e.nodeCnt <= 0 {
		return errors.Errorf("tiflash node count should be greater than zero: %v", e.nodeCnt)
	}

	return nil
}

// Open decides the task counts and locations and generate exchange operators for every plan fragment.
// Then dispatch tasks to tiflash stores. If any task fails, it would cancel the rest tasks.
func (e *MPPGather) Open(ctx context.Context) error {
	if err := e.setupRespIter(ctx, false); err != nil {
		return err
	}

	holdCap := mathutil.Max(32, mppErrRecoveryHoldChkCap*e.ctx.GetSessionVars().MaxChunkSize)

	disaggTiFlashWithAutoScaler := config.GetGlobalConfig().DisaggregatedTiFlash && config.GetGlobalConfig().UseAutoScaler
	_, allowTiFlashFallback := e.ctx.GetSessionVars().AllowFallbackToTiKV[kv.TiFlash]
	// 1. For now, mpp err recovery only support MemLimit, which is only useful when AutoScaler is used.
	// 2. When enable fallback to tikv, the returned mpp err will be ErrTiFlashServerTimeout,
	//    which we cannot handle for now. Also there is no need to recovery because tikv will retry the query.
	// 3. For cached table, will not dispatch tasks to TiFlash, so no need to recovery.
	enableMPPRecovery := disaggTiFlashWithAutoScaler && !allowTiFlashFallback && !e.dummy

	failpoint.Inject("mpp_recovery_test_mock_enable", func() {
		if !e.dummy && !allowTiFlashFallback {
			enableMPPRecovery = true
		}
	})

	e.mppErrRecovery = mpperr.NewRecoveryHandler(disaggTiFlashWithAutoScaler, uint64(holdCap), enableMPPRecovery, e.memTracker)
	return nil
}

func (e *MPPGather) nextWithRecovery(ctx context.Context) error {
	if !e.mppErrRecovery.Enabled() {
		return nil
	}

	for e.mppErrRecovery.CanHoldResult() {
		tmpChk := newFirstChunk(e)
		mppErr := e.respIter.Next(ctx, tmpChk)

		// Mock recovery n times.
		failpoint.Inject("mpp_recovery_test_max_err_times", func(forceErrCnt failpoint.Value) {
			forceErrCntInt := forceErrCnt.(int)
			if e.mppErrRecovery.RecoveryCnt() < uint32(forceErrCntInt) {
				mppErr = errors.New("mock mpp error")
			}
		})

		if mppErr != nil {
			recoveryErr := e.mppErrRecovery.Recovery(&mpperr.RecoveryInfo{
				MPPErr:  mppErr,
				NodeCnt: e.nodeCnt,
			})

			// Mock recovery succeed, ignore no recovery handler err.
			failpoint.Inject("mpp_recovery_test_ignore_recovery_err", func() {
				if recoveryErr == nil {
					panic("mocked mpp err should got recovery err")
				}
				if strings.Contains(recoveryErr.Error(), "no handler to recovery") {
					recoveryErr = nil
				}
			})

			if recoveryErr != nil {
				logutil.BgLogger().Error("recovery mpp error failed", zap.Any("mppErr", mppErr),
					zap.Any("recoveryErr", recoveryErr))
				return mppErr
			}

			logutil.BgLogger().Info("recovery mpp error succeed, begin next retry",
				zap.Any("mppErr", mppErr), zap.Any("recoveryCnt", e.mppErrRecovery.RecoveryCnt()))

			if err := e.setupRespIter(ctx, true); err != nil {
				logutil.BgLogger().Error("setup resp iter when recovery mpp err failed", zap.Any("err", err))
				return mppErr
			}
			e.mppErrRecovery.ResetHolder()

			continue
		}

		if tmpChk.NumRows() == 0 {
			break
		}

		e.mppErrRecovery.HoldResult(tmpChk)
	}

	failpoint.Inject("mpp_recovery_test_hold_size", func(num failpoint.Value) {
		// Note: this failpoint only execute once.
		curRows := e.mppErrRecovery.NumHoldRows()
		numInt := num.(int)
		if curRows != uint64(numInt) {
			panic(fmt.Sprintf("unexpected holding rows, cur: %d", curRows))
		}
	})
	return nil
}

// Next fills data into the chunk passed by its caller.
func (e *MPPGather) Next(ctx context.Context, chk *chunk.Chunk) error {
	chk.Reset()
	if e.dummy {
		return nil
	}

	if err := e.nextWithRecovery(ctx); err != nil {
		return err
	}

	if e.mppErrRecovery.NumHoldChk() != 0 {
		var tmpChk *chunk.Chunk
		if tmpChk = e.mppErrRecovery.PopFrontChk(); tmpChk == nil {
			return errors.New("cannot get chunk from mpp result holder")
		}
		chk.SwapColumns(tmpChk)
	} else if err := e.respIter.Next(ctx, chk); err != nil {
		// Got here when:
		// 1. mppErrRecovery is disabled. So no chk held in mppErrRecovery.
		// 2. mppErrRecovery is enabled and it holds some chks, but we consume all these chks.
		return err
	}

	if chk.NumRows() == 0 {
		return nil
	}

	err := table.FillVirtualColumnValue(e.virtualColumnRetFieldTypes, e.virtualColumnIndex, e.Schema().Columns, e.columns, e.ctx, chk)
	if err != nil {
		return err
	}
	return nil
}

// Close and release the used resources.
func (e *MPPGather) Close() error {
	if e.dummy {
		return nil
	}
	if e.mppErrRecovery != nil {
		e.mppErrRecovery.ResetHolder()
	}
	e.mppReqs = nil
	if e.respIter != nil {
		return e.respIter.Close()
	}
	return nil
}

// Table implements the dataSourceExecutor interface.
func (e *MPPGather) Table() table.Table {
	return e.table
}

func (e *MPPGather) setDummy() {
	e.dummy = true
}
