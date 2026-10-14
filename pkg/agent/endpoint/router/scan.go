// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package router

import "strings"

// Scanning is byte-wise: every byte a convertor distinguishes ('/', '\n', and
// [0-9a-fA-F-]) is ASCII, and UTF-8 is self-synchronising, so no multi-byte
// sequence can contain one.

// scan returns the longest run the convertor consumes at the head of s, or -1
// when it cannot match at all.
func (k kind) scan(s string) int {
	switch k {
	case kindStr:
		if n := runTo(s, '/'); n > 0 {
			return n
		}
	case kindPath:
		return runTo(s, '\n')
	case kindInt:
		if n := digitRun(s); n > 0 {
			return n
		}
	case kindFloat:
		d := digitRun(s)
		if d == 0 {
			break
		}
		if d < len(s) && s[d] == '.' {
			if f := digitRun(s[d+1:]); f > 0 {
				return d + 1 + f
			}
		}
		return d
	case kindUUID:
		return uuidRun(s)
	}
	return -1
}

// next returns the next shorter viable run after prev, or -1 when prev was the
// last candidate. Candidates descend, so runs are enumerated greedily.
func (k kind) next(s string, prev int) int {
	switch k {
	case kindStr, kindInt:
		if prev > 1 {
			return prev - 1
		}
	case kindPath:
		if prev > 0 {
			return prev - 1
		}
	case kindFloat:
		// the integer part is a digit run from the head, so a '.' can only sit
		// at its end: viable runs are 1..d and d+1+1..d+1+f, never d+1
		d := digitRun(s)
		switch {
		case prev > d+2:
			return prev - 1
		case prev == d+2:
			return d
		case prev > 1:
			return prev - 1
		}
	case kindUUID:
		// every '-?' is forced by the input: skipping a present hyphen demands a
		// hex digit where the hyphen is, so the shape admits one length
	}
	return -1
}

// charset reports whether a shorter run of this convertor could be followed by
// c, which is what makes an edge ambiguous.
func (k kind) charset(c byte) bool {
	switch k {
	case kindStr:
		return c != '/'
	case kindPath:
		return c != '\n'
	case kindInt:
		return isDigit(c)
	case kindFloat:
		return isDigit(c) || c == '.'
	}
	return false
}

func runTo(s string, c byte) int {
	if i := strings.IndexByte(s, c); i >= 0 {
		return i
	}
	return len(s)
}

func digitRun(s string) int {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return i
}

func uuidRun(s string) int {
	i := 0
	for g, n := range [...]int{8, 4, 4, 4, 12} {
		if g > 0 && i < len(s) && s[i] == '-' {
			i++
		}
		for range n {
			if i >= len(s) || !isHex(s[i]) {
				return -1
			}
			i++
		}
	}
	return i
}
