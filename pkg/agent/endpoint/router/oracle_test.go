// Copyright 2026 LiveKit, Inc.

package router

import (
	"fmt"
	"regexp"
	"strings"
)

// One compiled regex per route, scanned linearly. Transcribed from starlette's
// compile_path and carrying its own parser, so it references the hand-rolled
// scanner as well as the trie.

var oracleConvertors = map[string]string{
	"str":   `[^/]+`,
	"path":  `.*`,
	"int":   `[0-9]+`,
	"float": `[0-9]+(?:\.[0-9]+)?`,
	"uuid":  `[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}`,
}

// starlette's PARAM_REGEX
var oracleParamRegex = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)(:[a-zA-Z_][a-zA-Z0-9_]*)?\}`)

func oracleCompile(path string) (*regexp.Regexp, error) {
	var pattern strings.Builder
	pattern.WriteString("^")

	idx := 0
	for _, m := range oracleParamRegex.FindAllStringSubmatchIndex(path, -1) {
		start, end := m[0], m[1]
		convertor := "str"
		if m[4] != -1 {
			convertor = path[m[4]+1 : m[5]]
		}
		p, ok := oracleConvertors[convertor]
		if !ok {
			return nil, fmt.Errorf("unknown convertor %q", convertor)
		}
		pattern.WriteString(regexp.QuoteMeta(path[idx:start]))
		pattern.WriteString("(?:")
		pattern.WriteString(p)
		pattern.WriteString(")")
		idx = end
	}
	pattern.WriteString(regexp.QuoteMeta(path[idx:]))
	// python's '$' is Go's `\n?\z`
	pattern.WriteString("\n?$")

	return regexp.Compile(pattern.String())
}

type oracleRoute struct {
	re   *regexp.Regexp
	raw  string
	mask Mask
}

type oracle struct{ routes []oracleRoute }

func (o *oracle) add(raw string, mask Mask) error {
	re, err := oracleCompile(raw)
	if err != nil {
		return err
	}
	o.routes = append(o.routes, oracleRoute{re: re, raw: raw, mask: mask})
	return nil
}

func (o *oracle) match(path string, q Mask) (string, Result) {
	partial := false
	for _, r := range o.routes {
		if !r.re.MatchString(path) {
			continue
		}
		if r.mask&q != 0 {
			return r.raw, ResultFull
		}
		partial = true
	}
	if partial {
		return "", ResultPartial
	}
	return "", ResultNone
}
