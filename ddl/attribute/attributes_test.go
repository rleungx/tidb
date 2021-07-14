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
	"errors"
	"testing"

	. "github.com/pingcap/check"
)

func TestT(t *testing.T) {
	TestingT(t)
}

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
		{
			name:  "normal with space",
			input: " nomerge ",
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

func (t *testAttributeSuite) TestRestore(c *C) {
	type TestCase struct {
		name   string
		input  Attribute
		output string
		err    error
	}
	var tests []TestCase

	input, err := NewAttribute("nomerge")
	c.Assert(err, IsNil)
	tests = append(tests, TestCase{
		name:   "normal",
		input:  input,
		output: "nomerge",
	})

	input, err = NewAttribute(" nomerge  ")
	c.Assert(err, IsNil)
	tests = append(tests, TestCase{
		name:   "normal with spaces",
		input:  input,
		output: "nomerge",
	})

	for _, t := range tests {
		output, err := t.input.Restore()
		comment := Commentf("%s: %v", t.name, err)
		if t.err == nil {
			c.Assert(err, IsNil, comment)
			c.Assert(output, Equals, t.output, comment)
		} else {
			c.Assert(errors.Is(err, t.err), IsTrue, comment)
		}
	}
}

var _ = Suite(&testAttributesSuite{})

type testAttributesSuite struct{}

func (t *testAttributesSuite) TestNew(c *C) {
	_, err := NewAttributes(nil)
	c.Assert(err, IsNil)

	_, err = NewAttributes([]string{})
	c.Assert(err, IsNil)

	attrs, err := NewAttributes([]string{"nomerge"})
	c.Assert(err, IsNil)
	c.Assert(attrs, HasLen, 1)
	c.Assert(attrs[0].Value, Equals, "nomerge")

	attrs, err = NewAttributes([]string{"nomerge", "somethingelse"})
	c.Assert(err, IsNil)
	c.Assert(attrs, HasLen, 2)
	c.Assert(attrs[0].Value, Equals, "nomerge")
	c.Assert(attrs[1].Value, Equals, "somethingelse")

	attrs, err = NewAttributes([]string{"nomerge", "nomerge"})
	c.Assert(err, IsNil)
	c.Assert(attrs, HasLen, 1)
	c.Assert(attrs[0].Value, Equals, "nomerge")
}

func (t *testAttributesSuite) TestAdd(c *C) {
	type TestCase struct {
		name   string
		labels Attributes
		label  Attribute
		err    error
	}
	var tests []TestCase

	labels, err := NewAttributes([]string{"nomerge"})
	c.Assert(err, IsNil)
	label, err := NewAttribute("somethingelse")
	c.Assert(err, IsNil)
	tests = append(tests, TestCase{
		"normal",
		labels, label,
		nil,
	})

	labels, err = NewAttributes([]string{"nomerge"})
	c.Assert(err, IsNil)
	label, err = NewAttribute("nomerge")
	c.Assert(err, IsNil)
	tests = append(tests, TestCase{
		"duplicated attributes, skip",
		labels, label,
		nil,
	})

	tests = append(tests, TestCase{
		"duplicated attributes, skip",
		append(labels, Attribute{
			Key:   "attribute",
			Value: "nomerge",
		}), label,
		nil,
	})

	for _, t := range tests {
		err := t.labels.Add(t.label)
		comment := Commentf("%s: %v", t.name, err)
		if t.err == nil {
			c.Assert(err, IsNil, comment)
			c.Assert(t.labels[len(t.labels)-1], DeepEquals, t.label, comment)
		} else {
			c.Assert(errors.Is(err, t.err), IsTrue, comment)
		}
	}
}

func (t *testAttributesSuite) TestRestore(c *C) {
	type TestCase struct {
		name   string
		input  Attributes
		output string
		err    error
	}
	var tests []TestCase

	tests = append(tests, TestCase{
		"normal1",
		Attributes{},
		"",
		nil,
	})

	input1, err := NewAttribute("nomerge")
	c.Assert(err, IsNil)
	input2, err := NewAttribute("somethingelse")
	c.Assert(err, IsNil)
	tests = append(tests, TestCase{
		"normal2",
		Attributes{input1, input2},
		`"nomerge","somethingelse"`,
		nil,
	})

	for _, t := range tests {
		res, err := t.input.Restore()
		comment := Commentf("%s: %v", t.name, err)
		if t.err == nil {
			c.Assert(err, IsNil, comment)
			c.Assert(res, Equals, t.output, comment)
		} else {
			c.Assert(errors.Is(err, t.err), IsTrue, comment)
		}
	}
}
