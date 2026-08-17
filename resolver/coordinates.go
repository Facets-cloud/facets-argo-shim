// coordinates.go
// Facets project/environment coordinate resolution for shim mode. Comes
// from one of two places, in order:
//
//  1. Per-Application, via a live lookup — not a shared file, not a separate
//     daemon. The shim forwards --namespace/--name-template (extracted from
//     real helm's own argv — a live probe of a real argocd-repo-server found
//     `--name-template <name>` and `--namespace <ns>` always present in the
//     exec'd argv, even though no ARGOCD_APP_* env vars exist for a
//     builtin, non-CMP Helm render). If both are non-empty, this builds an
//     in-cluster kube client — lazily, only at this point, never on the
//     zero-ref or flags-absent paths — and LISTs Application CRs in
//     FACETS_ARGOCD_NAMESPACE, looking for exactly one whose
//     spec.destination.namespace/effective release name match and which
//     carries both facets.cloud/project and facets.cloud/environment
//     annotations.
//  2. Repo-server-wide fallback: FACETS_PROJECT/FACETS_ENVIRONMENT, set
//     once on the argocd-repo-server deployment — one Argo CD (repo-server)
//     mapped to one Facets project+environment context for every
//     Application it renders that isn't (or can't be) resolved per-app.
//
// A per-Application lookup that runs but fails outright — a Kubernetes API
// error, or an AMBIGUOUS match (more than one Application with the same
// destination namespace + effective release name) — is a hard, fail-closed
// error: it never silently falls through to the env-var fallback, since a
// transient API blip resolving against the wrong environment's fallback
// would be worse than failing loudly. Only a *clean* non-match (zero
// matching Applications, or a match that simply doesn't carry both
// annotations) falls through to (2).
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
// path, and never when the flags are absent.
type ConfigProvider func() (*rest.Config, error)

// resolveCoordinates implements the resolution order documented above: (1) a
// live per-Application LIST lookup, attempted only when namespace and
// nameTemplate are both non-empty; a hard error there (API error, or an
// ambiguous multi-match) returns immediately and never falls through to (2).
// A clean non-match (zero matches, or a match without both annotations)
// falls through normally to (2) FACETS_PROJECT/FACETS_ENVIRONMENT. If
// neither yields usable coordinates, every reason why is returned: the
// per-app non-match (if that path was attempted) and/or which env var(s)
// are unset.
func resolveCoordinates(namespace, nameTemplate string, getKubeCfg ConfigProvider) (project, environment string, errs []error) {
	attemptedPerApp := false
	if namespace != "" && nameTemplate != "" {
		attemptedPerApp = true
		argoNS := os.Getenv("FACETS_ARGOCD_NAMESPACE")
		if argoNS == "" {
			argoNS = "argocd"
		}
		p, e, found, err := perAppCoordinates(getKubeCfg, argoNS, namespace, nameTemplate)
		if err != nil {
			// Hard error: fail closed, never fall through to the env
			// fallback below — a transient API blip must not silently
			// resolve against the wrong (repo-server-wide) environment.
			return "", "", []error{err}
		}
		if found {
			return p, e, nil
		}
	}

	envProject := os.Getenv("FACETS_PROJECT")
	envEnvironment := os.Getenv("FACETS_ENVIRONMENT")
	if envProject != "" && envEnvironment != "" {
		return envProject, envEnvironment, nil
	}

	if attemptedPerApp {
		errs = append(errs, fmt.Errorf("rendered manifests contain ${facets:...} refs but no Application with both facets.cloud annotations matches destination namespace %q and release name %q", namespace, nameTemplate))
	}
	if envProject == "" {
		errs = append(errs, errors.New("rendered manifests contain ${facets:...} refs but FACETS_PROJECT is not set"))
	}
	if envEnvironment == "" {
		errs = append(errs, errors.New("rendered manifests contain ${facets:...} refs but FACETS_ENVIRONMENT is not set"))
	}
	return "", "", errs
}

// perAppCoordinates lists Application CRs in argoNS and looks for exactly
// one whose spec.destination.namespace and effective release name (see
// releaseKey) equal destNamespace/nameTemplate. found=false, err=nil means
// "no matching Application, or a match without both facets.cloud
// annotations" — a clean non-match the caller should fall through on. Any
// API error, or more than one match, is returned as a non-nil err — a hard
// stop, not a clean non-match.
func perAppCoordinates(getKubeCfg ConfigProvider, argoNS, destNamespace, nameTemplate string) (project, environment string, found bool, err error) {
	cfg, err := getKubeCfg()
	if err != nil {
		return "", "", false, fmt.Errorf("in-cluster kube config: %w", err)
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return "", "", false, fmt.Errorf("building dynamic client: %w", err)
	}
	list, err := dc.Resource(appGVR).Namespace(argoNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return "", "", false, fmt.Errorf("listing Applications in %s: %w", argoNS, err)
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
		return "", "", false, nil
	case 1:
		ann := matches[0].GetAnnotations()
		p := ann[annotationProject]
		e := ann[annotationEnvironment]
		if p == "" || e == "" {
			return "", "", false, nil
		}
		return p, e, true, nil
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.GetNamespace()+"/"+m.GetName())
		}
		return "", "", false, fmt.Errorf("ambiguous: %d Applications in %s match destination namespace %q and release name %q: %s", len(matches), argoNS, destNamespace, nameTemplate, strings.Join(names, ", "))
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
