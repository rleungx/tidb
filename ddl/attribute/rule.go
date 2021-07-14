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
	"fmt"

	"github.com/pingcap/parser/ast"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/util/codec"
	"gopkg.in/yaml.v2"
)

// Rule is the rule to assign labels to a region.
type Rule struct {
	ID         string      `json:"id"`
	Attribules []Attribute `json:"labels"`
	RuleType   string      `json:"rule_type"`
	Rule       interface{} `json:"rule"`
}

func NewRule(id string) *Rule {
	return &Rule{
		ID: id,
	}
}

func (r *Rule) ApplyAttributesSpec(spec *ast.AttributesSpec) error {
	attrBytes := []byte("[" + spec.Attributes + "]")
	attributes := []string{}
	err := yaml.UnmarshalStrict(attrBytes, &attributes)
	if err == nil {
		attributes, err := NewAttributes(attributes)
		if err != nil {
			return err
		}
		r.Attribules = attributes
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

// Clone is used to duplicate a bundle.
func (r *Rule) Clone() *Rule {
	newRule := &Rule{}
	*newRule = *r
	return newRule
}

// Reset resets the bundle ID and keyrange of all rules.
func (r *Rule) Reset(newID int64) *Rule {
	r.ID = ID(newID)
	r.RuleType = "key-range"
	r.Rule = map[string]string{
		"start_key": hex.EncodeToString(codec.EncodeBytes(nil, tablecodec.GenTableRecordPrefix(newID))),
		"end_key":   hex.EncodeToString(codec.EncodeBytes(nil, tablecodec.GenTableRecordPrefix(newID+1))),
	}
	return r
}

func ID(id int64) string {
	return fmt.Sprintf("%d", id)
}
