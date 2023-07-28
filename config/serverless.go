// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

// BootstrapControl contains ratelimit configuration options.
type BootstrapControl struct {
	SkipServerlessVariables bool `toml:"skip-serverless-variables" json:"skip-serverless-variables"`
	SkipRootPriv            bool `toml:"skip-root-priv" json:"skip-root-priv"`
	SkipCloudAdminPriv      bool `toml:"skip-cloud-admin-priv" json:"skip-cloud-admin-priv"`
	SkipRoleAdminPriv       bool `toml:"skip-role-admin-priv" json:"skip-role-admin-priv"`
	SkipPushdownBlacklist   bool `toml:"skip-pushdown-blacklist" json:"skip-pushdown-blacklist"`
}

// defaultBootstrapControl creates a new BootstrapControl.
func defaultBootstrapControl() BootstrapControl {
	return BootstrapControl{
		SkipServerlessVariables: false,
		SkipRootPriv:            false,
		SkipCloudAdminPriv:      false,
		SkipRoleAdminPriv:       false,
		SkipPushdownBlacklist:   false,
	}
}

// DefaultResourceGroup is the default resource group name for all txns and snapshots.
var DefaultResourceGroup string
