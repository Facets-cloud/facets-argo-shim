// coordinates.go
// Facets project/environment coordinate resolution for shim mode. Per-app
// annotations are the ONLY coordinate source — a live, lazy Kubernetes LIST
// lookup, not a shared file, not a separate daemon, and no repo-server-wide
// fallback of any kind.
//
// The shim forwards --namespace/--name-template (extracted from real helm's
// own argv — a live probe of a real argocd-repo-server found
// `--name-template <name>` and `--namespace <ns>` always present in the
// exec'd argv, even though no ARGOCD_APP_* env vars exist for a builtin,
// non-CMP Helm render). Whenever the rendered stream contains any
// ${facets:...} ref, both flags are required: this builds an in-cluster kube
// client — lazily, only at this point, never on the zero-ref path — and
// LISTs Application CRs in FACETS_ARGOCD_NAMESPACE, looking for exactly one
// whose spec.destination.namespace/effective release name match and which
// carries both facets.cloud/project and facets.cloud/environment
// annotations.
//
// Every failure mode is a hard, fail-closed error — there is no fallback to
// silently resolve against: missing flags, a Kubernetes API error, zero
// matching Applications, a match missing one or both annotations, or an
// AMBIGUOUS match (more than one Application with the same destination
// namespace + effective release name) all abort the render.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

const (
	annotationProject     = "facets.cloud/project"
	annotationEnvironment = "facets.cloud/environment"
)

var appGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// ConfigProvider builds the in-cluster kube config needed for the
// per-Application LIST lookup. It's called at most once, and only when
// --namespace and --name-template are both non-empty AND a ref has actually
// been found in the rendered stream — never on the zero-ref passthrough
// path, and never when either flag is empty.
type ConfigProvider func() (*rest.Config, error)

// resolveCoordinates resolves Facets project/environment via the sole
// mechanism described in the package doc: a live per-Application LIST
// lookup, keyed on namespace+nameTemplate. Both must be non-empty — refs are
// present in the stream (the only case this is called for), so there is no
// meaningful fallback identity to resolve against, and an empty flag is
// itself a hard, immediate error (no kube client is ever built for it).
func resolveCoordinates(namespace, nameTemplate string, getKubeCfg ConfigProvider) (project, environment string, errs []error) {
	if namespace == "" || nameTemplate == "" {
		return "", "", []error{errors.New("rendered manifests contain ${facets:...} refs but --namespace and --name-template were not both provided; per-Application coordinate resolution requires both to identify the Application")}
	}

	argoNS := os.Getenv("FACETS_ARGOCD_NAMESPACE")
	if argoNS == "" {
		argoNS = "argocd"
	}
	p, e, err := perAppCoordinates(getKubeCfg, argoNS, namespace, nameTemplate)
	if err != nil {
		return "", "", []error{err}
	}
	return p, e, nil
}

// perAppCoordinates lists Application CRs in argoNS and looks for exactly
// one whose spec.destination.namespace and effective release name (see
// releaseKey) equal destNamespace/nameTemplate. Every non-exactly-one
// outcome is a hard error: a Kubernetes API error, zero matches, a match
// missing one or both facets.cloud annotations, or more than one match
// (ambiguous).
func perAppCoordinates(getKubeCfg ConfigProvider, argoNS, destNamespace, nameTemplate string) (project, environment string, err error) {
	cfg, err := getKubeCfg()
	if err != nil {
		return "", "", fmt.Errorf("in-cluster kube config: %w", err)
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return "", "", fmt.Errorf("building dynamic client: %w", err)
	}
	list, err := dc.Resource(appGVR).Namespace(argoNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return "", "", fmt.Errorf("listing Applications in %s: %w", argoNS, err)
	}

	var matches []*unstructured.Unstructured
	for i := range list.Items {
		item := &list.Items[i]
		ns, name := releaseKey(item)
		if ns == destNamespace && name == nameTemplate {
			matches = append(matches, item)
		}
	}

	switch len(matches) {
	case 0:
		return "", "", fmt.Errorf("no Application found with destination namespace %q and release name %q in %s", destNamespace, nameTemplate, argoNS)
	case 1:
		app := matches[0]
		ann := app.GetAnnotations()
		p := ann[annotationProject]
		e := ann[annotationEnvironment]
		var missing []string
		if p == "" {
			missing = append(missing, annotationProject)
		}
		if e == "" {
			missing = append(missing, annotationEnvironment)
		}
		if len(missing) > 0 {
			return "", "", fmt.Errorf("Application %s/%s matches destination namespace %q and release name %q but is missing annotation(s): %s",
				app.GetNamespace(), app.GetName(), destNamespace, nameTemplate, strings.Join(missing, ", "))
		}
		return p, e, nil
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.GetNamespace()+"/"+m.GetName())
		}
		return "", "", fmt.Errorf("ambiguous: %d Applications in %s match destination namespace %q and release name %q: %s", len(matches), argoNS, destNamespace, nameTemplate, strings.Join(names, ", "))
	}
}

// releaseKey derives an Application's destination namespace and effective
// release name — the same two-part key the shim's --namespace/
// --name-template flags carry, so a live List result can be matched against
// them directly. Pure and side-effect-free (works on an already-fetched
// object), so it's directly unit-testable against fake objects.
//
// name precedence: spec.source.helm.releaseName (single-source), if set and
// non-empty; else the first non-empty spec.sources[].helm.releaseName
// (multi-source); else metadata.name (the Application's own name).
func releaseKey(obj *unstructured.Unstructured) (destNamespace, name string) {
	destNamespace, _, _ = unstructured.NestedString(obj.Object, "spec", "destination", "namespace")

	if rn, ok, _ := unstructured.NestedString(obj.Object, "spec", "source", "helm", "releaseName"); ok && rn != "" {
		return destNamespace, rn
	}

	if sources, ok, _ := unstructured.NestedSlice(obj.Object, "spec", "sources"); ok {
		for _, s := range sources {
			sm, ok := s.(map[string]any)
			if !ok {
				continue
			}
			helm, ok := sm["helm"].(map[string]any)
			if !ok {
				continue
			}
			if rn, ok := helm["releaseName"].(string); ok && rn != "" {
				return destNamespace, rn
			}
		}
	}

	return destNamespace, obj.GetName()
}
