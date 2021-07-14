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
	"strings"
)

// Attribute...
type Attribute struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

// Attributes is a slice of Attributes.
type Attributes []Attribute

// NewConstraints will check labels, and build Constraints for rule.
func NewAttributes(attrs []string) (Attributes, error) {
	attributes := make(Attributes, 0, len(attrs))
	for _, str := range attrs {
		attr, err := NewAttribute(strings.TrimSpace(str))
		if err != nil {
			return attributes, err
		}

		err = attributes.Add(attr)
		if err != nil {
			return attributes, err
		}
	}
	return attributes, nil
}

// Restore converts label constraints to a string.
func (attributes *Attributes) Restore() (string, error) {
	var sb strings.Builder
	for i, attribute := range *attributes {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('"')
		conStr, err := attribute.Restore()
		if err != nil {
			return "", err
		}
		sb.WriteString(conStr)
		sb.WriteByte('"')
	}
	return sb.String(), nil
}

// Add will add a new label constraint, with validation of all constraints.
// Note that Add does not validate one single constraint.
func (attributes *Attributes) Add(attr Attribute) error {
	pass := true
	for _, attribute := range *attributes {
		if attr.Value != attribute.Value {
			continue
		} else {
			pass = false
			continue
		}
	}

	if pass {
		*attributes = append(*attributes, attr)
	}
	return nil
}

// NewConstraint will create a Constraint from a string.
func NewAttribute(attr string) (Attribute, error) {
	r := Attribute{}
	value := strings.TrimSpace(attr)
	r.Key = "attribute"
	r.Value = value
	return r, nil
}

// Restore converts a Constraint to a string.
func (a *Attribute) Restore() (string, error) {
	var sb strings.Builder
	sb.WriteString(a.Value)
	return sb.String(), nil
}
