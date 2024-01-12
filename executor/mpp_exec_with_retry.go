// Copyright 2023 PingCAP, Inc.
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

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/memory"
	"github.com/pingcap/tipb/go-tipb"
	"go.uber.org/zap"
)

// MPPExecWithRetry receive mppResponse from localMppCoordinator,
// and tries to recovery mpp err if necessary.
// The abstraction layer of reading mpp resp:
//  1. MPPGather: As part of the TiDB Volcano model executor, it is equivalent to a TableReader.
//  2. selectResult: Decode select result(mppResponse) into chunk. Also record runtime info.
//  3. MPPExecWithRetry: Recovery mpp err if possible and retry MPP Task.
//  4. localMppCoordinator: Generate MPP fragment and dispatch MPPTask.
//     And receive MPP status for better err msg and correct stats for Limit.
//  5. mppIterator: Send or receive MPP RPC.
type MPPExecWithRetry struct {
	mppIterator kv.Response
	sctx        sessionctx.Context
	is          infoschema.InfoSchema
	plan        plannercore.PhysicalPlan
	ctx         context.Context
	memTracker  *memory.Tracker
	// mppErrRecovery is designed for the recovery of MPP errors.
	// Basic idea:
	// 1. It attempts to hold the results of MPP. During the holding process, if an error occurs, it starts error recovery.
	//    If the recovery is successful, it discards held results and reconstructs the respIter, then re-executes the MPP task.
	//    If the recovery fails, an error is reported directly.
	// 2. If the held MPP results exceed the capacity, will starts returning results to caller.
	//    Once the results start being returned, error recovery cannot be performed anymore.
	mppErrRecovery *RecoveryHandler
	planIDs        []int
	// Expose to let MPPGather access.
	KVRanges []kv.KeyRange
	queryID  kv.MPPQueryID
	startTS  uint64
	gatherID uint64
	nodeCnt  int
}

var _ kv.Response = &MPPExecWithRetry{}

// NewMPPExecWithRetry create MPPExecWithRetry.
func NewMPPExecWithRetry(ctx context.Context, sctx sessionctx.Context, parentTracker *memory.Tracker, planIDs []int,
	plan plannercore.PhysicalPlan, startTS uint64, queryID kv.MPPQueryID,
	is infoschema.InfoSchema) (*MPPExecWithRetry, error) {
	// TODO: After add row info in tipb.DataPacket, we can use row count as capacity.
	// For now, use the number of tipb.DataPacket as capacity.
	const holdCap = 2

	disaggTiFlashWithAutoScaler := config.GetGlobalConfig().DisaggregatedTiFlash && config.GetGlobalConfig().UseAutoScaler
	_, allowTiFlashFallback := sctx.GetSessionVars().AllowFallbackToTiKV[kv.TiFlash]

	// 1. For now, mpp err recovery only support MemLimit, which is only useful when AutoScaler is used.
	// 2. When enable fallback to tikv, the returned mpp err will be ErrTiFlashServerTimeout,
	//    which we cannot handle for now. Also there is no need to recovery because tikv will retry the query.
	// 3. For cached table, will not dispatch tasks to TiFlash, so no need to recovery.
	enableMPPRecovery := disaggTiFlashWithAutoScaler && !allowTiFlashFallback

	failpoint.Inject("mpp_recovery_test_mock_enable", func() {
		if !allowTiFlashFallback {
			enableMPPRecovery = true
		}
	})

	recoveryHandler := NewRecoveryHandler(disaggTiFlashWithAutoScaler,
		uint64(holdCap), enableMPPRecovery, parentTracker)
	memTracker := memory.NewTracker(parentTracker.Label(), 0)
	memTracker.AttachTo(parentTracker)
	retryer := &MPPExecWithRetry{
		ctx:            ctx,
		sctx:           sctx,
		memTracker:     memTracker,
		planIDs:        planIDs,
		is:             is,
		plan:           plan,
		startTS:        startTS,
		queryID:        queryID,
		mppErrRecovery: recoveryHandler,
	}

	var err error
	retryer.KVRanges, err = retryer.setupMPPIterator(ctx, false)
	return retryer, err
}

// Next implements kv.Response interface.
func (r *MPPExecWithRetry) Next(ctx context.Context) (resp kv.ResultSubset, err error) {
	if err = r.nextWithRecovery(ctx); err != nil {
		return nil, err
	}

	if r.mppErrRecovery.NumHoldResp() != 0 {
		if resp, err = r.mppErrRecovery.PopFrontResp(); err != nil {
			return nil, err
		}
	} else if resp, err = r.mppIterator.Next(ctx); err != nil {
		return nil, err
	}
	return resp, nil
}

// Close implements kv.Response interface.
func (r *MPPExecWithRetry) Close() (err error) {
	// Clear other resources even when mppIterator got err.
	err = r.mppIterator.Close()
	r.mppErrRecovery.ResetHolder()
	// Should close mppIterator before detach, because mppIterator may still using it.
	r.memTracker.Detach()
	return err
}

func (r *MPPExecWithRetry) setupMPPIterator(ctx context.Context, recoverying bool) ([]kv.KeyRange, error) {
	if recoverying {
		// Sanity check.
		if r.mppIterator == nil {
			return nil, errors.New("mpp iterator should not be nil when recoverying")
		}
		if err := r.mppIterator.Close(); err != nil {
			return nil, err
		}
	}

	// Make sure gatherID is updated before dispatch mpp tasks.
	r.gatherID = allocMPPGatherID(r.sctx)

	mppIterator, kvRanges, nodeInfo, err := r.dispatchMPPTasks()
	if err != nil {
		return nil, err
	}
	if r.nodeCnt = len(nodeInfo); r.nodeCnt <= 0 {
		return nil, errors.Errorf("tiflash node count should be greater than zero: %v", r.nodeCnt)
	}
	r.mppIterator = mppIterator

	return kvRanges, err
}

func (r *MPPExecWithRetry) nextWithRecovery(ctx context.Context) error {
	if !r.mppErrRecovery.Enabled() {
		return nil
	}

	for r.mppErrRecovery.CanHoldResult() {
		resp, mppErr := r.mppIterator.Next(ctx)

		// Mock recovery n times.
		failpoint.Inject("mpp_recovery_test_max_err_times", func(forceErrCnt failpoint.Value) {
			forceErrCntInt := forceErrCnt.(int)
			if r.mppErrRecovery.RecoveryCnt() < uint32(forceErrCntInt) {
				mppErr = errors.New("mock mpp error")
			}
		})

		if mppErr != nil {
			recoveryErr := r.mppErrRecovery.Recovery(&RecoveryInfo{
				MPPErr:  mppErr,
				NodeCnt: r.nodeCnt,
			})

			// Mock recovery succeed, ignore no recovery handler err.
			failpoint.Inject("mpp_recovery_test_ignore_recovery_err", func() {
				if recoveryErr == nil {
					panic("mocked mpp err should got recovery err")
				}
				if strings.Contains(mppErr.Error(), "mock mpp error") && strings.Contains(recoveryErr.Error(), "no handler to recovery") {
					recoveryErr = nil
				}
			})

			logutil.BgLogger().Info("recovery mpp error done", zap.Any("mppErr", mppErr), zap.Any("recoveryErr", recoveryErr),
				zap.Any("recoveryCnt", r.mppErrRecovery.RecoveryCnt()), zap.Any("nodeCnt", r.nodeCnt),
				zap.Any("queryID", r.queryID), zap.Any("startTS", r.startTS), zap.Any("gatherID", r.gatherID))

			if recoveryErr != nil {
				return mppErr
			}

			if _, err := r.setupMPPIterator(r.ctx, true); err != nil {
				logutil.BgLogger().Error("setup resp iter when recovery mpp err failed", zap.Any("err", err))
				return mppErr
			}
			r.mppErrRecovery.ResetHolder()

			continue
		}

		if resp == nil {
			break
		}

		r.mppErrRecovery.HoldResult(resp)
	}

	failpoint.Inject("mpp_recovery_test_hold_size", func(num failpoint.Value) {
		// Note: this failpoint only execute once.
		curRows := r.mppErrRecovery.NumHoldResp()
		numInt := num.(int)
		if curRows != numInt {
			panic(fmt.Sprintf("unexpected holding rows, cur: %d", curRows))
		}
	})
	return nil
}

func allocMPPGatherID(ctx sessionctx.Context) uint64 {
	mppQueryInfo := &ctx.GetSessionVars().StmtCtx.MPPQueryInfo
	return mppQueryInfo.AllocatedGatherID.Add(1)
}

func (r *MPPExecWithRetry) dispatchMPPTasks() (kv.Response, []kv.KeyRange, map[string]bool, error) {
	sender, ok := r.plan.(*plannercore.PhysicalExchangeSender)
	if !ok {
		return nil, nil, nil, errors.Errorf("unexpected plan type, expect: PhysicalExchangeSender, got: %s", r.plan.TP())
	}
	frags, kvRanges, nodeInfo, err := plannercore.GenerateRootMPPTasks(r.sctx, r.gatherID, r.startTS, r.queryID, sender, r.is)
	if err != nil {
		return nil, nil, nil, err
	}

	var allReqs []*kv.MPPDispatchRequest
	for _, frag := range frags {
		fragReqs, err := r.appendMPPDispatchReq(frag)
		if err != nil {
			return nil, nil, nil, err
		}
		allReqs = append(allReqs, fragReqs...)
	}

	failpoint.Inject("checkTotalMPPTasks", func(val failpoint.Value) {
		if val.(int) != len(allReqs) {
			failpoint.Return(nil, nil, nil, errors.Errorf("The number of tasks is not right, expect %d tasks but actually there are %d tasks", val.(int), len(allReqs)))
		}
	})

	_, allowTiFlashFallback := r.sctx.GetSessionVars().AllowFallbackToTiKV[kv.TiFlash]
	mppIterator := r.sctx.GetMPPClient().DispatchMPPTasks(r.ctx, r.sctx.GetSessionVars().KVVars,
		allReqs, allowTiFlashFallback, r.startTS, r.queryID, r.sctx.GetSessionVars().ChooseMppVersion(),
		r.memTracker)
	return mppIterator, kvRanges, nodeInfo, nil
}

func (r *MPPExecWithRetry) appendMPPDispatchReq(pf *plannercore.Fragment) (mppReqs []*kv.MPPDispatchRequest, err error) {
	dagReq, err := constructDAGReq(r.sctx, []plannercore.PhysicalPlan{pf.ExchangeSender}, kv.TiFlash)
	if err != nil {
		return nil, errors.Trace(err)
	}
	for i := range pf.ExchangeSender.Schema().Columns {
		dagReq.OutputOffsets = append(dagReq.OutputOffsets, uint32(i))
	}
	if !pf.IsRoot {
		dagReq.EncodeType = tipb.EncodeType_TypeCHBlock
	} else {
		dagReq.EncodeType = tipb.EncodeType_TypeChunk
	}
	rgName := r.sctx.GetSessionVars().ResourceGroupName
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
			return nil, errors.Trace(err)
		}
		pbData, err := dagReq.Marshal()
		if err != nil {
			return nil, errors.Trace(err)
		}

		logutil.BgLogger().Info("Dispatch mpp task", zap.Uint64("timestamp", mppTask.StartTs),
			zap.Int64("ID", mppTask.ID), zap.Uint64("QueryTs", mppTask.MppQueryID.QueryTs), zap.Uint64("LocalQueryId", mppTask.MppQueryID.LocalQueryID),
			zap.Uint64("ServerID", mppTask.MppQueryID.ServerID), zap.String("address", mppTask.Meta.GetAddress()),
			zap.String("plan", plannercore.ToString(pf.ExchangeSender)),
			zap.Int64("mpp-version", mppTask.MppVersion.ToInt64()),
			zap.String("exchange-compression-mode", pf.ExchangeSender.CompressionMode.Name()),
			zap.String("ResourceGroup", rgName),
		)
		if mppTask.GatherID != r.gatherID {
			return nil, errors.Errorf("unexpected gather id for mpp task, expect %v, got %v", r.gatherID, mppTask.GatherID)
		}
		req := &kv.MPPDispatchRequest{
			Data:              pbData,
			Meta:              mppTask.Meta,
			GatherID:          mppTask.GatherID,
			ID:                mppTask.ID,
			IsRoot:            pf.IsRoot,
			Timeout:           10,
			SchemaVar:         r.is.SchemaMetaVersion(),
			StartTs:           r.startTS,
			MppQueryID:        mppTask.MppQueryID,
			State:             kv.MppTaskReady,
			ResourceGroupName: rgName,
		}
		mppReqs = append(mppReqs, req)
	}
	return mppReqs, nil
}
