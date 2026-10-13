package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Gitea reads a single file out of a Gitea or Forgejo repository through the
// REST API. It never shells out to git and never clones anything, which is what
// keeps the whole service cheap enough for a small VPS.
//
// The name is the API dialect, not the product: Forgejo is a Gitea fork and
// serves the same /api/v1/repos/... shape, so one client covers both. Kind only
// affects how the source describes itself, because an operator looking at
// /healthz should see the product they actually run.
//
// The token stays on the server: the browser only ever talks to ghome.
type Gitea struct {
	kind   string
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
	// public ones. Sent as "Authorization: Bearer", which both Gitea and
	// Forgejo accept and which survives proxies that only forward the standard
	// RFC 6750 scheme.
	Token string
	// Kind names the product for Describe(): "gitea" or "forgejo". Defaults to
	// "gitea".
	Kind string
	// Timeout bounds a single API call. Defaults to 15s.
	Timeout time.Duration
}

// NewGitea validates the options and returns a ready source.
func NewGitea(o GiteaOptions) (*Gitea, error) {
	base := strings.TrimRight(strings.TrimSpace(o.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("forge url is required (--forge-url)")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("forge url %q: %w", base, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("forge url %q: scheme must be http or https", base)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("forge url %q: missing host", base)
	}
	if strings.TrimSpace(o.Owner) == "" {
		return nil, fmt.Errorf("forge owner is required (--forge-owner)")
	}
	if strings.TrimSpace(o.Repo) == "" {
		return nil, fmt.Errorf("forge repo is required (--forge-repo)")
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
		return nil, fmt.Errorf("forge path %q must be a relative path inside the repository", o.Path)
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	kind := strings.ToLower(strings.TrimSpace(o.Kind))
	if kind == "" {
		kind = "gitea"
	}

	return &Gitea{
		kind:   kind,
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
	return fmt.Sprintf("%s:%s/%s/%s@%s", g.kind, g.owner, g.repo, g.path, g.ref)
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
		return Snapshot{}, fmt.Errorf("%s: %q is not a path inside the repository", g.kind, path)
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
		req.Header.Set("Authorization", "Bearer "+g.token)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%s request failed: %w", g.kind, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := g.checkStatus(resp, path, endpoint); err != nil {
		return Snapshot{}, err
	}

	var cr contentsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&cr); err != nil {
		return Snapshot{}, fmt.Errorf("%s returned an unexpected body: %w", g.kind, err)
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
		// Hash the content rather than falling back to its length. Two different
		// documents of the same size would otherwise share a revision, so the
		// watcher's fingerprint would not move and a real edit would never be
		// published. The file source already revisions this way.
		sum := sha256.Sum256(data)
		rev = "sha256:" + hex.EncodeToString(sum[:])
	}
	return Snapshot{Data: data, Rev: rev}, nil
}

// checkStatus turns a non-200 into something an operator can act on. The path
// being read is passed in rather than taken from g.path: a sibling file such as
// the custom stylesheet is fetched through the same helper, and blaming nav.yaml
// for a missing stylesheet sends the reader to the wrong place.
func (g *Gitea) checkStatus(resp *http.Response, path, endpoint string) error {
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%s 404: %s not found in %s/%s at ref %q (check the path, the ref, and the token's repository scope)",
			g.kind, path, g.owner, g.repo, g.ref)
	case http.StatusUnauthorized:
		return fmt.Errorf("%s 401: the access token was rejected (is it expired or revoked?)", g.kind)
	case http.StatusForbidden:
		return fmt.Errorf("%s 403: the access token cannot read %s/%s (grant it repo read scope)", g.kind, g.owner, g.repo)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("%s %s: unexpected HTTP %d: %s", g.kind, endpoint, resp.StatusCode, bytes.TrimSpace(body))
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
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s raw request failed: %w", g.kind, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := g.checkStatus(resp, path, endpoint); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("%s raw read failed: %w", g.kind, err)
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
			return nil, fmt.Errorf("the API returned undecodable base64 content: %w", err)
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
