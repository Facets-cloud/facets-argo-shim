// cpclient.go
// Minimal Facets control-plane client for output lookups. Endpoints and
// auth mirror raptor (pkg/client, cmd/resource-outputs.go).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// CPConfig is the Facets control-plane connection: URL + basic-auth
// username/token.
type CPConfig struct {
	URL      string
	Username string
	Token    string
}

// ConfigFromEnv reads FACETS_CP_URL / FACETS_CP_USERNAME / FACETS_CP_TOKEN,
// reporting every missing variable in one error.
func ConfigFromEnv() (CPConfig, error) {
	cfg := CPConfig{
		URL:      os.Getenv("FACETS_CP_URL"),
		Username: os.Getenv("FACETS_CP_USERNAME"),
		Token:    os.Getenv("FACETS_CP_TOKEN"),
	}
	var missing []string
	if cfg.URL == "" {
		missing = append(missing, "FACETS_CP_URL")
	}
	if cfg.Username == "" {
		missing = append(missing, "FACETS_CP_USERNAME")
	}
	if cfg.Token == "" {
		missing = append(missing, "FACETS_CP_TOKEN")
	}
	if len(missing) > 0 {
		return CPConfig{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	warnIfNotHTTPS(cfg.URL)
	return cfg, nil
}

// warnIfNotHTTPS prints a one-line warning to stderr — never an error, this
// scheme is still allowed — when FACETS_CP_URL doesn't use https. An
// in-cluster stub or sidecar control-plane endpoint reached over plain HTTP
// inside a trusted mesh is a legitimate setup, but basic-auth credentials
// (and every resolved output) going out in cleartext to anything else is
// worth flagging loudly rather than staying silent about it.
func warnIfNotHTTPS(rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Scheme == "https" {
		return
	}
	fmt.Fprintf(os.Stderr, "facets-resolver: warning: FACETS_CP_URL %q does not use https — control-plane credentials and responses will be sent in cleartext\n", rawURL)
}

// CPClient is a minimal Facets control-plane client.
type CPClient struct {
	cfg  CPConfig
	http *http.Client
}

// NewCPClient builds a CPClient from cfg.
func NewCPClient(cfg CPConfig) *CPClient {
	return &CPClient{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
}

// cpResponseLimit caps how much of a control-plane response body this client
// will ever read — including a successful response, not just an error body —
// so a misbehaving or compromised control-plane endpoint can't exhaust this
// process's memory via an unbounded response.
const cpResponseLimit = 20 << 20 // 20MB

func (c *CPClient) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(c.cfg.URL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.cfg.Username, c.cfg.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("control plane unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// UseNumber: decode JSON numbers as json.Number (exact decimal text)
	// instead of float64, so a large integer (e.g. an 18-digit account or
	// resource ID) isn't silently corrupted by float64's ~15-17 significant
	// digit precision on its way through this client. resolver.go threads
	// json.Number through whole-ref injection and embedded stringification
	// so it round-trips into the rendered manifest exactly.
	dec := json.NewDecoder(io.LimitReader(resp.Body, cpResponseLimit))
	dec.UseNumber()
	return dec.Decode(out)
}

// put issues an authenticated PUT with a JSON body, discarding any
// response body beyond checking the status code. Used only by the v0.13
// consumed-references callback (callback.go) — every write this client ever
// makes goes through here. PUT, not POST: the designer/v2 resources
// endpoint treats POST as create-only (a live POST against an existing
// resource returns 400 "File already exists at <path>"); raptor's own
// apply.go issues updates with apiClient.Put and creates with Post. A
// non-2xx status (including 401/403 from a read-only CP token, which has no
// write access to the blueprint) surfaces as an error for the caller to
// treat as best-effort: see callback.go and main.go's run for why a failure
// here never fails the render itself.
func (c *CPClient) put(path string, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding request body: %w", err)
	}
	req, err := http.NewRequest(http.MethodPut, strings.TrimRight(c.cfg.URL, "/")+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.cfg.Username, c.cfg.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("control plane unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PUT %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// projectBranch fetches the project's blueprint branch (used on every
// designer/v2 resource write, mirroring raptor's own
// getProjectBranch/apply.go behavior), defaulting to "main" when the
// project has none set.
func (c *CPClient) projectBranch(project string) (string, error) {
	path := fmt.Sprintf("/cc-ui/v1/stacks/%s", url.PathEscape(project))
	var stack map[string]any
	if err := c.get(path, &stack); err != nil {
		return "", fmt.Errorf("getting project info: %w", err)
	}
	branch, _ := stack["branch"].(string)
	if branch == "" {
		branch = "main"
	}
	return branch, nil
}

func (c *CPClient) environmentID(project, environment string) (string, error) {
	var clusters []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := c.get(fmt.Sprintf("/cc-ui/v1/stacks/%s/clusters", url.PathEscape(project)), &clusters); err != nil {
		return "", fmt.Errorf("listing environments of project %q: %w", project, err)
	}
	var names []string
	for _, cl := range clusters {
		if cl.Name == environment {
			return cl.ID, nil
		}
		names = append(names, cl.Name)
	}
	return "", fmt.Errorf("environment %q not found in project %q (have: %s)", environment, project, strings.Join(names, ", "))
}

// Lookup returns a LookupFunc bound to one (project, environment) — "self"
// in every blueprint-scoped ref. Every value this closure needs from the
// control plane (the environment ID, each resource's outputs, the whole
// variables/secrets status map, individual secret values, the artifact CI
// list, the environment's release stream, and each artifact's resolved URI)
// is fetched at most once and memoized for the closure's lifetime, so a
// single render resolves from one consistent snapshot no matter how many
// refs of each kind it contains.
func (c *CPClient) Lookup(project, environment string) LookupFunc {
	st := &lookupState{
		client:       c,
		project:      project,
		environment:  environment,
		outputs:      map[string]map[string]any{},
		outputErrs:   map[string]error{},
		secretVals:   map[string]any{},
		secretErrs:   map[string]error{},
		artifactURIs: map[string]any{},
		artifactErrs: map[string]error{},
	}
	return st.lookup
}

// lookupState holds every piece of per-render memoized state a Lookup
// closure needs, across both ref grammars (resource output and
// blueprint-scoped). See Lookup's doc comment for what's memoized and why.
type lookupState struct {
	client      *CPClient
	project     string
	environment string

	envID       string
	envErr      error
	envResolved bool

	outputs    map[string]map[string]any // "type/name" -> body
	outputErrs map[string]error

	// vars is the whole varsWithStatus map for the environment — name ->
	// {"value":..., "secret":bool, "status":...} — shared by both the
	// variables and secrets ref classes, since a class-mismatch check (is
	// this actually a secret?) needs the same data either way.
	vars         map[string]any
	varsErr      error
	varsResolved bool

	secretVals map[string]any // name -> resolved value
	secretErrs map[string]error

	// ciNames is the set of artifact CI integration names registered for
	// the project — a blueprint.self.artifacts.<name> ref's <name> must be
	// one of these before its registrations are even fetched.
	ciNames    map[string]bool
	ciErr      error
	ciResolved bool

	// releaseStream is the environment's own release stream name, used to
	// match RELEASE_STREAM-registered artifacts. Resolved lazily (only once
	// an artifact ref is actually looked up), not alongside envID.
	releaseStream         string
	releaseStreamErr      error
	releaseStreamResolved bool

	artifactURIs map[string]any // name -> resolved image URI
	artifactErrs map[string]error
}

func (st *lookupState) resolveEnvID() error {
	if !st.envResolved {
		st.envID, st.envErr = st.client.environmentID(st.project, st.environment)
		st.envResolved = true
	}
	return st.envErr
}

func (st *lookupState) lookup(r Ref) (any, error) {
	if err := st.resolveEnvID(); err != nil {
		return nil, err
	}
	switch r.Kind {
	case RefKindBlueprintVariable:
		return st.lookupVariable(r.Name)
	case RefKindBlueprintSecret:
		return st.lookupSecret(r.Name)
	case RefKindBlueprintArtifact:
		return st.lookupArtifact(r.Name)
	default:
		return st.lookupOutput(r)
	}
}

func (st *lookupState) lookupOutput(r Ref) (any, error) {
	key := r.ResourceType + "/" + r.ResourceName
	body, ok := st.outputs[key]
	if !ok {
		if err, failed := st.outputErrs[key]; failed {
			return nil, err
		}
		path := fmt.Sprintf("/cc-ui/v1/clusters/%s/resourceType/%s/resourceName/%s/resource-out-properties",
			url.PathEscape(st.envID), url.PathEscape(r.ResourceType), url.PathEscape(r.ResourceName))
		body = map[string]any{}
		if err := st.client.get(path, &body); err != nil {
			err = fmt.Errorf("outputs of %s in %s/%s: %w", key, st.project, st.environment, err)
			st.outputErrs[key] = err
			return nil, err
		}
		st.outputs[key] = body
	}
	return walkPath(body, r.Path, key)
}

func walkPath(v any, path []string, key string) (any, error) {
	cur := v
	for i, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("output path %s of %s: %q is not an object", strings.Join(path[:i], "."), key, strings.Join(path[:i], "."))
		}
		cur, ok = m[seg]
		if !ok {
			return nil, fmt.Errorf("output %s has no %q under %q", key, seg, strings.Join(path[:i], "."))
		}
	}
	return cur, nil
}

// resolveVars fetches the environment's whole varsWithStatus map once —
// every variables/secrets ref in the render, however many, shares this one
// call.
func (st *lookupState) resolveVars() error {
	if st.varsResolved {
		return st.varsErr
	}
	st.varsResolved = true
	path := fmt.Sprintf("/cc-ui/v1/clusters/%s/varsWithStatus", url.PathEscape(st.envID))
	m := map[string]any{}
	if err := st.client.get(path, &m); err != nil {
		st.varsErr = fmt.Errorf("listing variables in %s/%s: %w", st.project, st.environment, err)
		return st.varsErr
	}
	st.vars = m
	return nil
}

// lookupVariable resolves blueprint.self.variables.<name>: the entry must
// exist and must NOT be a secret (a secret referenced via .variables. is a
// class-mismatch error, not a value leak). The value is passed through
// as-is — string, bool, or json.Number (see cpclient.go's UseNumber doc) —
// for resolver.go's typed whole-ref/embedded handling.
func (st *lookupState) lookupVariable(name string) (any, error) {
	if err := st.resolveVars(); err != nil {
		return nil, err
	}
	entry, ok := st.vars[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("blueprint variable %q not found in %s/%s", name, st.project, st.environment)
	}
	if secret, _ := entry["secret"].(bool); secret {
		return nil, fmt.Errorf("blueprint variable %q is a secret — use ${facets:blueprint.self.secrets.%s} instead of .variables.", name, name)
	}
	return entry["value"], nil
}

// lookupSecret resolves blueprint.self.secrets.<name>: the entry must exist
// in varsWithStatus and must BE a secret (the flip side of lookupVariable's
// check). Unlike variables, varsWithStatus itself never carries the actual
// secret value (it only proves the secret exists and what environment it's
// scoped to) — the real value is a separate, per-secret call, memoized by
// name so multiple refs to the same secret in one render cost one request.
func (st *lookupState) lookupSecret(name string) (any, error) {
	if err := st.resolveVars(); err != nil {
		return nil, err
	}
	entry, ok := st.vars[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("blueprint secret %q not found in %s/%s", name, st.project, st.environment)
	}
	if secret, _ := entry["secret"].(bool); !secret {
		return nil, fmt.Errorf("blueprint variable %q is not a secret — use ${facets:blueprint.self.variables.%s} instead of .secrets.", name, name)
	}

	if v, ok := st.secretVals[name]; ok {
		return v, nil
	}
	if err, failed := st.secretErrs[name]; failed {
		return nil, err
	}

	notSet := func() error {
		err := fmt.Errorf("secret %q has no value set for environment %q", name, st.environment)
		st.secretErrs[name] = err
		return err
	}

	path := fmt.Sprintf("/cc-ui/v1/stacks/%s/variables/%s/environments", url.PathEscape(st.project), url.PathEscape(name))
	var resp struct {
		EnvironmentValues []struct {
			EnvironmentName string `json:"environmentName"`
			Value           any    `json:"value"`
			Status          string `json:"status"`
		} `json:"environmentValues"`
	}
	if err := st.client.get(path, &resp); err != nil {
		err = fmt.Errorf("fetching secret %q values: %w", name, err)
		st.secretErrs[name] = err
		return nil, err
	}
	for _, ev := range resp.EnvironmentValues {
		if ev.EnvironmentName != st.environment {
			continue
		}
		if ev.Status != "OVERRIDDEN" {
			return nil, notSet()
		}
		st.secretVals[name] = ev.Value
		return ev.Value, nil
	}
	return nil, notSet()
}

// resolveArtifactCIs fetches the project's artifact CI integration list
// once — an artifact ref's name must be a registered ciName before its
// registrations are fetched at all.
func (st *lookupState) resolveArtifactCIs() error {
	if st.ciResolved {
		return st.ciErr
	}
	st.ciResolved = true
	path := fmt.Sprintf("/cc-ui/v1/artifacts-ci/blueprint/%s", url.PathEscape(st.project))
	var raw []map[string]any
	if err := st.client.get(path, &raw); err != nil {
		st.ciErr = fmt.Errorf("listing artifact CI integrations for %s: %w", st.project, err)
		return st.ciErr
	}
	names := map[string]bool{}
	for _, ci := range raw {
		if name, ok := ci["ciName"].(string); ok && name != "" {
			names[name] = true
		}
	}
	st.ciNames = names
	return nil
}

// resolveReleaseStream fetches the project's clusters-overview once, to
// find this environment's own release stream name (needed to match
// RELEASE_STREAM-registered artifacts). Not finding our own cluster in its
// own project's overview isn't itself fatal — releaseStream just stays
// empty, and RELEASE_STREAM registrations simply won't match (ENVIRONMENT
// registrations are unaffected).
func (st *lookupState) resolveReleaseStream() error {
	if st.releaseStreamResolved {
		return st.releaseStreamErr
	}
	st.releaseStreamResolved = true
	path := fmt.Sprintf("/cc-ui/v1/stacks/%s/clusters-overview", url.PathEscape(st.project))
	var overviews []map[string]any
	if err := st.client.get(path, &overviews); err != nil {
		st.releaseStreamErr = fmt.Errorf("listing environments of project %q: %w", st.project, err)
		return st.releaseStreamErr
	}
	for _, ov := range overviews {
		cluster, ok := ov["cluster"].(map[string]any)
		if !ok {
			continue
		}
		if id, _ := cluster["id"].(string); id == st.envID {
			st.releaseStream, _ = cluster["releaseStream"].(string)
			return nil
		}
	}
	return nil
}

// lookupArtifact resolves blueprint.self.artifacts.<name> to the artifact's
// effective image URI for this environment. Precedence: an ENVIRONMENT
// registration whose registrationValue is this environment's cluster ID,
// else a RELEASE_STREAM registration whose registrationValue is this
// environment's release stream name, else a single unscoped default
// registration (empty registrationType/registrationValue) if the API
// exposes one. No match is a hard error listing every registration this
// artifact actually has, so the fix is obvious from the error alone.
func (st *lookupState) lookupArtifact(name string) (any, error) {
	if v, ok := st.artifactURIs[name]; ok {
		return v, nil
	}
	if err, failed := st.artifactErrs[name]; failed {
		return nil, err
	}

	if err := st.resolveArtifactCIs(); err != nil {
		return nil, err
	}
	if !st.ciNames[name] {
		err := fmt.Errorf("blueprint artifact %q not found for project %q", name, st.project)
		st.artifactErrs[name] = err
		return nil, err
	}

	path := fmt.Sprintf("/cc-ui/v1/artifacts-ci/%s/artifacts", url.PathEscape(name))
	var raw []map[string]any
	if err := st.client.get(path, &raw); err != nil {
		err = fmt.Errorf("listing registrations for artifact %q: %w", name, err)
		st.artifactErrs[name] = err
		return nil, err
	}

	if err := st.resolveReleaseStream(); err != nil {
		return nil, err
	}

	var envMatch, streamMatch, defaultMatch string
	var available []string
	for _, reg := range raw {
		regType, _ := reg["registrationType"].(string)
		regValue, _ := reg["registrationValue"].(string)
		uri, _ := reg["artifactUri"].(string)
		available = append(available, fmt.Sprintf("%s=%s", regType, regValue))
		switch {
		case regType == "ENVIRONMENT" && regValue == st.envID:
			envMatch = uri
		case regType == "RELEASE_STREAM" && st.releaseStream != "" && regValue == st.releaseStream:
			streamMatch = uri
		case regType == "" && regValue == "":
			defaultMatch = uri
		}
	}

	var uri string
	switch {
	case envMatch != "":
		uri = envMatch
	case streamMatch != "":
		uri = streamMatch
	case defaultMatch != "":
		uri = defaultMatch
	default:
		err := fmt.Errorf("blueprint artifact %q has no registration for environment %q (available registrations: %s)", name, st.environment, strings.Join(available, ", "))
		st.artifactErrs[name] = err
		return nil, err
	}

	st.artifactURIs[name] = uri
	return uri, nil
}
