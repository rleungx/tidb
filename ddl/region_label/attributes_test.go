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
	"errors"

	. "github.com/pingcap/check"
)

var _ = Suite(&testAttributeSuite{})

type testAttributeSuite struct{}

func (t *testAttributeSuite) TestNew(c *C) {
	type TestCase struct {
		name  string
		input string
		label Attribute
		err   error
	}
	tests := []TestCase{
		{
			name:  "normal",
			input: "nomerge",
			label: Attribute{
				Key:   "attribute",
				Value: "nomerge",
			},
		},
	}

	for _, t := range tests {
		label, err := NewAttribute(t.input)
		comment := Commentf("%s: %v", t.name, err)
		if t.err == nil {
			c.Assert(err, IsNil, comment)
			c.Assert(label, DeepEquals, t.label, comment)
		} else {
			c.Assert(errors.Is(err, t.err), IsTrue, comment)
		}
	}
}
