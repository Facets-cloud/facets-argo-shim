// coordinates_test.go
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

const refStream = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: demo\ndata:\n  queueUrl: ${facets:sqs.orders.out.attributes.queue_url}\n"

// --- Facets CP stub (shared by all tests that need a real resolve) ---

// stubCP serves the Facets CP for the app-project/app-environment
// coordinates a matched, annotated Application resolves to.
func stubCP(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/cc-ui/v1/stacks/app-project/clusters":
			json.NewEncoder(w).Encode([]map[string]string{{"id": "env-2", "name": "app-environment"}})
		case r.URL.Path == "/cc-ui/v1/clusters/env-2/resourceType/sqs/resourceName/orders/resource-out-properties":
			json.NewEncoder(w).Encode(map[string]any{"attributes": map[string]any{"queue_url": "https://sqs.example/from-app"}})
		default:
			http.Error(w, r.URL.Path, http.StatusNotFound)
		}
	}))
}

func setEnv(t *testing.T, cpURL string) {
	t.Setenv("FACETS_CP_URL", cpURL)
	t.Setenv("FACETS_CP_USERNAME", "u")
	t.Setenv("FACETS_CP_TOKEN", "t")
}

// --- Kubernetes LIST stub ---

// stubKube serves the Application LIST endpoint the dynamic client hits
// (confirmed empirically: GET /apis/argoproj.io/v1alpha1/namespaces/<ns>
// /applications, no query params), returning the given items.
func stubKube(t *testing.T, items []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications" {
			http.Error(w, r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationList",
			"items": items,
		})
	}))
}

// stubKubeError serves a 500 for the LIST endpoint, to exercise the
// hard-error path.
func stubKubeError(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
}

func appObj(name, destNamespace string, annotations map[string]any, helmReleaseName string) map[string]any {
	spec := map[string]any{"destination": map[string]any{"namespace": destNamespace}}
	if helmReleaseName != "" {
		spec["source"] = map[string]any{"helm": map[string]any{"releaseName": helmReleaseName}}
	}
	return map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": name, "namespace": "argocd", "annotations": annotations},
		"spec":     spec,
	}
}

func staticConfig(cfg *rest.Config) ConfigProvider {
	return func() (*rest.Config, error) { return cfg, nil }
}

// explodingConfigProvider returns a ConfigProvider that errors AND records
// whether it was called, via the returned *bool — used to prove a code path
// never needs a kube config at all.
func explodingConfigProvider() (ConfigProvider, *bool) {
	called := false
	return func() (*rest.Config, error) {
		called = true
		return nil, errors.New("ConfigProvider must not be called on this code path")
	}, &called
}

// --- Per-app happy path: a single matching, annotated Application ---

func TestRunPerAppAnnotatedMatchHappyPath(t *testing.T) {
	cpSrv := stubCP(t)
	defer cpSrv.Close()
	setEnv(t, cpSrv.URL)
	kube := stubKube(t, []map[string]any{
		appObj("my-app", "my-ns", map[string]any{
			"facets.cloud/project": "app-project", "facets.cloud/environment": "app-environment",
		}, ""), // no explicit releaseName -> falls back to metadata.name "my-app"
	})
	defer kube.Close()

	var out bytes.Buffer
	err := run(strings.NewReader(refStream), &out, "my-ns", "my-app", staticConfig(&rest.Config{Host: kube.URL}))
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "https://sqs.example/from-app") {
		t.Fatalf("expected the matched Application's coordinates (app-project/app-environment) to be used, got:\n%s", s)
	}
}

// --- Custom releaseName match: --name-template matches spec.source.helm.releaseName, not metadata.name ---

func TestRunPerAppMatchViaCustomReleaseName(t *testing.T) {
	kube := stubKube(t, []map[string]any{
		appObj("some-app-cr-name", "my-ns", map[string]any{
			"facets.cloud/project": "app-project", "facets.cloud/environment": "app-environment",
		}, "custom-release-name"),
	})
	defer kube.Close()
	cpSrv := stubCP(t)
	defer cpSrv.Close()
	setEnv(t, cpSrv.URL)

	var out bytes.Buffer
	// --name-template matches the CR's releaseName, NOT its metadata.name.
	err := run(strings.NewReader(refStream), &out, "my-ns", "custom-release-name", staticConfig(&rest.Config{Host: kube.URL}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "https://sqs.example/from-app") {
		t.Fatalf("expected match via custom releaseName, got:\n%s", out.String())
	}
}

// --- Matched Application missing both annotations: hard error naming the app and the missing keys ---

func TestRunPerAppMatchMissingAnnotationsHardError(t *testing.T) {
	kube := stubKube(t, []map[string]any{
		appObj("my-app", "my-ns", map[string]any{}, ""), // matches ns+name, but no annotations at all
	})
	defer kube.Close()
	cpSrv := stubCP(t)
	defer cpSrv.Close()
	setEnv(t, cpSrv.URL)

	err := run(strings.NewReader(refStream), &bytes.Buffer{}, "my-ns", "my-app", staticConfig(&rest.Config{Host: kube.URL}))
	if err == nil {
		t.Fatal("expected a hard error for a match missing both annotations")
	}
	for _, want := range []string{"argocd/my-app", annotationProject, annotationEnvironment} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

// --- Matched Application missing only ONE annotation: error names only that key ---

func TestRunPerAppMatchMissingOneAnnotationNamesOnlyThatOne(t *testing.T) {
	kube := stubKube(t, []map[string]any{
		appObj("my-app", "my-ns", map[string]any{
			"facets.cloud/project": "app-project", // environment annotation absent
		}, ""),
	})
	defer kube.Close()
	cpSrv := stubCP(t)
	defer cpSrv.Close()
	setEnv(t, cpSrv.URL)

	err := run(strings.NewReader(refStream), &bytes.Buffer{}, "my-ns", "my-app", staticConfig(&rest.Config{Host: kube.URL}))
	if err == nil {
		t.Fatal("expected a hard error for a match missing one annotation")
	}
	if !strings.Contains(err.Error(), annotationEnvironment) {
		t.Fatalf("error %q missing %q", err, annotationEnvironment)
	}
	if strings.Contains(err.Error(), "missing annotation(s): "+annotationProject) {
		t.Fatalf("error incorrectly also names the present annotation %q: %v", annotationProject, err)
	}
}

// --- Zero matches: hard error ---

func TestRunPerAppZeroMatchesHardError(t *testing.T) {
	kube := stubKube(t, []map[string]any{
		appObj("other-app", "other-ns", map[string]any{
			"facets.cloud/project": "app-project", "facets.cloud/environment": "app-environment",
		}, ""),
	})
	defer kube.Close()
	cpSrv := stubCP(t)
	defer cpSrv.Close()
	setEnv(t, cpSrv.URL)

	err := run(strings.NewReader(refStream), &bytes.Buffer{}, "my-ns", "my-app", staticConfig(&rest.Config{Host: kube.URL}))
	if err == nil {
		t.Fatal("expected a hard error when zero Applications match")
	}
	for _, want := range []string{"my-ns", "my-app", "argocd"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

// --- Two matches: hard, named ambiguity error ---

func TestRunPerAppAmbiguousMatchesHardError(t *testing.T) {
	kube := stubKube(t, []map[string]any{
		appObj("app-one", "my-ns", map[string]any{
			"facets.cloud/project": "app-project", "facets.cloud/environment": "app-environment",
		}, "my-app"),
		appObj("app-two", "my-ns", map[string]any{
			"facets.cloud/project": "app-project", "facets.cloud/environment": "app-environment",
		}, "my-app"),
	})
	defer kube.Close()
	cpSrv := stubCP(t)
	defer cpSrv.Close()
	setEnv(t, cpSrv.URL)

	err := run(strings.NewReader(refStream), &bytes.Buffer{}, "my-ns", "my-app", staticConfig(&rest.Config{Host: kube.URL}))
	if err == nil {
		t.Fatal("expected an ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err missing \"ambiguous\": %v", err)
	}
	for _, want := range []string{"argocd/app-one", "argocd/app-two"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err %q missing ambiguous app name %q", err, want)
		}
	}
}

// --- Kubernetes API error: hard error ---

func TestRunPerAppListAPIErrorHardError(t *testing.T) {
	kube := stubKubeError(t)
	defer kube.Close()
	cpSrv := stubCP(t)
	defer cpSrv.Close()
	setEnv(t, cpSrv.URL)

	err := run(strings.NewReader(refStream), &bytes.Buffer{}, "my-ns", "my-app", staticConfig(&rest.Config{Host: kube.URL}))
	if err == nil {
		t.Fatal("expected a hard error from the failed LIST")
	}
}

// --- Zero refs: provably no flags read, no client built ---

func TestRunZeroRefsPassesThroughUnescaped(t *testing.T) {
	in := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: demo\ndata:\n  plain: value\n  escaped: $${facets:a.b.out.c}\n"
	want := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: demo\ndata:\n  plain: value\n  escaped: ${facets:a.b.out.c}\n"
	provider, called := explodingConfigProvider()

	var out bytes.Buffer
	// --namespace/--name-template ARE given here, to prove the zero-ref
	// fast path skips the per-app lookup entirely regardless of whether the
	// flags were provided, not just when they're empty.
	if err := run(strings.NewReader(in), &out, "my-ns", "my-app", provider); err != nil {
		t.Fatal(err)
	}
	if *called {
		t.Fatal("ConfigProvider was invoked on the zero-ref passthrough path")
	}
	if out.String() != want {
		t.Fatalf("passthrough mismatch:\ngot:  %q\nwant: %q", out.String(), want)
	}
}

// --- Empty flags + refs present: hard error, no kube client ever attempted, aggregated with missing CP config ---

func TestRunEmptyFlagsFailsClosed(t *testing.T) {
	t.Setenv("FACETS_CP_URL", "")
	t.Setenv("FACETS_CP_USERNAME", "")
	t.Setenv("FACETS_CP_TOKEN", "")
	provider, called := explodingConfigProvider()

	err := run(strings.NewReader(refStream), &bytes.Buffer{}, "", "", provider)
	if err == nil {
		t.Fatal("expected error")
	}
	if *called {
		t.Fatal("ConfigProvider was invoked despite empty --namespace/--name-template flags")
	}
	for _, want := range []string{"--namespace", "--name-template", "FACETS_CP_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestRunMalformedRefFailsClosed(t *testing.T) {
	in := "data:\n  bad: ${facets:sqs.orders.out.attributes.}\n"
	provider, called := explodingConfigProvider()

	err := run(strings.NewReader(in), &bytes.Buffer{}, "my-ns", "my-app", provider)
	if err == nil || !strings.Contains(err.Error(), "invalid facets ref") {
		t.Fatalf("err = %v", err)
	}
	if *called {
		t.Fatal("ConfigProvider was invoked despite a malformed ref short-circuiting first")
	}
}

// --- releaseKey (key derivation) unit tests ---

func TestReleaseKeySingleSourceHelmReleaseName(t *testing.T) {
	obj := &unstructured.Unstructured{Object: appObj("app1", "prod-ns", nil, "my-release")}
	ns, name := releaseKey(obj)
	if ns != "prod-ns" || name != "my-release" {
		t.Fatalf("releaseKey = (%q, %q), want (prod-ns, my-release)", ns, name)
	}
}

func TestReleaseKeyMultiSourceHelmReleaseName(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "app2", "namespace": "argocd"},
		"spec": map[string]any{
			"destination": map[string]any{"namespace": "prod-ns"},
			"sources": []any{
				map[string]any{"repoURL": "https://example/values-repo.git"}, // no helm block at all
				map[string]any{"helm": map[string]any{"releaseName": "multi-release"}},
			},
		},
	}}
	ns, name := releaseKey(obj)
	if ns != "prod-ns" || name != "multi-release" {
		t.Fatalf("releaseKey = (%q, %q), want (prod-ns, multi-release)", ns, name)
	}
}

func TestReleaseKeyMultiSourceSkipsEmptyReleaseNames(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "app3", "namespace": "argocd"},
		"spec": map[string]any{
			"destination": map[string]any{"namespace": "prod-ns"},
			"sources": []any{
				map[string]any{"helm": map[string]any{"releaseName": ""}},
				map[string]any{"helm": map[string]any{}},
				map[string]any{"helm": map[string]any{"releaseName": "second-source-release"}},
			},
		},
	}}
	ns, name := releaseKey(obj)
	if ns != "prod-ns" || name != "second-source-release" {
		t.Fatalf("releaseKey = (%q, %q), want (prod-ns, second-source-release)", ns, name)
	}
}

func TestReleaseKeyFallsBackToAppName(t *testing.T) {
	obj := &unstructured.Unstructured{Object: appObj("my-app-name", "prod-ns", nil, "")}
	ns, name := releaseKey(obj)
	if ns != "prod-ns" || name != "my-app-name" {
		t.Fatalf("releaseKey = (%q, %q), want (prod-ns, my-app-name)", ns, name)
	}
}

func TestReleaseKeySingleSourceWinsOverMultiSource(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "app4", "namespace": "argocd"},
		"spec": map[string]any{
			"destination": map[string]any{"namespace": "prod-ns"},
			"source":      map[string]any{"helm": map[string]any{"releaseName": "single-wins"}},
			"sources": []any{
				map[string]any{"helm": map[string]any{"releaseName": "should-not-be-used"}},
			},
		},
	}}
	ns, name := releaseKey(obj)
	if ns != "prod-ns" || name != "single-wins" {
		t.Fatalf("releaseKey = (%q, %q), want (prod-ns, single-wins)", ns, name)
	}
}
