// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package utils_test

import (
	"context"
	"sync"
	"testing"

	"github.com/pingcap/tidb/br/pkg/utils"
	"github.com/stretchr/testify/require"
	pd "github.com/tikv/pd/client"
)

func TestCheckGCSafepoint(t *testing.T) {
	ctx := context.Background()
	pdClient := &mockSafePoint{safepoint: 2333}
	{
		err := utils.CheckGCSafePoint(ctx, pdClient, 2333+1)
		require.NoError(t, err)
	}
	{
		err := utils.CheckGCSafePoint(ctx, pdClient, 2333)
		require.Error(t, err)
	}
	{
		err := utils.CheckGCSafePoint(ctx, pdClient, 2333-1)
		require.Error(t, err)
	}
	{
		err := utils.CheckGCSafePoint(ctx, pdClient, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "GC safepoint 2333 exceed TS 0")
	}
}

type mockSafePoint struct {
	sync.Mutex
	pd.Client
	safepoint           uint64
	minServiceSafepoint uint64
}

func (m *mockSafePoint) UpdateServiceGCSafePoint(ctx context.Context, serviceID string, ttl int64, safePoint uint64) (uint64, error) {
	m.Lock()
	defer m.Unlock()

	if m.safepoint > safePoint {
		return m.safepoint, nil
	}
	if m.minServiceSafepoint == 0 || m.minServiceSafepoint > safePoint {
		m.minServiceSafepoint = safePoint
	}
	return m.minServiceSafepoint, nil
}

func (m *mockSafePoint) UpdateGCSafePoint(ctx context.Context, safePoint uint64) (uint64, error) {
	m.Lock()
	defer m.Unlock()

	if m.safepoint < safePoint && safePoint < m.minServiceSafepoint {
		m.safepoint = safePoint
	}
	return m.safepoint, nil
}

func TestStartServiceSafePointKeeper(t *testing.T) {
	pdClient := &mockSafePoint{safepoint: 2333}

	cases := []struct {
		sp utils.ServiceSafePoint
		ok bool
	}{
		{
			utils.ServiceSafePoint{
				ID:  "br",
				TTL: 10,
				TS:  2333 + 1,
			},
			true,
		},

		// Invalid TTL.
		{
			utils.ServiceSafePoint{
				ID:  "br",
				TTL: 0,
				TS:  2333 + 1,
			}, false,
		},

		// Invalid ID.
		{
			utils.ServiceSafePoint{
				ID:  "",
				TTL: 0,
				TS:  2333 + 1,
			},
			false,
		},

		// TS is too small.
		{
			utils.ServiceSafePoint{
				ID:  "br",
				TTL: 10,
				TS:  2333,
			}, false,
		},
		{
			utils.ServiceSafePoint{
				ID:  "br",
				TTL: 10,
				TS:  2333 - 1,
			},
			false,
		},
	}
	for i, cs := range cases {
		ctx, cancel := context.WithCancel(context.Background())
		err := utils.StartServiceSafePointKeeper(ctx, pdClient, cs.sp)
		if cs.ok {
			require.NoErrorf(t, err, "case #%d, %v", i, cs)
		} else {
			require.Errorf(t, err, "case #%d, %v", i, cs)
		}
		cancel()
	}
}
