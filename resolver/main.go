// main.go
// facets-resolver: reads an already-rendered, multi-document Kubernetes
// manifest stream from stdin, resolves every ${facets:<type>.<name>.out.
// <path...>} reference against a Facets control plane, and writes the
// resolved stream to stdout.
//
// This binary exists to be piped into by shim/helm-shim.sh, which installs
// in place of the real `helm` binary inside argocd-repo-server: standard
// Argo Applications using Argo CD's builtin Helm source (including
// multi-source apps with a separate $values valueFiles ref) never go
// through a Config Management Plugin, so there's no other hook point to
// resolve refs at. The shim intercepts `helm template`'s own stdout and
// pipes it through this binary before Argo CD ever sees it — bringing
// facets ref resolution to every rendered manifest regardless of how the
// Application is wired, with zero Application-spec changes required.
//
// Facets project/environment coordinates are resolved per coordinates.go's
// package doc: a lazy, per-Application live LIST lookup keyed on
// --namespace/--name-template — the only coordinate source; there is no
// repo-server-wide fallback.
//
// v0.13: when the matched Application also carries the three optional
// facets.cloud/resource-* callback annotations, a successful resolution
// additionally reports every consumed ${facets:...} expression back to the
// blueprint resource that owns it — see callback.go. That reporting is
// best-effort and never affects this binary's exit code or stdout; see
// run's own doc comment for exactly where that boundary is.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"k8s.io/client-go/rest"
)

func main() {
	namespace := flag.String("namespace", "", "destination namespace, from the shim's own argv (optional)")
	nameTemplate := flag.String("name-template", "", "release/app name, from the shim's own argv (optional)")
	flag.Parse()

	if err := run(os.Stdin, os.Stdout, *namespace, *nameTemplate, rest.InClusterConfig); err != nil {
		// stderr is surfaced by argocd-repo-server as if `helm template`
		// itself had failed (the shim never touches real helm's own stderr,
		// but this binary's errors go to our own stderr the same way).
		fmt.Fprintf(os.Stderr, "facets-resolver: %v\n", err)
		os.Exit(1)
	}
}

// run is the whole entry flow, factored out of main for testability: read
// stdin; zero refs found -> Unescape-only passthrough (no coordinates, no
// CP, no kube client of any kind — the common case, so the overwhelming
// majority of `helm template` invocations pay no cost beyond the scan
// itself); refs present -> resolve coordinates (see coordinates.go),
// resolve every ref against the Facets control plane (cpclient.go), and
// write the resolved stream to stdout. Fail closed throughout: a malformed
// ref, unresolvable coordinates, or missing CP config are all hard errors —
// nothing partially resolved ever reaches stdout.
//
// The one deliberate exception is the v0.13 consumed-references callback
// (callback.go), fired after ResolveStream has already succeeded, when the
// matched Application opted in (cb != nil): its errors are logged as a
// stderr warning and swallowed, never joined into this function's returned
// error and never capable of changing the exit code or the stdout write
// below. The manifests already resolved correctly; a failure reporting that
// fact back to the blueprint is not a reason to fail the deploy.
func run(stdin io.Reader, stdout io.Writer, namespace, nameTemplate string, getKubeCfg ConfigProvider) error {
	input, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading rendered manifest stream: %w", err)
	}

	found, findErr := Find(string(input))
	if findErr != nil {
		// Malformed refs are a hard, standalone failure — no coordinate
		// resolution or CP config is needed to know the stream is broken.
		return findErr
	}
	if len(found) == 0 {
		// No refs anywhere: pure passthrough, zero CP interaction, no env
		// reads, and getKubeCfg is never called — namespace/nameTemplate are
		// ignored entirely on this path. Still unescape any $$-escaped
		// literals — a safe no-op string replace when there's nothing to
		// unescape.
		//
		// Guard first: refPattern's stricter grammar (see resolver.go) means
		// an unterminated "${facets:" sequence, or one using characters
		// outside type.name.out.path, no longer counts as a "found" ref at
		// all — without this check it would silently reach stdout verbatim
		// instead of failing closed like every other malformed ref does.
		raw := string(input)
		if snippet, bad := findUnresolvedRef(raw); bad {
			return fmt.Errorf("unresolved or malformed facets reference remains: %q", snippet)
		}
		_, err := stdout.Write([]byte(Unescape(raw)))
		return err
	}

	project, environment, cb, coordErrs := resolveCoordinates(namespace, nameTemplate, getKubeCfg)

	// Aggregate coordinate-resolution problems with the CP config check into
	// one error.
	setupErrs := append([]error{}, coordErrs...)
	cpCfg, cfgErr := ConfigFromEnv()
	if cfgErr != nil {
		setupErrs = append(setupErrs, cfgErr)
	}
	if len(setupErrs) > 0 {
		return errors.Join(setupErrs...)
	}

	cpClient := NewCPClient(cpCfg)
	resolved, err := ResolveStream(input, cpClient.Lookup(project, environment))
	if err != nil {
		return err
	}

	// v0.13 best-effort callback — see this function's doc comment above and
	// callback.go for why an error here is only ever a warning.
	if cb != nil {
		if err := reportConsumedReferences(cpClient, project, environment, *cb, found); err != nil {
			fmt.Fprintf(os.Stderr, "facets-resolver: warning: reporting consumed references to blueprint resource %s/%s failed (render succeeded regardless): %v\n",
				cb.ResourceType, cb.ResourceName, err)
		}
	}

	_, err = stdout.Write(resolved)
	return err
}
