// resolver.go
// ${facets:...} reference parsing and resolution, in two grammars —
// resource output (${facets:<type>.<name>.out.<path...>}) and blueprint-
// scoped (${facets:blueprint.self.<variables|secrets|artifacts>.<name>},
// see the Ref/RefKind doc comments below — finding refs in text, the
// $$-escape convention, and resolving every ref in a multi-document YAML
// manifest stream against a Facets control plane via a LookupFunc.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// refPattern requires the inner expression to match the type.name.out.path...
// grammar exactly: one or more [A-Za-z0-9_-] segments joined by dots, at
// least two segments. This is intentionally stricter than "anything but a
// closing brace" — an unterminated "${facets:" sequence, or one containing a
// character outside that set (a typo, stray punctuation, an empty segment
// from a doubled/trailing/leading dot), simply won't match at all, rather
// than being captured and rejected by Find's own arity/"out" checks below.
// That's a deliberate trade: it stops well-formed-looking-but-wrong refs
// from being silently misparsed, but it also means garbage no longer
// self-reports through Find's own error path — see findUnresolvedRef, the
// fail-closed net that catches exactly that gap downstream.
var refPattern = regexp.MustCompile(`\$\{facets:([A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)+)\}`)

// RefKind distinguishes the two ref grammars Find recognizes. The zero
// value, RefKindOutput, is deliberately the common <type>.<name>.out.<path>
// resource-output form, so existing Ref literals that don't set Kind
// (throughout this codebase's own tests) keep meaning exactly what they
// meant before blueprint-scoped refs existed.
type RefKind int

const (
	RefKindOutput            RefKind = iota // <type>.<name>.out.<path...>
	RefKindBlueprintVariable                // blueprint.self.variables.<name>
	RefKindBlueprintSecret                  // blueprint.self.secrets.<name>
	RefKindBlueprintArtifact                // blueprint.self.artifacts.<name>
)

// Ref is one ${facets:...} reference, in one of two grammars:
//
//   - Resource output: ${facets:<type>.<name>.out.<path...>} (Kind ==
//     RefKindOutput; ResourceType, ResourceName, Path are populated).
//   - Blueprint-scoped: ${facets:blueprint.self.<class>.<name>}, where
//     <class> is variables/secrets/artifacts (Kind ==
//     RefKindBlueprint{Variable,Secret,Artifact}; Name is populated). "self"
//     always means the same project/environment the render's other refs
//     resolve against — there is no cross-project or cross-environment
//     blueprint ref.
type Ref struct {
	Raw          string // the full ${facets:...} match
	Kind         RefKind
	ResourceType string   // RefKindOutput only
	ResourceName string   // RefKindOutput only
	Path         []string // RefKindOutput only: segments after "out", e.g. ["attributes","queue_url"]
	Name         string   // blueprint-scoped kinds only: the variable/secret/artifact name
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

		// blueprint.self.<class>.<name> — a reserved prefix, checked before
		// the resource-output grammar below, so e.g.
		// ${facets:blueprint.self.out.x} is rejected as an invalid
		// blueprint.self ref rather than treated as a (nonsensical) output
		// ref for a resource literally named type "blueprint", name "self".
		if len(parts) >= 2 && parts[0] == "blueprint" && parts[1] == "self" {
			if len(parts) != 4 {
				errs = append(errs, fmt.Sprintf("invalid facets ref %q: want ${facets:blueprint.self.<variables|secrets|artifacts>.<name>}", raw))
				continue
			}
			var kind RefKind
			switch parts[2] {
			case "variables":
				kind = RefKindBlueprintVariable
			case "secrets":
				kind = RefKindBlueprintSecret
			case "artifacts":
				kind = RefKindBlueprintArtifact
			default:
				errs = append(errs, fmt.Sprintf("invalid facets ref %q: blueprint.self class must be one of variables, secrets, artifacts (got %q)", raw, parts[2]))
				continue
			}
			if parts[3] == "" {
				errs = append(errs, fmt.Sprintf("invalid facets ref %q: want ${facets:blueprint.self.<variables|secrets|artifacts>.<name>}", raw))
				continue
			}
			out = append(out, Ref{Raw: raw, Kind: kind, Name: parts[3]})
			continue
		}

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
		out = append(out, Ref{Raw: raw, Kind: RefKindOutput, ResourceType: parts[0], ResourceName: parts[1], Path: parts[3:]})
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

// findUnresolvedRef is the fail-closed net for refPattern's stricter grammar
// (see its doc comment): it scans s for a literal "${facets:" occurrence
// that is neither part of a ref Find already matched and resolved (the
// caller only calls this on what's left after that) nor a legitimate
// $$-escape (a doubled $ immediately before it, which Unescape is meant to
// turn into exactly this literal text). Any other occurrence is garbage the
// regex no longer parses as a ref — an unterminated "${facets:" sequence, or
// one using characters outside the type.name.out.path grammar — that would
// otherwise leak into the rendered manifest silently instead of failing
// closed. Returns a short snippet for the error message.
func findUnresolvedRef(s string) (snippet string, found bool) {
	for i := 0; i < len(s); {
		idx := strings.Index(s[i:], "${facets:")
		if idx < 0 {
			return "", false
		}
		pos := i + idx
		if pos > 0 && s[pos-1] == '$' {
			// Escaped ($${facets:...}) — Unescape's job, not garbage.
			i = pos + 1
			continue
		}
		end := pos + 40
		if end > len(s) {
			end = len(s)
		}
		return s[pos:end], true
	}
	return "", false
}

// scalarNodeForValue builds the yaml.Node for a whole-scalar ref injection.
// json.Number (produced by cpclient.go's UseNumber() decoding, specifically
// to avoid silently corrupting large integers — e.g. 18-digit account or
// resource IDs — through a lossy round-trip via float64) is special-cased so
// it's emitted as a plain, unquoted YAML integer or float scalar using its
// exact decimal text, never quoted as a string and never re-rendered through
// float64's scientific notation. Every other type falls through to
// yaml.Node's own generic Encode, unchanged.
func scalarNodeForValue(v any) (yaml.Node, error) {
	if n, ok := v.(json.Number); ok {
		s := n.String()
		tag := "!!int"
		if strings.ContainsAny(s, ".eE") {
			tag = "!!float"
		}
		return yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: s}, nil
	}
	var node yaml.Node
	err := node.Encode(v)
	return node, err
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
		if snippet, bad := findUnresolvedRef(n.Value); bad {
			*errs = append(*errs, fmt.Errorf("line %d: unresolved or malformed facets reference remains: %q", n.Line, snippet))
			return
		}
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
		enc, err := scalarNodeForValue(v)
		if err != nil {
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
		switch vv := v.(type) {
		case string, float64, float32, int, int64, bool:
			s = strings.Replace(s, r.Raw, fmt.Sprintf("%v", v), 1)
		case json.Number:
			s = strings.Replace(s, r.Raw, vv.String(), 1)
		default:
			*errs = append(*errs, fmt.Errorf("line %d: %s resolves to a non-scalar value and cannot be embedded in a string", n.Line, r.Raw))
		}
	}
	if snippet, bad := findUnresolvedRef(s); bad {
		*errs = append(*errs, fmt.Errorf("line %d: unresolved or malformed facets reference remains: %q", n.Line, snippet))
		return
	}
	n.SetString(Unescape(s))
}
