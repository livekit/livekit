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

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// kind is what one element of a template consumes. The convertor set is part
// of the wire contract and is closed.
type kind uint8

const (
	kindLiteral kind = iota
	kindStr          // [^/]+
	kindPath         // .*, which excludes '\n'
	kindInt          // [0-9]+
	kindFloat        // [0-9]+(?:\.[0-9]+)?
	kindUUID         // 8-4-4-4-12 hex, every hyphen optional
)

func convertorKind(name string) (kind, bool) {
	switch name {
	case "str":
		return kindStr, true
	case "path":
		return kindPath, true
	case "int":
		return kindInt, true
	case "float":
		return kindFloat, true
	case "uuid":
		return kindUUID, true
	}
	return 0, false
}

// element is one step of a parsed template: literal bytes to match exactly, or
// a convertor to consume.
type element struct {
	kind kind
	lit  string // kindLiteral only
}

// Template is a parsed starlette-style path template. Parsing mirrors
// starlette's compile_path exactly.
type Template struct {
	// the template as declared
	raw      string
	elements []element
}

// String returns the template as declared.
func (t *Template) String() string { return t.raw }

// Canonical returns a key identifying what the template matches: two templates
// with the same canonical form accept exactly the same paths and differ only in
// param names. Literals are length-prefixed so that a literal brace cannot forge
// a convertor - "/x/{:str}" is literal text, and must not collide with "/x/{a}".
func (t *Template) Canonical() string {
	var b strings.Builder
	for _, e := range t.elements {
		if e.kind == kindLiteral {
			b.WriteByte('L')
			b.WriteString(strconv.Itoa(len(e.lit)))
			b.WriteByte(':')
			b.WriteString(e.lit)
			continue
		}
		b.WriteByte('K')
		b.WriteString(strconv.Itoa(int(e.kind)))
	}
	return b.String()
}

// Ambiguous reports whether the template's own shape can force the matcher to
// backtrack - a param a following literal can extend, adjacent params, or a
// non-final path convertor - independent of what other templates put in the
// tree.
func (t *Template) Ambiguous() bool {
	for i, e := range t.elements {
		if e.kind == kindLiteral || e.kind == kindUUID {
			continue
		}
		if i == len(t.elements)-1 {
			continue // terminal: only the greedy run can reach the end
		}
		next := t.elements[i+1]
		if next.kind != kindLiteral || e.kind.charset(next.lit[0]) {
			return true
		}
	}
	return false
}

// ParseTemplate parses a starlette path template. Custom convertors are
// rejected: only the five built-ins may travel over the wire.
func ParseTemplate(path string) (*Template, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("path template must start with '/': %q", path)
	}
	// literals are compared byte-wise, which agrees with rune-wise semantics
	// only for valid UTF-8
	if !utf8.ValidString(path) {
		return nil, fmt.Errorf("path template is not valid UTF-8: %q", path)
	}

	t := &Template{raw: path}
	seen := map[string]struct{}{}
	lit := 0
	for i := 0; i < len(path); {
		name, convertor, end, ok := scanParam(path, i)
		if !ok {
			i++
			continue
		}
		k, ok := convertorKind(convertor)
		if !ok {
			return nil, fmt.Errorf("unknown path convertor %q in template %q", convertor, path)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("duplicated param name %q in template %q", name, path)
		}
		seen[name] = struct{}{}

		if i > lit {
			t.elements = append(t.elements, element{kind: kindLiteral, lit: path[lit:i]})
		}
		t.elements = append(t.elements, element{kind: k})
		i, lit = end, end
	}
	if lit < len(path) {
		t.elements = append(t.elements, element{kind: kindLiteral, lit: path[lit:]})
	}
	return t, nil
}

// scanParam matches starlette's PARAM_REGEX at i:
//
//	{([a-zA-Z_][a-zA-Z0-9_]*)(:[a-zA-Z_][a-zA-Z0-9_]*)?}
//
// Everything between matches is a literal, so a brace that does not open a
// well-formed param is an ordinary literal character.
func scanParam(s string, i int) (name, convertor string, end int, ok bool) {
	if s[i] != '{' {
		return "", "", 0, false
	}
	j := i + 1
	n := scanIdent(s, j)
	if n == j {
		return "", "", 0, false
	}
	name, j = s[j:n], n

	convertor = "str"
	if j < len(s) && s[j] == ':' {
		c := scanIdent(s, j+1)
		if c == j+1 {
			return "", "", 0, false
		}
		convertor, j = s[j+1:c], c
	}
	if j >= len(s) || s[j] != '}' {
		return "", "", 0, false
	}
	return name, convertor, j + 1, true
}

func scanIdent(s string, i int) int {
	if i >= len(s) || !isIdentStart(s[i]) {
		return i
	}
	j := i + 1
	for j < len(s) && (isIdentStart(s[j]) || isDigit(s[j])) {
		j++
	}
	return j
}

func isAlpha(c byte) bool      { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return isAlpha(c) || c == '_' }

func isHex(c byte) bool {
	return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}
