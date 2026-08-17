// cpclient.go
// Minimal Facets control-plane client for output lookups. Endpoints and
// auth mirror raptor (pkg/client, cmd/resource-outputs.go).
package main

import (
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
	return cfg, nil
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
	return json.NewDecoder(resp.Body).Decode(out)
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

// Lookup returns a LookupFunc bound to one (project, environment). The env-ID
// and each resource's outputs are fetched once and memoized for the lifetime
// of the func, so a single render resolves from one consistent snapshot.
func (c *CPClient) Lookup(project, environment string) LookupFunc {
	var envID string
	var envErr error
	envResolved := false
	outputs := map[string]map[string]any{} // "type/name" -> body
	outputErrs := map[string]error{}

	return func(r Ref) (any, error) {
		if !envResolved {
			envID, envErr = c.environmentID(project, environment)
			envResolved = true
		}
		if envErr != nil {
			return nil, envErr
		}
		key := r.ResourceType + "/" + r.ResourceName
		body, ok := outputs[key]
		if !ok {
			if err, failed := outputErrs[key]; failed {
				return nil, err
			}
			path := fmt.Sprintf("/cc-ui/v1/clusters/%s/resourceType/%s/resourceName/%s/resource-out-properties",
				url.PathEscape(envID), url.PathEscape(r.ResourceType), url.PathEscape(r.ResourceName))
			body = map[string]any{}
			if err := c.get(path, &body); err != nil {
				err = fmt.Errorf("outputs of %s in %s/%s: %w", key, project, environment, err)
				outputErrs[key] = err
				return nil, err
			}
			outputs[key] = body
		}
		return walkPath(body, r.Path, key)
	}
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
