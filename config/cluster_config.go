// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"github.com/pingcap/errors"
	"fmt"
	pd "github.com/pingcap/pd/client"

	"github.com/pingcap/kvproto/pkg/configpb"
	
	"sync"
)

type ClusterConfig struct {
	sync.RWMutex
	configClient pd.ConfigClient
	// component -> global version
	global map[string]uint64
	// component -> component ID -> local version
	locals map[string]map[string]uint64
}

func NewClusterConfig(etcdAddrs []string) (*ClusterConfig, error) {
	security := GetGlobalConfig().Security
	configCli, err := pd.NewConfigClient(etcdAddrs, pd.SecurityOption{
		CAPath:   security.ClusterSSLCA,
		CertPath: security.ClusterSSLCert,
		KeyPath:  security.ClusterSSLKey,
	})

	if err != nil {
		return nil, errors.Trace(err)
	}
	return &ClusterConfig{
		configClient: configCli,
		global:       make(map[string]uint64),
		locals:       make(map[string]map[string]uint64),
	}, nil
}

func (cc *ClusterConfig) GetConfigClient() pd.ConfigClient {
	return cc.configClient
}

func (cc *ClusterConfig) GlobalVersion(component string) uint64 {
	fmt.Println("!!!!!")
	cc.Lock()
	defer cc.Unlock()
	if v, ok := cc.global[component]; ok {
		return v
	}
	cc.global[component] = 0
	return cc.global[component]
}

func (cc *ClusterConfig) LocalVersion(component, componentID string) uint64 {
	cc.Lock()
	defer cc.Unlock()
	if l, ok := cc.locals[component]; ok {
		if v, ok1 := l[componentID]; ok1 {
			return v
		}
		l[componentID] = 0
		return l[componentID]
	}
	cc.locals[component] = make(map[string]uint64)
	cc.locals[component][componentID] = 0
	return cc.locals[component][componentID]
}

func (cc *ClusterConfig) SetVersion(component, componentID string, ver *configpb.Version) {
	cc.Lock()
	defer cc.Unlock()
	if componentID == "" {
		cc.global[component] = ver.GetGlobal()
		return
	}
	local:=cc.locals[component]
	local[componentID]= ver.GetLocal()
	return
}