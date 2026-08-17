// helm_shim_test.go
//
// Shell-level tests for helm-shim.sh: exercises the real script with `sh`
// against fake REAL_HELM and FACETS_RESOLVER_BIN stand-ins, proving
// `template` gets piped through facets-resolver and every other subcommand
// doesn't — without needing a real helm binary or Facets control plane.
package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireSh finds a POSIX shell to run the script under. Skips (with a
// reason) only if none is available at all, which should essentially never
// happen — sh is a baseline assumption for running this shim in the first
// place.
func requireSh(t *testing.T) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found on PATH; cannot exercise helm-shim.sh")
	}
	return sh
}

// writeFakeHelm writes a fake "real helm" binary: a shell script that always
// emits a known, recognizable manifest stream on stdout and a known marker
// on stderr, regardless of arguments, then exits successfully.
func writeFakeHelm(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "helm-real")
	script := "#!/bin/sh\necho 'fake helm stderr' >&2\necho 'FAKE_HELM_OUTPUT'\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeFailingFakeHelm is like writeFakeHelm but exits non-zero after
// emitting a distinct stderr marker and partial stdout, to exercise the
// shim's failure path.
func writeFailingFakeHelm(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "helm-real-fail")
	script := "#!/bin/sh\necho 'fake helm stderr failure' >&2\necho 'PARTIAL_OUTPUT'\nexit 17\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeFakeResolver writes a fake "facets-resolver" binary: reads stdin and
// writes it back prefixed with a marker, so a test can tell whether the shim
// actually piped helm's stdout through it.
func writeFakeResolver(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "facets-resolver")
	script := "#!/bin/sh\n" +
		"printf 'RESOLVED:'\n" +
		"cat\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeFakeResolverRecordingArgs is like writeFakeResolver but additionally
// records the exact argv it was invoked with to the file named by the
// RECORD_FILE env var (set by the caller via runShimWithEnv), so a test can
// assert on exactly how the shim invoked facets-resolver — in particular,
// whether --namespace/--name-template were forwarded and with what values.
func writeFakeResolverRecordingArgs(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "facets-resolver")
	script := "#!/bin/sh\n" +
		"if [ -n \"${RECORD_FILE:-}\" ]; then echo \"$*\" > \"$RECORD_FILE\"; fi\n" +
		"printf 'RESOLVED:'\n" +
		"cat\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// runShim runs helm-shim.sh (at the repo root) under sh with the given
// REAL_HELM/FACETS_RESOLVER_BIN overrides and arguments, capturing stdout,
// stderr, and exit code separately.
func runShim(t *testing.T, sh, realHelm, resolverBin string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return runShimWithEnv(t, sh, realHelm, resolverBin, nil, args...)
}

// runShimWithEnv is runShim with additional environment variables (e.g.
// RECORD_FILE for writeFakeResolverRecordingArgs) set on the shim process.
func runShimWithEnv(t *testing.T, sh, realHelm, resolverBin string, extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	shimPath, err := filepath.Abs("../helm-shim.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, append([]string{shimPath}, args...)...)
	cmd.Env = append(append(os.Environ(), "REAL_HELM="+realHelm, "FACETS_RESOLVER_BIN="+resolverBin), extraEnv...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		exitErr, ok := runErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("running shim: %v (stderr: %s)", runErr, errBuf.String())
		}
		code = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), code
}

// `helm template` gets piped through facets-resolver.
func TestShimPipesTemplateThroughResolve(t *testing.T) {
	sh := requireSh(t)
	dir := t.TempDir()
	realHelm := writeFakeHelm(t, dir)
	resolverBin := writeFakeResolver(t, dir)

	stdout, stderr, code := runShim(t, sh, realHelm, resolverBin, "template", "demo", ".")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "RESOLVED:") || !strings.Contains(stdout, "FAKE_HELM_OUTPUT") {
		t.Fatalf("expected helm's stdout piped through facets-resolver, got stdout:\n%q", stdout)
	}
	if !strings.Contains(stderr, "fake helm stderr") {
		t.Fatalf("expected helm's stderr to pass through untouched, got:\n%q", stderr)
	}
}

// `helm version` (and, by the same code path, any other subcommand) does
// NOT get piped through facets-resolver — raw helm output reaches stdout
// unchanged.
func TestShimDoesNotPipeOtherSubcommands(t *testing.T) {
	sh := requireSh(t)
	dir := t.TempDir()
	realHelm := writeFakeHelm(t, dir)
	resolverBin := writeFakeResolver(t, dir)

	stdout, stderr, code := runShim(t, sh, realHelm, resolverBin, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr)
	}
	if strings.Contains(stdout, "RESOLVED:") {
		t.Fatalf("`helm version` must NOT be piped through facets-resolver, got:\n%q", stdout)
	}
	if !strings.Contains(stdout, "FAKE_HELM_OUTPUT") {
		t.Fatalf("expected raw fake helm output, got:\n%q", stdout)
	}
	if !strings.Contains(stderr, "fake helm stderr") {
		t.Fatalf("expected helm's stderr to pass through untouched, got:\n%q", stderr)
	}
}

// A failed `helm template` (non-zero exit) must not be piped through
// facets-resolver at all, and the shim must preserve real helm's exact exit
// code and stderr.
func TestShimPreservesHelmFailureExitCode(t *testing.T) {
	sh := requireSh(t)
	dir := t.TempDir()
	realHelm := writeFailingFakeHelm(t, dir)
	resolverBin := writeFakeResolver(t, dir)

	stdout, stderr, code := runShim(t, sh, realHelm, resolverBin, "template", "demo", ".")
	if code != 17 {
		t.Fatalf("exit code = %d, want 17; stdout:\n%q\nstderr:\n%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "RESOLVED:") {
		t.Fatalf("must not attempt to resolve helm's output on failure, got stdout:\n%q", stdout)
	}
	if !strings.Contains(stderr, "fake helm stderr failure") {
		t.Fatalf("expected helm's stderr to pass through untouched on failure, got:\n%q", stderr)
	}
}

// helm's own argv (as passed straight through to the shim, since the shim
// IS installed in place of `helm`) carries --name-template/--namespace, per
// a live probe of a real argocd-repo-server's builtin-Helm exec environment
// — the shim must extract their values and forward them to facets-resolver
// as --namespace/--name-template flags.
func TestShimForwardsNameTemplateAndNamespaceFlags(t *testing.T) {
	sh := requireSh(t)
	dir := t.TempDir()
	realHelm := writeFakeHelm(t, dir)
	resolverBin := writeFakeResolverRecordingArgs(t, dir)
	recordFile := filepath.Join(dir, "record.txt")

	stdout, stderr, code := runShimWithEnv(t, sh, realHelm, resolverBin, []string{"RECORD_FILE=" + recordFile},
		"template", "demo", ".", "--name-template", "myapp", "--namespace", "myns")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "RESOLVED:") {
		t.Fatalf("expected template to still be piped through facets-resolver, got stdout:\n%q", stdout)
	}
	recorded, err := os.ReadFile(recordFile)
	if err != nil {
		t.Fatalf("facets-resolver was not invoked (no record file written): %v", err)
	}
	got := strings.TrimSpace(string(recorded))
	if !strings.Contains(got, "--namespace myns") {
		t.Fatalf("facets-resolver args %q missing --namespace myns", got)
	}
	if !strings.Contains(got, "--name-template myapp") {
		t.Fatalf("facets-resolver args %q missing --name-template myapp", got)
	}
}

// When helm's argv does NOT carry --name-template/--namespace at all, the
// shim must still work (both flags forwarded as empty strings, not omitted
// or erroring) — the flags are optional both in the shim and in
// facets-resolver.
func TestShimForwardsEmptyFlagsWhenArgvLacksThem(t *testing.T) {
	sh := requireSh(t)
	dir := t.TempDir()
	realHelm := writeFakeHelm(t, dir)
	resolverBin := writeFakeResolverRecordingArgs(t, dir)
	recordFile := filepath.Join(dir, "record.txt")

	stdout, stderr, code := runShimWithEnv(t, sh, realHelm, resolverBin, []string{"RECORD_FILE=" + recordFile},
		"template", "demo", ".")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "RESOLVED:") {
		t.Fatalf("expected template to still be piped through facets-resolver, got stdout:\n%q", stdout)
	}
	recorded, err := os.ReadFile(recordFile)
	if err != nil {
		t.Fatalf("facets-resolver was not invoked (no record file written): %v", err)
	}
	got := strings.TrimSpace(string(recorded))
	if got != "--namespace  --name-template" {
		t.Fatalf("facets-resolver args = %q, want %q (both flags present with empty values)", got, "--namespace  --name-template")
	}
}
