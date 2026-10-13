package source

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Gitea reads a single file out of a Gitea/Forgejo repository through the REST
// API. It never shells out to git and never clones anything, which is what
// keeps the whole service cheap enough for a small VPS.
//
// The token stays on the server: the browser only ever talks to ghome.
type Gitea struct {
	base   string
	owner  string
	repo   string
	ref    string
	path   string
	token  string
	client *http.Client
}

// GiteaOptions configures a Gitea source.
type GiteaOptions struct {
	// BaseURL is the Gitea root, e.g. https://git.example.com. A reverse
	// proxied sub-path such as https://example.com/gitea is supported.
	BaseURL string
	Owner   string
	Repo    string
	// Ref is a branch, tag or commit SHA. Defaults to main.
	Ref string
	// Path is the file inside the repository. Defaults to nav.yaml.
	Path string
	// Token is a read-only access token for private repositories. Empty for
	// public ones.
	Token string
	// Timeout bounds a single API call. Defaults to 15s.
	Timeout time.Duration
}

// NewGitea validates the options and returns a ready source.
func NewGitea(o GiteaOptions) (*Gitea, error) {
	base := strings.TrimRight(strings.TrimSpace(o.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("gitea url is required (--gitea-url)")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("gitea url %q: %w", base, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("gitea url %q: scheme must be http or https", base)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("gitea url %q: missing host", base)
	}
	if strings.TrimSpace(o.Owner) == "" {
		return nil, fmt.Errorf("gitea owner is required (--gitea-owner)")
	}
	if strings.TrimSpace(o.Repo) == "" {
		return nil, fmt.Errorf("gitea repo is required (--gitea-repo)")
	}

	ref := strings.TrimSpace(o.Ref)
	if ref == "" {
		ref = "main"
	}
	path := strings.TrimLeft(strings.TrimSpace(o.Path), "/")
	if path == "" {
		path = "nav.yaml"
	}
	if !SafeRelativePath(path) {
		return nil, fmt.Errorf("gitea path %q must be a relative path inside the repository", o.Path)
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	return &Gitea{
		base:   base,
		owner:  strings.TrimSpace(o.Owner),
		repo:   strings.TrimSpace(o.Repo),
		ref:    ref,
		path:   path,
		token:  strings.TrimSpace(o.Token),
		client: &http.Client{Timeout: timeout},
	}, nil
}

// Ref is the configured branch/tag/SHA. The webhook handler uses it to ignore
// pushes to unrelated branches.
func (g *Gitea) Ref() string { return g.ref }

func (g *Gitea) Describe() string {
	return fmt.Sprintf("gitea:%s/%s/%s@%s", g.owner, g.repo, g.path, g.ref)
}

// contentsResponse is the subset of GET /contents/{path} we care about.
type contentsResponse struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Size     int64  `json:"size"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func (g *Gitea) Fetch(ctx context.Context) (Snapshot, error) {
	return g.fetchPath(ctx, g.path)
}

// FetchPath reads a sibling file, such as the custom stylesheet referenced by
// style.customCss.
func (g *Gitea) FetchPath(ctx context.Context, path string) (Snapshot, error) {
	if !SafeRelativePath(path) {
		return Snapshot{}, fmt.Errorf("gitea: %q is not a path inside the repository", path)
	}
	return g.fetchPath(ctx, path)
}

func (g *Gitea) fetchPath(ctx context.Context, path string) (Snapshot, error) {
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/contents/%s?ref=%s",
		g.base, escapeSegment(g.owner), escapeSegment(g.repo), escapePath(path), url.QueryEscape(g.ref))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Snapshot{}, err
	}
	req.Header.Set("Accept", "application/json")
	if g.token != "" {
		req.Header.Set("Authorization", "token "+g.token)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("gitea request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := g.checkStatus(resp, endpoint); err != nil {
		return Snapshot{}, err
	}

	var cr contentsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&cr); err != nil {
		return Snapshot{}, fmt.Errorf("gitea returned an unexpected body: %w", err)
	}

	data, err := decodeContent(&cr)
	if err != nil {
		return Snapshot{}, err
	}
	if data == nil {
		// The API omits content for files above its inline limit; fall back to
		// the raw endpoint. nav.yaml is tiny, so this is just belt and braces.
		data, err = g.fetchRaw(ctx, path)
		if err != nil {
			return Snapshot{}, err
		}
	}

	rev := cr.SHA
	if rev == "" {
		rev = fmt.Sprintf("size:%d", len(data))
	}
	return Snapshot{Data: data, Rev: rev}, nil
}

func (g *Gitea) checkStatus(resp *http.Response, endpoint string) error {
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("gitea 404: %s not found in %s/%s at ref %q (check the path, the ref, and the token's repository scope)",
			g.path, g.owner, g.repo, g.ref)
	case http.StatusUnauthorized:
		return fmt.Errorf("gitea 401: the access token was rejected (is it expired or revoked?)")
	case http.StatusForbidden:
		return fmt.Errorf("gitea 403: the access token cannot read %s/%s (grant it repo read scope)", g.owner, g.repo)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("gitea %s: unexpected HTTP %d: %s", endpoint, resp.StatusCode, bytes.TrimSpace(body))
	}
}

func (g *Gitea) fetchRaw(ctx context.Context, path string) ([]byte, error) {
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/raw/%s?ref=%s",
		g.base, escapeSegment(g.owner), escapeSegment(g.repo), escapePath(path), url.QueryEscape(g.ref))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if g.token != "" {
		req.Header.Set("Authorization", "token "+g.token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitea raw request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := g.checkStatus(resp, endpoint); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("gitea raw read failed: %w", err)
	}
	return data, nil
}

// decodeContent returns nil (and no error) when the API did not inline the
// file, so the caller can fall back to the raw endpoint.
func decodeContent(cr *contentsResponse) ([]byte, error) {
	if strings.EqualFold(cr.Encoding, "base64") && cr.Content != "" {
		// Gitea wraps base64 at 60 columns.
		cleaned := strings.Map(func(r rune) rune {
			switch r {
			case '\n', '\r', ' ', '\t':
				return -1
			}
			return r
		}, cr.Content)
		data, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return nil, fmt.Errorf("gitea returned undecodable base64 content: %w", err)
		}
		return data, nil
	}
	return nil, nil
}

// escapeSegment escapes one path element for use in a URL path.
func escapeSegment(s string) string { return url.PathEscape(s) }

// escapePath escapes a slash separated path while keeping the separators.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
