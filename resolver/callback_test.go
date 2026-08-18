// callback_test.go
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"k8s.io/client-go/rest"
)

// captureStderr redirects os.Stderr for the duration of fn and returns
// everything written to it — used to assert on the warnings this codebase
// writes directly to stderr (callbackTarget's partial-annotation warning,
// run's callback-failure warning) rather than returning as errors.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestNormalizeExpressionsStripsDedupsSorts(t *testing.T) {
	refs := []Ref{
		mustRef(t, "${facets:dynamodb.state-lock.out.attributes.table_name}"),
		mustRef(t, "${facets:blueprint.self.variables.logo_url}"),
		mustRef(t, "${facets:dynamodb.state-lock.out.attributes.table_name}"), // duplicate
		mustRef(t, "${facets:blueprint.self.secrets.API_KEY}"),
	}
	got := normalizeExpressions(refs)
	want := []string{
		"${blueprint.self.secrets.API_KEY}",
		"${blueprint.self.variables.logo_url}",
		"${dynamodb.state-lock.out.attributes.table_name}",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// --- Direct reportConsumedReferences tests (no run()/kube layer) ---

// callbackCPStub serves the two reads (resources-info, stack branch) and the
// one write (designer/v2) reportConsumedReferences needs, for project
// "cb-project" / resource argocd_application/my-app-resource. currentByEnv
// nil means the resource's spec has no facets_references field at all yet;
// otherwise it seeds spec.facets_references directly with the given value
// (keyed by environment name, e.g. {"dev": {"expressions": [...]}}).
// postStatus 0 means "succeed" (200).
func callbackCPStub(t *testing.T, postCalls *atomic.Int32, lastPostBody *[]byte, currentByEnv map[string]any, postStatus int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cc-ui/v1/dropdown/stack/cb-project/resources-info":
			spec := map[string]any{"chart": "my-chart"}
			if currentByEnv != nil {
				spec["facets_references"] = currentByEnv
			}
			content, _ := json.Marshal(map[string]any{
				"kind":   "argocd_application",
				"flavor": "default",
				"spec":   spec,
			})
			json.NewEncoder(w).Encode([]map[string]any{
				{"resourceType": "argocd_application", "resourceName": "my-app-resource", "content": string(content)},
				{"resourceType": "other_type", "resourceName": "unrelated", "content": `{"spec":{}}`},
			})
		case "/cc-ui/v1/stacks/cb-project":
			json.NewEncoder(w).Encode(map[string]any{"branch": "main"})
		case "/cc-ui/v1/designer/v2/cb-project/resources":
			if postCalls != nil {
				postCalls.Add(1)
			}
			if lastPostBody != nil {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				*lastPostBody = body
				mu.Unlock()
			}
			if postStatus != 0 {
				w.WriteHeader(postStatus)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, r.URL.Path, http.StatusNotFound)
		}
	}))
}

func cbTarget() CallbackTarget {
	return CallbackTarget{
		AppName:         "my-app",
		ResourceType:    "argocd_application",
		ResourceName:    "my-app-resource",
		ReferencesField: "facets_references",
	}
}

func cbRefs(t *testing.T) []Ref {
	return []Ref{
		mustRef(t, "${facets:sqs.orders.out.attributes.queue_url}"),
		mustRef(t, "${facets:blueprint.self.variables.logo_url}"),
	}
}

// TestReportConsumedReferencesHappyPath: field currently absent -> exactly
// one write, with the correct URL, branch, commit message, and normalized
// sorted expressions nested under the environment name
// ({"dev": {"expressions": [...]}}).
func TestReportConsumedReferencesHappyPath(t *testing.T) {
	var postCalls atomic.Int32
	var body []byte
	srv := callbackCPStub(t, &postCalls, &body, nil, 0)
	defer srv.Close()
	client := NewCPClient(CPConfig{URL: srv.URL, Username: "u", Token: "t"})

	if err := reportConsumedReferences(client, "cb-project", "dev", cbTarget(), cbRefs(t)); err != nil {
		t.Fatal(err)
	}
	if postCalls.Load() != 1 {
		t.Fatalf("POST called %d times, want 1", postCalls.Load())
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("posted body not valid JSON: %v\n%s", err, body)
	}
	if req["branch"] != "main" {
		t.Fatalf("branch = %v, want main", req["branch"])
	}
	msg, _ := req["commitMessage"].(string)
	if !strings.Contains(msg, "my-app") {
		t.Fatalf("commitMessage %q missing app name", msg)
	}
	resources, _ := req["resources"].([]any)
	if len(resources) != 1 {
		t.Fatalf("resources = %#v, want exactly 1", resources)
	}
	res, _ := resources[0].(map[string]any)
	if res["resourceType"] != "argocd_application" || res["resourceName"] != "my-app-resource" {
		t.Fatalf("resource identity wrong: %#v", res)
	}
	content, _ := res["content"].(map[string]any)
	spec, _ := content["spec"].(map[string]any)
	byEnv, _ := spec["facets_references"].(map[string]any)
	devSection, _ := byEnv["dev"].(map[string]any)
	exprs, _ := devSection["expressions"].([]any)
	want := []any{"${blueprint.self.variables.logo_url}", "${sqs.orders.out.attributes.queue_url}"}
	if !reflect.DeepEqual(exprs, want) {
		t.Fatalf("expressions = %#v, want %#v", exprs, want)
	}
	// Untouched sibling spec field must survive the read-modify-write.
	if spec["chart"] != "my-chart" {
		t.Fatalf("unrelated spec field clobbered: %#v", spec)
	}
}

// TestReportConsumedReferencesIdempotentSkip: the environment's section
// already deep-equals what we'd compute -> zero write calls, zero commits.
func TestReportConsumedReferencesIdempotentSkip(t *testing.T) {
	var postCalls atomic.Int32
	already := map[string]any{
		"dev": map[string]any{"expressions": []string{"${blueprint.self.variables.logo_url}", "${sqs.orders.out.attributes.queue_url}"}},
	}
	srv := callbackCPStub(t, &postCalls, nil, already, 0)
	defer srv.Close()
	client := NewCPClient(CPConfig{URL: srv.URL, Username: "u", Token: "t"})

	if err := reportConsumedReferences(client, "cb-project", "dev", cbTarget(), cbRefs(t)); err != nil {
		t.Fatal(err)
	}
	if postCalls.Load() != 0 {
		t.Fatalf("POST called %d times, want 0 (idempotent skip)", postCalls.Load())
	}
}

// TestReportConsumedReferencesChangeTriggersWrite: the environment's section
// exists but with stale/different expressions -> exactly one write, with
// the new value.
func TestReportConsumedReferencesChangeTriggersWrite(t *testing.T) {
	var postCalls atomic.Int32
	var body []byte
	stale := map[string]any{
		"dev": map[string]any{"expressions": []string{"${some.old.out.attributes.thing}"}},
	}
	srv := callbackCPStub(t, &postCalls, &body, stale, 0)
	defer srv.Close()
	client := NewCPClient(CPConfig{URL: srv.URL, Username: "u", Token: "t"})

	if err := reportConsumedReferences(client, "cb-project", "dev", cbTarget(), cbRefs(t)); err != nil {
		t.Fatal(err)
	}
	if postCalls.Load() != 1 {
		t.Fatalf("POST called %d times, want 1", postCalls.Load())
	}
	if !strings.Contains(string(body), "logo_url") {
		t.Fatalf("posted body missing the new expressions:\n%s", body)
	}
	if strings.Contains(string(body), "old.out.attributes.thing") {
		t.Fatalf("posted body still contains the stale expression:\n%s", body)
	}
}

// TestReportConsumedReferencesPreservesOtherEnvironments: a DIFFERENT
// environment's section already exists under the same references field ->
// the write still fires (this environment's own section is new/changed),
// but the other environment's section survives untouched in the posted
// body — a merge at the environment key, never a wholesale field
// replacement.
func TestReportConsumedReferencesPreservesOtherEnvironments(t *testing.T) {
	var postCalls atomic.Int32
	var body []byte
	prodOnly := map[string]any{
		"prod": map[string]any{"expressions": []string{"${prod.only.out.attributes.thing}"}},
	}
	srv := callbackCPStub(t, &postCalls, &body, prodOnly, 0)
	defer srv.Close()
	client := NewCPClient(CPConfig{URL: srv.URL, Username: "u", Token: "t"})

	if err := reportConsumedReferences(client, "cb-project", "dev", cbTarget(), cbRefs(t)); err != nil {
		t.Fatal(err)
	}
	if postCalls.Load() != 1 {
		t.Fatalf("POST called %d times, want 1", postCalls.Load())
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("posted body not valid JSON: %v\n%s", err, body)
	}
	resources, _ := req["resources"].([]any)
	res, _ := resources[0].(map[string]any)
	content, _ := res["content"].(map[string]any)
	spec, _ := content["spec"].(map[string]any)
	byEnv, _ := spec["facets_references"].(map[string]any)

	prodSection, _ := byEnv["prod"].(map[string]any)
	prodExprs, _ := prodSection["expressions"].([]any)
	if !reflect.DeepEqual(prodExprs, []any{"${prod.only.out.attributes.thing}"}) {
		t.Fatalf("prod's section was not preserved untouched: %#v", prodExprs)
	}

	devSection, _ := byEnv["dev"].(map[string]any)
	devExprs, _ := devSection["expressions"].([]any)
	want := []any{"${blueprint.self.variables.logo_url}", "${sqs.orders.out.attributes.queue_url}"}
	if !reflect.DeepEqual(devExprs, want) {
		t.Fatalf("dev's section = %#v, want %#v", devExprs, want)
	}
}

// TestReportConsumedReferencesResourceWithNoSpecFieldAtAll proves the
// callback still works when the blueprint resource's content has no "spec"
// key at all yet (e.g. a freshly created resource whose module's
// facets.yaml declares the references field in its schema, but nothing has
// ever populated ANY spec value) — not just an absent references field
// within an existing spec object (already covered by the happy-path test
// above), but a completely missing spec object one level up. Both spec and
// the references field are created from scratch, and the write still only
// touches this one environment's section.
func TestReportConsumedReferencesResourceWithNoSpecFieldAtAll(t *testing.T) {
	var postCalls atomic.Int32
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cc-ui/v1/dropdown/stack/cb-project/resources-info":
			// content has no "spec" key at all — not even an empty object.
			json.NewEncoder(w).Encode([]map[string]any{
				{"resourceType": "argocd_application", "resourceName": "my-app-resource", "content": `{"kind":"argocd_application","flavor":"default"}`},
			})
		case "/cc-ui/v1/stacks/cb-project":
			json.NewEncoder(w).Encode(map[string]any{"branch": "main"})
		case "/cc-ui/v1/designer/v2/cb-project/resources":
			postCalls.Add(1)
			b, _ := io.ReadAll(r.Body)
			body = b
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	client := NewCPClient(CPConfig{URL: srv.URL, Username: "u", Token: "t"})

	if err := reportConsumedReferences(client, "cb-project", "dev", cbTarget(), cbRefs(t)); err != nil {
		t.Fatal(err)
	}
	if postCalls.Load() != 1 {
		t.Fatalf("POST called %d times, want 1", postCalls.Load())
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("posted body not valid JSON: %v\n%s", err, body)
	}
	resources, _ := req["resources"].([]any)
	res, _ := resources[0].(map[string]any)
	content, _ := res["content"].(map[string]any)
	spec, ok := content["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec was not created from scratch: %#v", content)
	}
	byEnv, _ := spec["facets_references"].(map[string]any)
	devSection, _ := byEnv["dev"].(map[string]any)
	exprs, _ := devSection["expressions"].([]any)
	want := []any{"${blueprint.self.variables.logo_url}", "${sqs.orders.out.attributes.queue_url}"}
	if !reflect.DeepEqual(exprs, want) {
		t.Fatalf("expressions = %#v, want %#v", exprs, want)
	}
}

// TestReportConsumedReferencesAPIErrorSurfaced proves the function itself
// returns a normal error on a write failure — main.go's run is what turns
// this into a warning, not this function (see its doc comment).
func TestReportConsumedReferencesAPIErrorSurfaced(t *testing.T) {
	srv := callbackCPStub(t, nil, nil, nil, http.StatusForbidden)
	defer srv.Close()
	client := NewCPClient(CPConfig{URL: srv.URL, Username: "u", Token: "t"})

	err := reportConsumedReferences(client, "cb-project", "dev", cbTarget(), cbRefs(t))
	if err == nil {
		t.Fatal("expected an error from a 403 write")
	}
}

// --- End-to-end via run(): the callback wired into the full pipeline ---

func callbackAppObj(annotations map[string]any) map[string]any {
	return appObj("app-with-callback", "my-ns", annotations, "my-app")
}

// runStub bundles a matched Application (kube) + resource-output/branch/
// designer endpoints (CP) for an end-to-end run() callback test, all under
// project "app-project" / environment "app-environment", matching
// refStream's "${facets:sqs.orders.out.attributes.queue_url}".
// currentByEnv seeds the resource's existing spec.facets_references value
// (nil = absent).
func runStub(t *testing.T, annotations map[string]any, postCalls *atomic.Int32, lastPostBody *[]byte, currentByEnv map[string]any, postStatus int) (kube, cp *httptest.Server) {
	t.Helper()
	kube = stubKube(t, []map[string]any{callbackAppObj(annotations)})
	var mu sync.Mutex
	cp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cc-ui/v1/stacks/app-project/clusters":
			json.NewEncoder(w).Encode([]map[string]string{{"id": "env-2", "name": "app-environment"}})
		case "/cc-ui/v1/clusters/env-2/resourceType/sqs/resourceName/orders/resource-out-properties":
			json.NewEncoder(w).Encode(map[string]any{"attributes": map[string]any{"queue_url": "https://sqs.example/from-app"}})
		case "/cc-ui/v1/dropdown/stack/app-project/resources-info":
			spec := map[string]any{}
			if currentByEnv != nil {
				spec["facets_references"] = currentByEnv
			}
			content, _ := json.Marshal(map[string]any{"kind": "argocd_application", "flavor": "default", "spec": spec})
			json.NewEncoder(w).Encode([]map[string]any{
				{"resourceType": "argocd_application", "resourceName": "my-app-resource", "content": string(content)},
			})
		case "/cc-ui/v1/stacks/app-project":
			json.NewEncoder(w).Encode(map[string]any{"branch": "main"})
		case "/cc-ui/v1/designer/v2/app-project/resources":
			if postCalls != nil {
				postCalls.Add(1)
			}
			if lastPostBody != nil {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				*lastPostBody = body
				mu.Unlock()
			}
			if postStatus != 0 {
				w.WriteHeader(postStatus)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, r.URL.Path, http.StatusNotFound)
		}
	}))
	return kube, cp
}

func fullCallbackAnnotations() map[string]any {
	return map[string]any{
		annotationProject:         "app-project",
		annotationEnvironment:     "app-environment",
		annotationResourceType:    "argocd_application",
		annotationResourceName:    "my-app-resource",
		annotationReferencesField: "facets_references",
	}
}

// TestRunFullCallbackHappyPath: an Application with all three callback
// annotations, end to end through run() — the callback fires exactly once
// with the right shape (expressions nested under "app-environment"), and
// the resolved manifest still reaches stdout correctly.
func TestRunFullCallbackHappyPath(t *testing.T) {
	var postCalls atomic.Int32
	var body []byte
	kube, cp := runStub(t, fullCallbackAnnotations(), &postCalls, &body, nil, 0)
	defer kube.Close()
	defer cp.Close()
	setEnv(t, cp.URL)

	var out bytes.Buffer
	err := run(strings.NewReader(refStream), &out, "my-ns", "my-app", staticConfig(&rest.Config{Host: kube.URL}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "https://sqs.example/from-app") {
		t.Fatalf("resolved manifest missing/wrong:\n%s", out.String())
	}
	if postCalls.Load() != 1 {
		t.Fatalf("POST called %d times, want 1", postCalls.Load())
	}
	if !strings.Contains(string(body), "sqs.orders.out.attributes.queue_url") {
		t.Fatalf("posted body missing the consumed expression:\n%s", body)
	}
	if !strings.Contains(string(body), `"app-environment"`) {
		t.Fatalf("posted body doesn't key the expressions by environment name:\n%s", body)
	}
}

// TestRunCallbackAPIErrorDoesNotFailRender: the write fails (403, e.g. a
// read-only CP token), but the render is unaffected — run returns nil and
// stdout has the fully resolved manifest, exactly as if no callback were
// configured at all. Only a stderr warning marks the difference.
func TestRunCallbackAPIErrorDoesNotFailRender(t *testing.T) {
	kube, cp := runStub(t, fullCallbackAnnotations(), nil, nil, nil, http.StatusForbidden)
	defer kube.Close()
	defer cp.Close()
	setEnv(t, cp.URL)

	var out bytes.Buffer
	var runErr error
	stderr := captureStderr(t, func() {
		runErr = run(strings.NewReader(refStream), &out, "my-ns", "my-app", staticConfig(&rest.Config{Host: kube.URL}))
	})
	if runErr != nil {
		t.Fatalf("render must succeed despite the callback failing: %v", runErr)
	}
	if !strings.Contains(out.String(), "https://sqs.example/from-app") {
		t.Fatalf("resolved manifest missing/wrong despite callback failure:\n%s", out.String())
	}
	if !strings.Contains(stderr, "reporting consumed references") {
		t.Fatalf("expected a warning on stderr specifically about the failed callback, got:\n%s", stderr)
	}
}

// TestRunPartialCallbackAnnotationsSkipsCallback: only 2 of the 3 optional
// annotations present -> no callback attempted (zero writes) and a stderr
// warning naming what's missing, but the render itself is unaffected.
func TestRunPartialCallbackAnnotationsSkipsCallback(t *testing.T) {
	ann := fullCallbackAnnotations()
	delete(ann, annotationReferencesField) // only 2 of 3 present
	var postCalls atomic.Int32
	kube, cp := runStub(t, ann, &postCalls, nil, nil, 0)
	defer kube.Close()
	defer cp.Close()
	setEnv(t, cp.URL)

	var out bytes.Buffer
	var runErr error
	stderr := captureStderr(t, func() {
		runErr = run(strings.NewReader(refStream), &out, "my-ns", "my-app", staticConfig(&rest.Config{Host: kube.URL}))
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !strings.Contains(out.String(), "https://sqs.example/from-app") {
		t.Fatalf("resolved manifest missing/wrong:\n%s", out.String())
	}
	if postCalls.Load() != 0 {
		t.Fatalf("POST called %d times, want 0 (partial annotations must not trigger a callback)", postCalls.Load())
	}
	if !strings.Contains(stderr, annotationReferencesField) {
		t.Fatalf("warning doesn't name the missing annotation %q: %q", annotationReferencesField, stderr)
	}
}

// TestRunNoCallbackAnnotationsIsSilent: zero of the three present is the
// ordinary case (callback simply not configured) and must not warn at all.
func TestRunNoCallbackAnnotationsIsSilent(t *testing.T) {
	ann := map[string]any{
		annotationProject:     "app-project",
		annotationEnvironment: "app-environment",
	}
	var postCalls atomic.Int32
	kube, cp := runStub(t, ann, &postCalls, nil, nil, 0)
	defer kube.Close()
	defer cp.Close()
	setEnv(t, cp.URL)

	var out bytes.Buffer
	var runErr error
	stderr := captureStderr(t, func() {
		runErr = run(strings.NewReader(refStream), &out, "my-ns", "my-app", staticConfig(&rest.Config{Host: kube.URL}))
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	if postCalls.Load() != 0 {
		t.Fatalf("POST called %d times, want 0", postCalls.Load())
	}
	if strings.Contains(stderr, "callback") || strings.Contains(stderr, "facets-references") {
		t.Fatalf("unexpected callback-related warning for an Application with no callback annotations at all: %q", stderr)
	}
}
