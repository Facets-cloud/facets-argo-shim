// callback.go
// v0.13 post-render callback: after a render successfully resolves, report
// every distinct ${facets:...} expression it consumed back to the blueprint
// resource that owns the Application it rendered, in native blueprint
// expression syntax (see Ref.nativeExpr in resolver.go). The idea: the
// resource's own module can then evaluate those same expressions as native
// blueprint inputs and fold the resolved values into the Application CR as
// static spec fields — so a Facets release that changes any of them mutates
// the CR itself, Argo re-renders naturally on its own watch of that CR, and
// this shim needs no refresh/webhook machinery of its own to keep up.
//
// This is opt-in per Application (see CallbackTarget in coordinates.go —
// all three facets.cloud/resource-* annotations must be present) and is
// pure best-effort reporting bolted onto an already-successful render: see
// the doc comment on reportConsumedReferences below, and main.go's run, for
// the failure semantics — DIFFERENT from everything else in this codebase.
// Every other failure mode here fails the whole render closed; a callback
// failure never does. It only ever logs a warning to stderr.
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
)

// reportConsumedReferences is the whole v0.13 callback. It:
//  1. Normalizes every ref in refs to native blueprint syntax, deduplicates,
//     and sorts lexicographically (normalizeExpressions) for a deterministic
//     payload.
//  2. Reads the current content of the blueprint resource named by cb from
//     the control plane.
//  3. Compares the desired {"expressions": [...]} value against whatever is
//     already at spec[cb.ReferencesField][environment] — keyed by
//     environment name, since a resource shared across environments (the
//     usual case: one blueprint resource, many Argo Applications, one per
//     environment) needs a different expression set per environment, not
//     one shared value. If that one environment's section already matches
//     exactly, this does nothing at all — no write, no commit, no-op. Every
//     OTHER environment's section already present under
//     spec[cb.ReferencesField] is left completely untouched — this is a
//     merge at the environment key, never a wholesale replacement of the
//     field.
//  4. Otherwise writes the updated resource content back, with a commit
//     message naming the Application that triggered it.
//
// FAILURE SEMANTICS — read this before changing anything here: unlike every
// other failure in this codebase (which fails the render closed), any error
// this function returns is meant to be treated as a WARNING by the caller,
// never a reason to fail the render. The manifests this process already
// resolved are correct and complete regardless of whether this bookkeeping
// call succeeds — reporting must never break a deploy. See main.go's run,
// which is the only caller and is where that "log and continue" behavior
// actually lives; this function itself is a normal function that returns a
// normal error, specifically so tests can assert on it precisely.
func reportConsumedReferences(client *CPClient, project, environment string, cb CallbackTarget, refs []Ref) error {
	desiredForEnv := map[string]any{"expressions": toAnySlice(normalizeExpressions(refs))}

	resource, err := fetchResourceContent(client, project, cb.ResourceType, cb.ResourceName)
	if err != nil {
		return fmt.Errorf("reading blueprint resource %s/%s: %w", cb.ResourceType, cb.ResourceName, err)
	}

	spec, ok := resource["spec"].(map[string]any)
	if !ok {
		spec = map[string]any{}
		resource["spec"] = spec
	}

	// byEnv holds every environment's section, keyed by environment name.
	// Only ever mutated at the [environment] key below — every other
	// environment already present survives untouched, including when this
	// process didn't understand the field's existing shape at all (e.g. an
	// older, pre-v0.13 flat {"expressions": [...]} value with no
	// environment keys): in that case byEnv starts empty here rather than
	// erroring, and this write replaces that old flat shape with the new
	// per-environment one going forward.
	byEnv, _ := spec[cb.ReferencesField].(map[string]any)
	if byEnv == nil {
		byEnv = map[string]any{}
	}

	if reflect.DeepEqual(byEnv[environment], desiredForEnv) {
		return nil // this environment's section is already up to date: no write, no commit
	}
	byEnv[environment] = desiredForEnv
	spec[cb.ReferencesField] = byEnv

	if err := writeResourceContent(client, project, cb.ResourceType, cb.ResourceName, resource, cb.AppName); err != nil {
		return fmt.Errorf("writing blueprint resource %s/%s: %w", cb.ResourceType, cb.ResourceName, err)
	}
	return nil
}

// normalizeExpressions converts every ref actually present in the rendered
// stream into its native blueprint expression syntax, deduplicated and
// sorted lexicographically — the exact payload shape this callback reports.
func normalizeExpressions(refs []Ref) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range refs {
		expr := r.nativeExpr()
		if seen[expr] {
			continue
		}
		seen[expr] = true
		out = append(out, expr)
	}
	sort.Strings(out)
	return out
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// fetchResourceContent fetches the full content document of one blueprint
// resource. Endpoint and shape verified against raptor's own CLI source
// (cmd/resources.go's getResourceByProject/extractResourceFromRaw): the
// project-level resources-info dropdown endpoint with includeContent=true
// returns every resource in the project, each with its content as a raw
// JSON STRING (not a nested object) in the "content" field — this parses
// that string into the actual resource document (top-level "kind",
// "flavor", "spec", etc., matching what `raptor apply` itself would send).
// Decoded with UseNumber (see cpclient.go's get) so any numeric field
// elsewhere in the resource's own spec survives this read-modify-write
// round-trip exactly, not just the one field this callback actually
// changes.
func fetchResourceContent(client *CPClient, project, resourceType, resourceName string) (map[string]any, error) {
	path := fmt.Sprintf("/cc-ui/v1/dropdown/stack/%s/resources-info?includeContent=true&excludeAddOns=false", url.PathEscape(project))
	var raw []map[string]any
	if err := client.get(path, &raw); err != nil {
		return nil, fmt.Errorf("listing resources: %w", err)
	}

	for _, entry := range raw {
		if t, _ := entry["resourceType"].(string); t != resourceType {
			continue
		}
		if n, _ := entry["resourceName"].(string); n != resourceName {
			continue
		}
		contentStr, ok := entry["content"].(string)
		if !ok || contentStr == "" {
			return nil, fmt.Errorf("resource has no content")
		}
		dec := json.NewDecoder(strings.NewReader(contentStr))
		dec.UseNumber()
		var resource map[string]any
		if err := dec.Decode(&resource); err != nil {
			return nil, fmt.Errorf("parsing resource content: %w", err)
		}
		return resource, nil
	}
	return nil, fmt.Errorf("not found in project %q", project)
}

// writeResourceContent writes an updated resource document back via the
// same designer/v2 batch endpoint `raptor apply` itself uses
// (buildUpdateResourcePayload/batchApplyResources in apply.go) — a single
// resource in the "resources" array, with the project's own branch (raptor
// defaults to "main" when the project has none set; see
// CPClient.projectBranch) and a commit message naming the Application that
// triggered this write. NOTE: apply.go issues this as an HTTP POST
// (apiClient.Post), not a PUT — verified against the actual client call,
// not assumed from the "update" framing.
func writeResourceContent(client *CPClient, project, resourceType, resourceName string, resource map[string]any, appName string) error {
	branch, err := client.projectBranch(project)
	if err != nil {
		return fmt.Errorf("resolving project branch: %w", err)
	}

	body := map[string]any{
		"branch": branch,
		"resources": []map[string]any{{
			"resourceName": resourceName,
			"resourceType": resourceType,
			"content":      resource,
		}},
		"commitMessage": fmt.Sprintf("facets-argo-shim: facets_references for %s", appName),
	}

	path := fmt.Sprintf("/cc-ui/v1/designer/v2/%s/resources", url.PathEscape(project))
	return client.post(path, body)
}
