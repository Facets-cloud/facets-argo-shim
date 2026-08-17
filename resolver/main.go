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
// --namespace/--name-template first, falling back to
// FACETS_PROJECT/FACETS_ENVIRONMENT.
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
		_, err := stdout.Write([]byte(Unescape(string(input))))
		return err
	}

	project, environment, coordErrs := resolveCoordinates(namespace, nameTemplate, getKubeCfg)

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

	lookup := NewCPClient(cpCfg).Lookup(project, environment)
	resolved, err := ResolveStream(input, lookup)
	if err != nil {
		return err
	}
	_, err = stdout.Write(resolved)
	return err
}
