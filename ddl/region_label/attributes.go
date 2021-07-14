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

package label

import (
	"fmt"
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
func NewAttributes(labels []string) (Attributes, error) {
	attributes := make(Attributes, 0, len(labels))
	for _, str := range labels {
		label, err := NewAttribute(strings.TrimSpace(str))
		if err != nil {
			return attributes, err
		}

		err = attributes.Add(label)
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
func (attributes *Attributes) Add(label Attribute) error {
	pass := true

	for _, cnst := range *attributes {
		res := label.CompatibleWith(&cnst)
		if res == ConstraintCompatible {
			continue
		}
		if res == ConstraintDuplicated {
			pass = false
			continue
		}
		s1, err := label.Restore()
		if err != nil {
			s1 = err.Error()
		}
		s2, err := cnst.Restore()
		if err != nil {
			s2 = err.Error()
		}
		return fmt.Errorf("%w: '%s' and '%s'", ErrConflictingConstraints, s1, s2)
	}

	if pass {
		*attributes = append(*attributes, label)
	}
	return nil
}

// NewConstraint will create a Constraint from a string.
func NewAttribute(label string) (Attribute, error) {
	r := Attribute{}

	if len(label) < 4 {
		return r, fmt.Errorf("%w: %s", ErrInvalidConstraintFormat, label)
	}

	kv := strings.Split(label[1:], "=")
	if len(kv) != 2 {
		return r, fmt.Errorf("%w: %s", ErrInvalidConstraintFormat, label)
	}

	val := strings.TrimSpace(kv[1])
	if val == "" {
		return r, fmt.Errorf("%w: %s", ErrInvalidConstraintFormat, label)
	}

	r.Key = "attribute"
	r.Value = val
	return r, nil
}

// Restore converts a Constraint to a string.
func (a *Attribute) Restore() (string, error) {
	var sb strings.Builder
	if len(a.Value) != 1 {
		return "", fmt.Errorf("%w: constraint should have exactly one label value, got %v", ErrInvalidConstraintFormat, c.Values)
	}
	sb.WriteString(a.Key)
	sb.WriteString("=")
	sb.WriteString(a.Value)
	return sb.String(), nil
}
