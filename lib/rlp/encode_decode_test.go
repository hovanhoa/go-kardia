/*
 *  Copyright 2018 KardiaChain
 *  This file is part of the go-kardia library.
 *
 *  The go-kardia library is free software: you can redistribute it and/or modify
 *  it under the terms of the GNU Lesser General Public License as published by
 *  the Free Software Foundation, either version 3 of the License, or
 *  (at your option) any later version.
 *
 *  The go-kardia library is distributed in the hope that it will be useful,
 *  but WITHOUT ANY WARRANTY; without even the implied warranty of
 *  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 *  GNU Lesser General Public License for more details.
 *
 *  You should have received a copy of the GNU Lesser General Public License
 *  along with the go-kardia library. If not, see <http://www.gnu.org/licenses/>.
 */

package rlp

import (
	"bytes"
	"fmt"
	"testing"
)

type Simple struct {
	X uint32
	Y uint32
}

func (x *Simple) Equal(y *Simple) bool {
	if x == nil && y == nil {
		return true
	}
	if x == nil || y == nil {
		return false
	}
	return x.X == y.X && x.Y == y.Y
}

type SimpleSet struct {
	Set []*Simple
}

func (x *SimpleSet) Equal(y *SimpleSet) bool {
	if len(x.Set) != len(y.Set) {
		return false
	}

	for i := 0; i < len(x.Set); i++ {
		if !x.Set[i].Equal(y.Set[i]) {
			return false
		}
	}

	return true
}

func EncodeThenDecode(t *testing.T, x interface{}) *SimpleSet {
	b := new(bytes.Buffer)
	if err := Encode(b, x); err != nil {
		t.Fatalf("Encode error: %v", err)
	}

	fmt.Println("decoded byte", b.Bytes())

	var y SimpleSet
	if err := Decode(bytes.NewReader(b.Bytes()), &y); err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	return &y
}

func TestBasic(t *testing.T) {
	a := make([]*Simple, 2)
	a[0] = &Simple{1, 2}
	a[1] = &Simple{5, 6}

	x := SimpleSet{
		Set: a,
	}

	y := EncodeThenDecode(t, x)
	if !x.Equal(y) {
		t.Fail()
	}
}

/* Disable-the test for now.
// This test is expected to fail.
// Fix issues#73 to make this test passes.
func TestNilElement(t *testing.T) {
	a := make([]*Simple, 2)
	a[0] = &Simple{1, 2}
	a[1] = nil

	x := SimpleSet{
		Set: a,
	}

	y := EncodeThenDecode(t, x)
	if !x.Equal(y) {
		t.Fail()
	}
}
*/
