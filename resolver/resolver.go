// resolver.go
// ${facets:<type>.<name>.out.<path...>} reference parsing and resolution:
// finding refs in text, the $$-escape convention, and resolving every ref in
// a multi-document YAML manifest stream against a Facets control plane via a
// LookupFunc.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var refPattern = regexp.MustCompile(`\$\{facets:([^}]*)\}`)

// Ref is one ${facets:<type>.<name>.out.<path...>} reference.
type Ref struct {
	Raw          string // the full ${facets:...} match
	ResourceType string
	ResourceName string
	Path         []string // segments after "out", e.g. ["attributes","queue_url"]
}

// Find returns all refs in s in order of appearance. A ref preceded by an
// extra '$' ($${facets:...}) is an escape and is skipped. Malformed refs are
// errors — fail closed rather than passing them through to manifests.
func Find(s string) ([]Ref, error) {
	var out []Ref
	var errs []string
	for _, m := range refPattern.FindAllStringSubmatchIndex(s, -1) {
		start := m[0]
		if start > 0 && s[start-1] == '$' {
			continue // escaped
		}
		raw := s[m[0]:m[1]]
		expr := s[m[2]:m[3]]
		parts := strings.Split(expr, ".")
		if len(parts) < 4 || parts[2] != "out" {
			errs = append(errs, fmt.Sprintf("invalid facets ref %q: want ${facets:<type>.<name>.out.<path...>}", raw))
			continue
		}
		// Check that no segment is empty.
		hasEmpty := false
		for _, part := range parts {
			if part == "" {
				hasEmpty = true
				break
			}
		}
		if hasEmpty {
			errs = append(errs, fmt.Sprintf("invalid facets ref %q: want ${facets:<type>.<name>.out.<path...>}", raw))
			continue
		}
		out = append(out, Ref{Raw: raw, ResourceType: parts[0], ResourceName: parts[1], Path: parts[3:]})
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}

// Unescape rewrites $${facets:...} escapes to their literal form.
func Unescape(s string) string {
	return strings.ReplaceAll(s, "$${facets:", "${facets:")
}

// LookupFunc resolves one ref to its output value (as decoded JSON: string,
// float64, bool, map[string]any, []any).
type LookupFunc func(Ref) (any, error)

// ResolveStream substitutes every ${facets:...} ref across a multi-document
// YAML stream (e.g. rendered manifests, "---"-separated). Each document is
// decoded and walked independently (typed whole-scalar injection, embedded
// stringification, anchor preservation — see walk/resolveScalar below), but
// errors are aggregated across ALL documents in the stream — never a
// partially substituted stream. Document order is preserved in the output,
// "---"-separated the same way the source was. Non-map scalar documents, and
// null/empty documents (e.g. a stray leading/trailing "---"), pass through
// unchanged.
func ResolveStream(stream []byte, lookup LookupFunc) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(stream))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("parsing rendered manifest stream: %w", err)
		}
		docs = append(docs, &doc)
	}

	var errs []error
	for _, d := range docs {
		walk(d, lookup, &errs)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			_ = enc.Close()
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func walk(n *yaml.Node, lookup LookupFunc, errs *[]error) {
	if n.Kind == yaml.ScalarNode {
		resolveScalar(n, lookup, errs)
		return
	}
	for _, c := range n.Content {
		walk(c, lookup, errs)
	}
}

func resolveScalar(n *yaml.Node, lookup LookupFunc, errs *[]error) {
	found, err := Find(n.Value)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("line %d: %w", n.Line, err))
		return
	}
	if len(found) == 0 {
		n.Value = Unescape(n.Value)
		return
	}
	// Whole-scalar single ref: inject typed value.
	if len(found) == 1 && strings.TrimSpace(n.Value) == found[0].Raw {
		v, err := lookup(found[0])
		if err != nil {
			*errs = append(*errs, fmt.Errorf("line %d: %s: %w", n.Line, found[0].Raw, err))
			return
		}
		var enc yaml.Node
		if err := enc.Encode(v); err != nil {
			*errs = append(*errs, fmt.Errorf("line %d: encoding value for %s: %w", n.Line, found[0].Raw, err))
			return
		}
		// Preserve YAML metadata (anchors, aliases, comments) from original node.
		enc.Anchor = n.Anchor
		enc.HeadComment = n.HeadComment
		enc.LineComment = n.LineComment
		enc.FootComment = n.FootComment
		*n = enc
		return
	}
	// Embedded ref(s): stringify scalars in place.
	s := n.Value
	for _, r := range found {
		v, err := lookup(r)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("line %d: %s: %w", n.Line, r.Raw, err))
			continue
		}
		switch v.(type) {
		case string, float64, float32, int, int64, bool:
			s = strings.Replace(s, r.Raw, fmt.Sprintf("%v", v), 1)
		default:
			*errs = append(*errs, fmt.Errorf("line %d: %s resolves to a non-scalar value and cannot be embedded in a string", n.Line, r.Raw))
		}
	}
	n.SetString(Unescape(s))
}
