// resolver_test.go
package main

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFindParsesRef(t *testing.T) {
	got, err := Find("url: ${facets:sqs.orders.out.attributes.queue_url}")
	if err != nil {
		t.Fatal(err)
	}
	want := []Ref{{
		Raw:          "${facets:sqs.orders.out.attributes.queue_url}",
		ResourceType: "sqs",
		ResourceName: "orders",
		Path:         []string{"attributes", "queue_url"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestFindMultipleAndInterfaces(t *testing.T) {
	got, err := Find("${facets:postgres.main.out.interfaces.reader.connection_string} and ${facets:redis.cache.out.attributes.host}")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path[0] != "interfaces" || got[1].ResourceType != "redis" {
		t.Fatalf("got %+v", got)
	}
}

func TestFindSkipsEscaped(t *testing.T) {
	got, err := Find("literal: $${facets:sqs.q.out.attributes.url}")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("escaped ref matched: %+v", got)
	}
}

func TestFindRejectsMalformed(t *testing.T) {
	// These all match refPattern's grammar (dot-separated
	// [A-Za-z0-9_-]+ segments) but fail Find's own arity/"out"-position
	// checks — still caught directly, with an error, inside Find itself.
	for _, s := range []string{
		"${facets:sqs.orders}",                // no out segment
		"${facets:sqs.orders.attributes.url}", // missing out
		"${facets:sqs.orders.out}",            // empty path
	} {
		if _, err := Find(s); err == nil {
			t.Fatalf("expected error for %q", s)
		}
	}
}

// TestFindSkipsMalformedGrammarWithoutMatching documents the flip side of
// refPattern's stricter grammar: these never match the regex at all (an
// empty segment from a leading/interior/trailing dot, or fully empty
// content), so Find reports zero refs and no error — it's not that Find
// silently approves them, it simply never sees them as a ref candidate in
// the first place. They're caught downstream instead, by the
// findUnresolvedRef fail-closed net exercised in
// TestResolveStreamUnresolvedGarbageHardError and
// TestRunZeroRefsGarbageHardError.
func TestFindSkipsMalformedGrammarWithoutMatching(t *testing.T) {
	for _, s := range []string{
		"${facets:}",                                     // empty
		"${facets:.orders.out.attributes.x}",             // empty type (leading dot)
		"${facets:sqs..out.attributes.x}",                // empty name (interior dot)
		"${facets:sqs.orders.out.attributes.}",           // trailing dot (empty path segment)
		"${facets:sqs.orders.out.attributes..queue_url}", // interior empty segment
		"${facets:sqs.orders!.out.attributes.x}",         // illegal character
		"${facets:sqs.orders.out.attributes.queue_url",   // unterminated (no closing brace)
	} {
		got, err := Find(s)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", s, err)
		}
		if len(got) != 0 {
			t.Fatalf("%q: got %+v, want zero refs (regex shouldn't match)", s, got)
		}
	}
}

func TestFindParsesBlueprintRefs(t *testing.T) {
	got, err := Find("v: ${facets:blueprint.self.variables.DB_HOST} s: ${facets:blueprint.self.secrets.API_KEY} a: ${facets:blueprint.self.artifacts.web}")
	if err != nil {
		t.Fatal(err)
	}
	want := []Ref{
		{Raw: "${facets:blueprint.self.variables.DB_HOST}", Kind: RefKindBlueprintVariable, Name: "DB_HOST"},
		{Raw: "${facets:blueprint.self.secrets.API_KEY}", Kind: RefKindBlueprintSecret, Name: "API_KEY"},
		{Raw: "${facets:blueprint.self.artifacts.web}", Kind: RefKindBlueprintArtifact, Name: "web"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

// TestFindRejectsMalformedBlueprintRefs proves a blueprint.self ref whose
// class isn't one of variables/secrets/artifacts, or whose segment count is
// wrong, is a hard error naming the valid classes — never silently
// misparsed as (or falling through to) a resource-output ref, even when
// parts[2] happens to be "out" (a resource type/name literally "blueprint"/
// "self" is not reachable via this grammar; it's reserved).
func TestFindRejectsMalformedBlueprintRefs(t *testing.T) {
	for _, s := range []string{
		"${facets:blueprint.self.out.x}",             // "out" is not a valid blueprint class
		"${facets:blueprint.self.envvars.DB_HOST}",   // not one of the three classes
		"${facets:blueprint.self.variables}",         // missing name (3 segments)
		"${facets:blueprint.self}",                   // missing class and name (2 segments)
		"${facets:blueprint.self.variables.X.extra}", // too many segments (5)
	} {
		if _, err := Find(s); err == nil {
			t.Fatalf("expected error for %q", s)
		} else if !strings.Contains(err.Error(), "variables, secrets, artifacts") && !strings.Contains(err.Error(), "blueprint.self.<variables|secrets|artifacts>") {
			t.Fatalf("%q: error doesn't name the valid classes: %v", s, err)
		}
	}
}

func TestUnescape(t *testing.T) {
	if got := Unescape("a $${facets:x.y.out.z} b"); got != "a ${facets:x.y.out.z} b" {
		t.Fatalf("got %q", got)
	}
}

func lookup(t *testing.T) LookupFunc {
	vals := map[string]any{
		"sqs.orders.out.attributes.queue_url": "https://sqs.example/orders",
		"service.api.out.attributes.replicas": float64(3),
		"feature.flags.out.attributes.on":     true,
		"postgres.main.out.attributes.conn":   map[string]any{"host": "db.example", "port": float64(5432)},
	}
	return func(r Ref) (any, error) {
		key := r.ResourceType + "." + r.ResourceName + ".out." + strings.Join(r.Path, ".")
		v, ok := vals[key]
		if !ok {
			return nil, fmt.Errorf("no output at %s", key)
		}
		return v, nil
	}
}

// resolveStreamTo resolves a single-document stream through ResolveStream
// and decodes it back into a map, for tests that don't care about
// multi-document behavior (that's covered separately by the
// TestResolveStream* multi-doc tests below).
func resolveStreamTo(t *testing.T, in string) map[string]any {
	t.Helper()
	out, err := ResolveStream([]byte(in), lookup(t))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// decodeAllDocs decodes every document in a stream into plain Go values,
// re-parsing it the way a consumer downstream of ResolveStream would.
func decodeAllDocs(t *testing.T, stream []byte) []any {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(stream))
	var docs []any
	for {
		var v any
		err := dec.Decode(&v)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream does not re-parse doc-by-doc: %v\n%s", err, stream)
		}
		docs = append(docs, v)
	}
	return docs
}

func TestResolveStreamMultiDoc(t *testing.T) {
	in := "a: ${facets:sqs.orders.out.attributes.queue_url}\n---\nb: ${facets:service.api.out.attributes.replicas}\n"
	out, err := ResolveStream([]byte(in), lookup(t))
	if err != nil {
		t.Fatal(err)
	}
	docs := decodeAllDocs(t, out)
	if len(docs) != 2 {
		t.Fatalf("got %d docs, want 2:\n%s", len(docs), out)
	}
	doc0 := docs[0].(map[string]any)
	if doc0["a"] != "https://sqs.example/orders" {
		t.Fatalf("doc0 = %#v", doc0)
	}
	doc1 := docs[1].(map[string]any)
	if doc1["b"] != 3 && doc1["b"] != float64(3) {
		t.Fatalf("doc1 = %#v, want order preserved (doc1.b resolved)", doc1)
	}
}

func TestResolveStreamAggregatesErrorsAcrossDocs(t *testing.T) {
	in := "a: ${facets:nope.a.out.attributes.x}\n---\nb: ${facets:nope.b.out.attributes.y}\n"
	_, err := ResolveStream([]byte(in), lookup(t))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"nope.a", "nope.b"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestResolveStreamEscapeOnly(t *testing.T) {
	in := "raw: $${facets:sqs.orders.out.attributes.queue_url}\n---\nother: $${facets:a.b.out.c}\n"
	out, err := ResolveStream([]byte(in), lookup(t))
	if err != nil {
		t.Fatal(err)
	}
	docs := decodeAllDocs(t, out)
	if len(docs) != 2 {
		t.Fatalf("got %d docs, want 2:\n%s", len(docs), out)
	}
	if docs[0].(map[string]any)["raw"] != "${facets:sqs.orders.out.attributes.queue_url}" {
		t.Fatalf("doc0 = %#v", docs[0])
	}
	if docs[1].(map[string]any)["other"] != "${facets:a.b.out.c}" {
		t.Fatalf("doc1 = %#v", docs[1])
	}
	if strings.Contains(string(out), "$${facets:") {
		t.Fatalf("escaped ref leaked verbatim:\n%s", out)
	}
}

func TestResolveStreamNullAndEmptyDocsPassThrough(t *testing.T) {
	in := "a: 1\n---\n---\nb: 2\n"
	out, err := ResolveStream([]byte(in), lookup(t))
	if err != nil {
		t.Fatal(err)
	}
	docs := decodeAllDocs(t, out)
	if len(docs) != 3 {
		t.Fatalf("got %d docs, want 3 (incl. 1 null passthrough doc):\n%s", len(docs), out)
	}
	if docs[0].(map[string]any)["a"] != 1 {
		t.Fatalf("doc0 = %#v", docs[0])
	}
	if docs[1] != nil {
		t.Fatalf("doc1 = %#v, want nil (empty doc passthrough)", docs[1])
	}
	if docs[2].(map[string]any)["b"] != 2 {
		t.Fatalf("doc2 = %#v", docs[2])
	}
}

func TestWholeRefKeepsType(t *testing.T) {
	m := resolveStreamTo(t, "replicas: ${facets:service.api.out.attributes.replicas}\nflag: ${facets:feature.flags.out.attributes.on}\n")
	if m["replicas"] != 3 && m["replicas"] != float64(3) {
		t.Fatalf("replicas = %#v, want 3", m["replicas"])
	}
	if m["flag"] != true {
		t.Fatalf("flag = %#v, want true", m["flag"])
	}
}

func TestWholeRefObject(t *testing.T) {
	m := resolveStreamTo(t, "db: ${facets:postgres.main.out.attributes.conn}\n")
	db := m["db"].(map[string]any)
	if db["host"] != "db.example" {
		t.Fatalf("db = %#v", db)
	}
}

func TestEmbeddedRefStringifies(t *testing.T) {
	m := resolveStreamTo(t, "url: prefix-${facets:sqs.orders.out.attributes.queue_url}-suffix\nn: \"r=${facets:service.api.out.attributes.replicas}\"\n")
	if m["url"] != "prefix-https://sqs.example/orders-suffix" {
		t.Fatalf("url = %#v", m["url"])
	}
	if m["n"] != "r=3" {
		t.Fatalf("n = %#v", m["n"])
	}
}

func TestEmbeddedObjectIsError(t *testing.T) {
	_, err := ResolveStream([]byte("x: a-${facets:postgres.main.out.attributes.conn}\n"), lookup(t))
	if err == nil || !strings.Contains(err.Error(), "non-scalar") {
		t.Fatalf("err = %v", err)
	}
}

// Aggregation WITHIN a single document (two bad refs, two different keys, one
// document) — distinct from TestResolveStreamAggregatesErrorsAcrossDocs above,
// which aggregates across separate "---"-separated documents.
func TestErrorsAggregateWithinDocument(t *testing.T) {
	_, err := ResolveStream([]byte("a: ${facets:nope.a.out.attributes.x}\nb: ${facets:nope.b.out.attributes.y}\n"), lookup(t))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"nope.a", "nope.b"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestNestedStructuresWalked(t *testing.T) {
	m := resolveStreamTo(t, "outer:\n  list:\n  - ${facets:sqs.orders.out.attributes.queue_url}\n")
	list := m["outer"].(map[string]any)["list"].([]any)
	if list[0] != "https://sqs.example/orders" {
		t.Fatalf("list = %#v", list)
	}
}

// TestResolveStreamUnresolvedGarbageHardError proves the findUnresolvedRef
// fail-closed net: input that refPattern's stricter grammar no longer
// matches as a ref (see TestFindSkipsMalformedGrammarWithoutMatching) must
// still hard-fail the render rather than leaking the literal, un-resolved
// "${facets:" text into the output.
func TestResolveStreamUnresolvedGarbageHardError(t *testing.T) {
	cases := map[string]string{
		"trailing dot":   "bad: ${facets:sqs.orders.out.attributes.}\n",
		"interior empty": "bad: ${facets:sqs.orders.out.attributes..queue_url}\n",
		"illegal char":   "bad: ${facets:sqs.orders!.out.attributes.x}\n",
		"unterminated":   "bad: ${facets:sqs.orders.out.attributes.queue_url\n",
		"empty":          "bad: ${facets:}\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveStream([]byte(in), lookup(t))
			if err == nil {
				t.Fatalf("expected a hard error for %s", name)
			}
			if !strings.Contains(err.Error(), "unresolved or malformed facets reference") {
				t.Fatalf("%s: err = %v", name, err)
			}
		})
	}
}

// TestResolveStreamUnresolvedGarbageAlongsideValidRef proves the guard also
// fires when garbage shares a document with an otherwise-valid, successfully
// resolved ref (i.e. it isn't just a whole-stream fallback check).
func TestResolveStreamUnresolvedGarbageAlongsideValidRef(t *testing.T) {
	in := "good: ${facets:sqs.orders.out.attributes.queue_url}\nbad: ${facets:sqs.orders!.out.attributes.x}\n"
	_, err := ResolveStream([]byte(in), lookup(t))
	if err == nil {
		t.Fatal("expected a hard error")
	}
	if !strings.Contains(err.Error(), "unresolved or malformed facets reference") {
		t.Fatalf("err = %v", err)
	}
}

// TestResolveStreamEscapedGarbageLookalikeIsNotFlagged proves the guard
// doesn't false-positive on a legitimately $$-escaped ref, even though the
// escaped and unresolved-garbage cases look identical AFTER Unescape runs —
// the guard runs against the pre-Unescape text specifically to tell them
// apart. Overlaps with TestResolveStreamEscapeOnly but asserts it from the
// guard's specific point of view.
func TestResolveStreamEscapedGarbageLookalikeIsNotFlagged(t *testing.T) {
	in := "raw: $${facets:sqs.orders.out.attributes.queue_url}\n"
	out, err := ResolveStream([]byte(in), lookup(t))
	if err != nil {
		t.Fatalf("escaped ref incorrectly flagged as unresolved garbage: %v", err)
	}
	if !strings.Contains(string(out), "${facets:sqs.orders.out.attributes.queue_url}") {
		t.Fatalf("unescaped literal missing from output:\n%s", out)
	}
}

func TestWholeRefPreservesAnchor(t *testing.T) {
	out, err := ResolveStream([]byte("anchor: &a ${facets:sqs.orders.out.attributes.queue_url}\nalias: *a\n"), lookup(t))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("resolved document does not re-parse: %v\n%s", err, out)
	}
	if m["anchor"] != "https://sqs.example/orders" || m["alias"] != "https://sqs.example/orders" {
		t.Fatalf("m = %#v", m)
	}
}
