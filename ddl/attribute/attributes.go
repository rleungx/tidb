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

// Label ...
type Label struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

// Labels is a slice of Labels.
type Labels []Label

// NewLabels ...
func NewLabels(attrs []string) (Labels, error) {
	labels := make(Labels, 0, len(attrs))
	for _, attr := range attrs {
		label, err := NewLabel(strings.TrimSpace(attr))
		if err != nil {
			return labels, err
		}

		err = labels.Add(label)
		if err != nil {
			return labels, err
		}
	}
	return labels, nil
}

// Restore ...
func (labels *Labels) Restore() (string, error) {
	var sb strings.Builder
	for i, label := range *labels {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('"')
		conStr, err := label.Restore()
		if err != nil {
			return "", err
		}
		sb.WriteString(conStr)
		sb.WriteByte('"')
	}
	return sb.String(), nil
}

// Add ...
func (labels *Labels) Add(l Label) error {
	pass := true
	for _, label := range *labels {
		if l.Value != label.Value {
			continue
		}
		pass = false
	}

	if pass {
		*labels = append(*labels, l)
	}
	return nil
}

// NewLabel ...
func NewLabel(attr string) (Label, error) {
	r := Label{}
	value := strings.TrimSpace(attr)
	r.Key = "attribute"
	r.Value = value
	return r, nil
}

// Restore converts a Constraint to a string.
func (a *Label) Restore() (string, error) {
	var sb strings.Builder
	sb.WriteString(a.Value)
	return sb.String(), nil
}
