// cpclient_test.go
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newStub(t *testing.T, outputCalls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "rohit@facets.cloud" || p != "tok-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/cc-ui/v1/stacks/demo-project/clusters":
			json.NewEncoder(w).Encode([]map[string]string{
				{"id": "env-dev-1", "name": "dev"},
				{"id": "env-prod-1", "name": "prod"},
			})
		case r.URL.Path == "/cc-ui/v1/clusters/env-dev-1/resourceType/sqs/resourceName/orders/resource-out-properties":
			if outputCalls != nil {
				outputCalls.Add(1)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"attributes": map[string]any{"queue_url": "https://sqs.example/orders", "depth": 5},
				"interfaces": map[string]any{"reader": map[string]any{"endpoint": "r.example"}},
			})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

func mustRef(t *testing.T, s string) Ref {
	t.Helper()
	rs, err := Find(s)
	if err != nil || len(rs) != 1 {
		t.Fatalf("bad test ref %q: %v", s, err)
	}
	return rs[0]
}

func TestLookupResolvesAttributeAndInterface(t *testing.T) {
	srv := newStub(t, nil)
	defer srv.Close()
	c := NewCPClient(CPConfig{URL: srv.URL, Username: "rohit@facets.cloud", Token: "tok-123"})
	look := c.Lookup("demo-project", "dev")

	v, err := look(mustRef(t, "${facets:sqs.orders.out.attributes.queue_url}"))
	if err != nil || v != "https://sqs.example/orders" {
		t.Fatalf("v=%v err=%v", v, err)
	}
	v, err = look(mustRef(t, "${facets:sqs.orders.out.interfaces.reader.endpoint}"))
	if err != nil || v != "r.example" {
		t.Fatalf("v=%v err=%v", v, err)
	}
}

func TestLookupMemoizesPerResource(t *testing.T) {
	var calls atomic.Int32
	srv := newStub(t, &calls)
	defer srv.Close()
	c := NewCPClient(CPConfig{URL: srv.URL, Username: "rohit@facets.cloud", Token: "tok-123"})
	look := c.Lookup("demo-project", "dev")
	look(mustRef(t, "${facets:sqs.orders.out.attributes.queue_url}"))
	look(mustRef(t, "${facets:sqs.orders.out.attributes.depth}"))
	if calls.Load() != 1 {
		t.Fatalf("outputs fetched %d times, want 1", calls.Load())
	}
}

func TestLookupUnknownEnvironment(t *testing.T) {
	srv := newStub(t, nil)
	defer srv.Close()
	c := NewCPClient(CPConfig{URL: srv.URL, Username: "rohit@facets.cloud", Token: "tok-123"})
	_, err := c.Lookup("demo-project", "staging")(mustRef(t, "${facets:sqs.orders.out.attributes.queue_url}"))
	if err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("err = %v", err)
	}
}

func TestLookupBadPath(t *testing.T) {
	srv := newStub(t, nil)
	defer srv.Close()
	c := NewCPClient(CPConfig{URL: srv.URL, Username: "rohit@facets.cloud", Token: "tok-123"})
	_, err := c.Lookup("demo-project", "dev")(mustRef(t, "${facets:sqs.orders.out.attributes.nope}"))
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnauthorizedSurfaced(t *testing.T) {
	srv := newStub(t, nil)
	defer srv.Close()
	c := NewCPClient(CPConfig{URL: srv.URL, Username: "rohit@facets.cloud", Token: "wrong"})
	_, err := c.Lookup("demo-project", "dev")(mustRef(t, "${facets:sqs.orders.out.attributes.queue_url}"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

// TestLookupMemoizesFailedFetch proves a resource whose outputs endpoint
// 500s is only ever fetched once per render, even when looked up multiple
// times — the failure itself is memoized (outputErrs), not just successes,
// so a hot loop over a broken resource can't hammer the control plane.
func TestLookupMemoizesFailedFetch(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/cc-ui/v1/stacks/demo-project/clusters":
			json.NewEncoder(w).Encode([]map[string]string{{"id": "env-dev-1", "name": "dev"}})
		case r.URL.Path == "/cc-ui/v1/clusters/env-dev-1/resourceType/sqs/resourceName/broken/resource-out-properties":
			calls.Add(1)
			http.Error(w, "internal error", http.StatusInternalServerError)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := NewCPClient(CPConfig{URL: srv.URL, Username: "rohit@facets.cloud", Token: "tok-123"})
	look := c.Lookup("demo-project", "dev")

	ref := mustRef(t, "${facets:sqs.broken.out.attributes.queue_url}")
	_, err1 := look(ref)
	_, err2 := look(ref)
	if err1 == nil || err2 == nil {
		t.Fatalf("expected errors, got err1=%v err2=%v", err1, err2)
	}
	if err1.Error() != err2.Error() {
		t.Fatalf("errors differ across calls: %q vs %q", err1, err2)
	}
	if calls.Load() != 1 {
		t.Fatalf("outputs endpoint hit %d times, want 1", calls.Load())
	}
}

func TestConfigFromEnvListsAllMissing(t *testing.T) {
	t.Setenv("FACETS_CP_URL", "")
	t.Setenv("FACETS_CP_USERNAME", "")
	t.Setenv("FACETS_CP_TOKEN", "")
	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, v := range []string{"FACETS_CP_URL", "FACETS_CP_USERNAME", "FACETS_CP_TOKEN"} {
		if !strings.Contains(err.Error(), v) {
			t.Fatalf("error %q missing %s", err, v)
		}
	}
}
