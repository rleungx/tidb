// Copyright 2021 PingCAP, Inc.
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

package attribute

import (
	"encoding/hex"
	"encoding/json"

	"github.com/pingcap/parser/ast"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/util/codec"
	"gopkg.in/yaml.v2"
)

const idPrefix = "schema"

// Rule is the rule to assign labels to a region.
type Rule struct {
	ID       string      `json:"id"`
	Labels   Labels      `json:"labels"`
	RuleType string      `json:"rule_type"`
	Rule     interface{} `json:"rule"`
}

// NewRule ...
func NewRule(m map[string]string) *Rule {
	id := idPrefix
	if v, ok := m["db"]; ok {
		id = id + "/" + v
	}

	if v, ok := m["table"]; ok {
		id = id + "/" + v
	}

	if v, ok := m["partition"]; ok {
		id = id + "/" + v
	}

	return &Rule{
		ID: id,
	}
}

// ApplyAttributesSpec ...
func (r *Rule) ApplyAttributesSpec(spec *ast.AttributesSpec) error {
	attrBytes := []byte("[" + spec.Attributes + "]")
	attributes := []string{}
	err := yaml.UnmarshalStrict(attrBytes, &attributes)
	if err == nil {
		labels, err := NewLabels(attributes)
		if err != nil {
			return err
		}
		r.Labels = labels
		return nil
	}
	return nil
}

// String implements fmt.Stringer.
func (r *Rule) String() string {
	t, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(t)
}

// Clone ...
func (r *Rule) Clone() *Rule {
	newRule := &Rule{}
	*newRule = *r
	return newRule
}

// Reset ...
func (r *Rule) Reset(id int64, m map[string]string) *Rule {
	r.ID = idPrefix
	if v, ok := m["db"]; ok {
		r.Labels = append(r.Labels, Label{Key: "db", Value: v})
		r.ID = r.ID + "/" + v
	}

	if v, ok := m["table"]; ok {
		r.Labels = append(r.Labels, Label{Key: "table", Value: v})
		r.ID = r.ID + "/" + v
	}

	if v, ok := m["partition"]; ok {
		r.Labels = append(r.Labels, Label{Key: "partition", Value: v})
		r.ID = r.ID + "/" + v
	}

	r.RuleType = "key-range"
	r.Rule = map[string]string{
		"start_key": hex.EncodeToString(codec.EncodeBytes(nil, tablecodec.GenTableRecordPrefix(id))),
		"end_key":   hex.EncodeToString(codec.EncodeBytes(nil, tablecodec.GenTableRecordPrefix(id+1))),
	}
	return r
}
