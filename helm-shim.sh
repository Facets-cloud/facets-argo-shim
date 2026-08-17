#!/bin/sh
# shim/helm-shim.sh
#
# Drop-in replacement for the `helm` binary inside argocd-repo-server (see
# README.md for the full repo-server wiring). Every invocation is passed
# straight through to the REAL helm binary UNCHANGED, except `helm
# template`: on success, its stdout (the rendered manifest stream) is piped
# through `facets-resolver`, which resolves any ${facets:...} refs the
# manifests contain — via a live, per-render lookup against Application CRs
# (see coordinates.go), never a shared file or separate daemon — before Argo
# CD ever sees them. Real helm's stderr is never touched, in either path —
# only stdout is ever intercepted, and only for `template`, and only when
# real helm itself succeeded (its exit code is preserved exactly on failure,
# and nothing is resolved/emitted from a failed/partial render).
#
# POSIX sh only — no bashisms (no arrays, no [[ ]], no PIPESTATUS). Runs as
# whatever minimal shell is available in the repo-server image.
set -eu

REAL_HELM=${REAL_HELM:-/custom-tools/helm-real}
FACETS_RESOLVER_BIN=${FACETS_RESOLVER_BIN:-/custom-tools/facets-resolver}

# Find the first non-flag argument: helm's subcommand. A flag is anything
# starting with "-". (Known simplification: this does not special-case a
# global flag that takes a separate value, e.g. `--kube-context foo` — the
# "foo" would be mistaken for the subcommand. Argo CD's own repo-server
# invocation always calls `helm template <name> <path> [flags...]` with the
# subcommand as the very first argument, so this doesn't arise in practice
# for this shim's actual use case.)
#
# Also extract --name-template/--namespace's OWN values from the same argv,
# to forward to facets-resolver's --namespace/--name-template flags for its
# per-Application live lookup — a live probe of a real argocd-repo-server
# found both always present on a `helm template` invocation
# (space-separated, not "--flag=value" form, but both forms are handled here
# for robustness), even though no ARGOCD_APP_* env vars exist for a builtin
# (non-CMP) Helm render. Either may legitimately be absent; an empty value is
# passed through to facets-resolver either way, which treats empty as "skip
# the per-Application lookup, use the FACETS_PROJECT/FACETS_ENVIRONMENT
# fallback".
subcommand=""
resolve_namespace=""
resolve_name_template=""
prev=""
for arg in "$@"; do
	if [ "$subcommand" = "" ]; then
		case "$arg" in
			-*) ;;
			*) subcommand="$arg" ;;
		esac
	fi
	case "$prev" in
		--namespace) resolve_namespace="$arg" ;;
		--name-template) resolve_name_template="$arg" ;;
	esac
	case "$arg" in
		--namespace=*) resolve_namespace="${arg#--namespace=}" ;;
		--name-template=*) resolve_name_template="${arg#--name-template=}" ;;
	esac
	prev="$arg"
done

if [ "$subcommand" != "template" ]; then
	# Every other subcommand (version, dependency, pull, registry, ...):
	# exec real helm untouched — stdout, stderr, and exit code all pass
	# through natively, since exec replaces this process entirely.
	exec "$REAL_HELM" "$@"
fi

# `helm template`: run real helm, capturing only its stdout to a temp file.
# stderr is left connected to this script's own inherited stderr throughout
# (never redirected), so it reaches the real stderr completely untouched, on
# both success and failure below.
tmp_out=$(mktemp)
trap 'rm -f "$tmp_out"' EXIT INT TERM

if "$REAL_HELM" "$@" >"$tmp_out"; then
	# Real helm succeeded: pipe its captured stdout through facets-resolver,
	# forwarding the --namespace/--name-template values extracted above
	# (either may be empty; facets-resolver treats that as "skip the
	# per-Application lookup"). Its own exit code becomes this script's exit
	# code (the last command run), which is what argocd-repo-server sees as
	# "helm template"'s result.
	"$FACETS_RESOLVER_BIN" --namespace "$resolve_namespace" --name-template "$resolve_name_template" <"$tmp_out"
else
	# Real helm failed: preserve its exit code exactly. Never attempt to
	# resolve/emit its (possibly partial or empty) stdout.
	status=$?
	exit "$status"
fi
