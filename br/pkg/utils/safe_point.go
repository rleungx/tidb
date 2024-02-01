// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package utils

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/keyspacepb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/log"
	berrors "github.com/pingcap/tidb/br/pkg/errors"
	tidbconfig "github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/gcutil"
	"github.com/tikv/client-go/v2/oracle"
	pd "github.com/tikv/pd/client"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	brServiceSafePointIDFormat      = "br-%s"
	preUpdateServiceSafePointFactor = 3
	checkGCSafePointGapTime         = 5 * time.Second
	// DefaultBRGCSafePointTTL means PD keep safePoint limit at least 5min.
	DefaultBRGCSafePointTTL = 5 * 60
	// DefaultCheckpointGCSafePointTTL means PD keep safePoint limit at least 72 minutes.
	DefaultCheckpointGCSafePointTTL = 72 * 60
	// DefaultStreamStartSafePointTTL specifies keeping the server safepoint 30 mins when start task.
	DefaultStreamStartSafePointTTL = 1800
	// DefaultStreamPauseSafePointTTL specifies Keeping the server safePoint at list 24h when pause task.
	DefaultStreamPauseSafePointTTL = 24 * 3600
)

// ServiceSafePoint is metadata of service safe point from a BR 'instance'.
type ServiceSafePoint struct {
	ID  string
	TTL int64
	TS  uint64
}

// MarshalLogObject implements zapcore.ObjectMarshaler.
func (sp ServiceSafePoint) MarshalLogObject(encoder zapcore.ObjectEncoder) error {
	encoder.AddString("ID", sp.ID)
	ttlDuration := time.Duration(sp.TTL) * time.Second
	encoder.AddString("TTL", ttlDuration.String())
	backupTime := oracle.GetTimeFromTS(sp.TS)
	encoder.AddString("ServiceTime", backupTime.String())
	encoder.AddUint64("ServiceTS", sp.TS)
	return nil
}

// getGCSafePoint returns the current gc safe point.
// TODO: Some cluster may not enable distributed GC.
func getGCSafePoint(ctx context.Context, pdClient pd.Client, keyspaceName string) (uint64, error) {
	if keyspaceName == "" {
		log.Info("To get GC safe point v1.")
		safePoint, err := pdClient.UpdateGCSafePoint(ctx, 0)
		if err != nil {
			return 0, errors.Trace(err)
		}
		return safePoint, nil
	}

	keyspaceID, keyspaceSafePointVersion, err := getKeyspaceMeta(ctx, pdClient, keyspaceName)
	if err != nil {
		return 0, errors.Trace(err)
	}

	if keyspaceSafePointVersion == tidbconfig.SafePointV2 {
		log.Info("To get GC safe point v2.", zap.String("keyspace-name", keyspaceName), zap.Uint32("keyspace-id", keyspaceID))
		return pdClient.UpdateGCSafePointV2(ctx, keyspaceID, 0)
	} else {
		log.Info("To get GC safe point v1.", zap.String("keyspace-name", keyspaceName), zap.Uint32("keyspace-id", keyspaceID))
		return pdClient.UpdateGCSafePoint(ctx, 0)
	}
}

// MakeSafePointID makes a unique safe point ID, for reduce name conflict.
func MakeSafePointID() string {
	return fmt.Sprintf(brServiceSafePointIDFormat, uuid.New())
}

// CheckGCSafePoint checks whether the ts is older than GC safepoint.
// Note: It ignores errors other than exceed GC safepoint.
func CheckGCSafePoint(ctx context.Context, pdClient pd.Client, ts uint64) error {
	keyspaceName := tidbconfig.GetGlobalConfig().KeyspaceName
	// TODO: use PDClient.GetGCSafePoint instead once PD client exports it.
	safePoint, err := getGCSafePoint(ctx, pdClient, keyspaceName)
	if err != nil {
		log.Warn("fail to get GC safe point", zap.Error(err))
		return nil
	}
	if ts <= safePoint {
		return errors.Annotatef(berrors.ErrBackupGCSafepointExceeded, "GC safepoint %d exceed TS %d", safePoint, ts)
	}
	return nil
}

// UpdateServiceSafePoint register TS to PD, to lock down TS as safePoint with TTL seconds.
func UpdateServiceSafePoint(ctx context.Context, pdClient pd.Client, sp ServiceSafePoint, keyspaceName string) error {
	log.Info("update PD safePoint limit with TTL", zap.Object("safePoint", sp), zap.Any("keyspaceName", keyspaceName))
	lastSafePoint, err := UpdatePdCliServiceSafePoint(ctx, pdClient, sp.ID, sp.TTL, sp.TS-1, keyspaceName)
	if lastSafePoint > sp.TS-1 {
		log.Warn("service GC safe point lost, we may fail to back up if GC lifetime isn't long enough",
			zap.Uint64("lastSafePoint", lastSafePoint),
			zap.Object("safePoint", sp),
		)
	}
	return errors.Trace(err)
}

func UpdatePdCliServiceSafePoint(ctx context.Context, pdClient pd.Client, serviceID string, ttl int64, safePoint uint64, keyspaceName string) (uint64, error) {
	if keyspaceName == "" {
		log.Info("use safe point v1.")
		return pdClient.UpdateServiceGCSafePoint(ctx, serviceID, ttl, safePoint)
	}

	keyspaceID, keyspaceSafePointVersion, err := getKeyspaceMeta(ctx, pdClient, keyspaceName)
	if err != nil {
		return 0, errors.Trace(err)
	}

	if keyspaceSafePointVersion == tidbconfig.SafePointV2 {
		log.Info("use safe point v2.", zap.String("keyspace-name", keyspaceName), zap.Uint32("keyspace-id", keyspaceID))
		return pdClient.UpdateServiceSafePointV2(ctx, keyspaceID, serviceID, ttl, safePoint)
	} else {
		log.Info("use safe point v1.", zap.String("keyspace-name", keyspaceName), zap.Uint32("keyspace-id", keyspaceID))
		return pdClient.UpdateServiceGCSafePoint(ctx, serviceID, ttl, safePoint)
	}

}

func getKeyspaceMeta(ctx context.Context, pdClient pd.Client, keyspaceName string) (uint32, string, error) {
	// Load Keyspace meta with retry.
	var keyspaceMeta *keyspacepb.KeyspaceMeta
	err := util.RunWithRetry(util.DefaultMaxRetries, util.RetryInterval, func() (bool, error) {
		var errInner error
		keyspaceMeta, errInner = pdClient.LoadKeyspace(ctx, keyspaceName)
		// Retry when pd not bootstrapped or if keyspace not exists.
		if IsNotBootstrappedError(errInner) || IsKeyspaceNotExistError(errInner) {
			return true, errInner
		}
		// Do not retry when success or encountered unexpected error.
		return false, errInner
	})
	if err != nil {
		return 0, "", err
	}

	return keyspaceMeta.Id, keyspaceMeta.Config[gcutil.SafePointVersion], nil

}

// IsNotBootstrappedError returns true if the error is pd not bootstrapped error.
func IsNotBootstrappedError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), pdpb.ErrorType_NOT_BOOTSTRAPPED.String())
}

// IsKeyspaceNotExistError returns true the error is caused by keyspace not exists.
func IsKeyspaceNotExistError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), pdpb.ErrorType_ENTRY_NOT_FOUND.String())
}

// StartServiceSafePointKeeper will run UpdateServiceSafePoint periodicity
// hence keeping service safepoint won't lose.
func StartServiceSafePointKeeper(
	ctx context.Context,
	pdClient pd.Client,
	sp ServiceSafePoint,
) error {
	if sp.ID == "" || sp.TTL <= 0 {
		return errors.Annotatef(berrors.ErrInvalidArgument, "invalid service safe point %v", sp)
	}
	keyspaceName := tidbconfig.GetGlobalConfig().KeyspaceName
	if err := CheckGCSafePoint(ctx, pdClient, sp.TS); err != nil {
		return errors.Trace(err)
	}
	// Update service safe point immediately to cover the gap between starting
	// update goroutine and updating service safe point.
	if err := UpdateServiceSafePoint(ctx, pdClient, sp, keyspaceName); err != nil {
		return errors.Trace(err)
	}

	// It would be OK since TTL won't be zero, so gapTime should > `0.
	updateGapTime := time.Duration(sp.TTL) * time.Second / preUpdateServiceSafePointFactor
	updateTick := time.NewTicker(updateGapTime)
	checkTick := time.NewTicker(checkGCSafePointGapTime)
	go func() {
		defer updateTick.Stop()
		defer checkTick.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Debug("service safe point keeper exited")
				return
			case <-updateTick.C:
				if err := UpdateServiceSafePoint(ctx, pdClient, sp, keyspaceName); err != nil {
					log.Warn("failed to update service safe point, backup may fail if gc triggered",
						zap.Error(err),
					)
				}
			case <-checkTick.C:
				if err := CheckGCSafePoint(ctx, pdClient, sp.TS); err != nil {
					log.Panic("cannot pass gc safe point check, aborting",
						zap.Error(err),
						zap.Object("safePoint", sp),
					)
				}
			}
		}
	}()
	return nil
}

type FakePDClient struct {
	pd.Client
	Stores []*metapb.Store
}

// GetAllStores return fake stores.
func (c FakePDClient) GetAllStores(context.Context, ...pd.GetStoreOption) ([]*metapb.Store, error) {
	return append([]*metapb.Store{}, c.Stores...), nil
}
