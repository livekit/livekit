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

package endpoint

import (
	"fmt"
	"regexp"
	"strings"
)

// Template is a compiled starlette-style path template. The compilation mirrors
// starlette's compile_path exactly: {name} or {name:convertor} segments become the
// convertor's pattern, everything else is matched literally, anchored on both ends.
// Workers declare templates through FastAPI routes, so any divergence from
// starlette's semantics would make the server route requests the worker-side
// router then refuses.
type Template struct {
	raw string
	re  *regexp.Regexp
}

// convertor patterns copied verbatim from starlette's convertors.py. The uuid
// pattern deliberately makes every hyphen optional, so 32 bare hex characters
// match too.
var convertorPatterns = map[string]string{
	"str":   `[^/]+`,
	"path":  `.*`,
	"int":   `[0-9]+`,
	"float": `[0-9]+(?:\.[0-9]+)?`,
	"uuid":  `[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}`,
}

// starlette's PARAM_REGEX
var paramRegex = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)(:[a-zA-Z_][a-zA-Z0-9_]*)?\}`)

// ParseTemplate compiles a starlette path template. Custom convertors are
// rejected: only the five built-ins may travel over the wire.
func ParseTemplate(path string) (*Template, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("path template must start with '/': %q", path)
	}

	var pattern strings.Builder
	pattern.WriteString("^")

	idx := 0
	seen := map[string]bool{}
	for _, m := range paramRegex.FindAllStringSubmatchIndex(path, -1) {
		start, end := m[0], m[1]
		name := path[m[2]:m[3]]
		convertor := "str"
		if m[4] != -1 {
			convertor = path[m[4]+1 : m[5]] // skip the ':'
		}
		convPattern, ok := convertorPatterns[convertor]
		if !ok {
			return nil, fmt.Errorf("unknown path convertor %q in template %q", convertor, path)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicated param name %q in template %q", name, path)
		}
		seen[name] = true

		pattern.WriteString(regexp.QuoteMeta(path[idx:start]))
		pattern.WriteString("(?:")
		pattern.WriteString(convPattern)
		pattern.WriteString(")")
		idx = end
	}
	pattern.WriteString(regexp.QuoteMeta(path[idx:]))
	pattern.WriteString("$")

	re, err := regexp.Compile(pattern.String())
	if err != nil {
		return nil, fmt.Errorf("invalid path template %q: %w", path, err)
	}
	return &Template{raw: path, re: re}, nil
}

func (t *Template) Match(path string) bool {
	return t.re.MatchString(path)
}

func (t *Template) String() string {
	return t.raw
}
