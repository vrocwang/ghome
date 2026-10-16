package source

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// wrap60 reproduces how Gitea wraps base64 payloads in its JSON responses.
func wrap60(s string) string {
	var b strings.Builder
	for len(s) > 60 {
		b.WriteString(s[:60])
		b.WriteByte('\n')
		s = s[60:]
	}
	b.WriteString(s)
	return b.String()
}

func TestGiteaFetchDecodesWrappedBase64(t *testing.T) {
	content := "site:\n  title: 我的导航\n"
	payload := wrap60(base64.StdEncoding.EncodeToString([]byte(content)))

	var gotAuth, gotPath, gotRef, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotPath = r.URL.Path
		gotRef = r.URL.Query().Get("ref")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"name":"nav.yaml","path":"nav.yaml","sha":"abc123","size":%d,"encoding":"base64","content":%q}`,
			len(content), payload)
	}))
	defer srv.Close()

	src, err := NewGitea(GiteaOptions{
		BaseURL: srv.URL, Owner: "acme", Repo: "nav",
		Ref: "main", Path: "nav.yaml", Token: "tok",
	})
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}

	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if string(snap.Data) != content {
		t.Errorf("Data = %q, want %q", snap.Data, content)
	}
	if snap.Rev != "abc123" {
		t.Errorf("Rev = %q, want abc123", snap.Rev)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok")
	}
	if gotPath != "/api/v1/repos/acme/nav/contents/nav.yaml" {
		t.Errorf("path = %q", gotPath)
	}
	if gotRef != "main" {
		t.Errorf("ref = %q, want main", gotRef)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if got := src.Describe(); got != "gitea:acme/nav/nav.yaml@main" {
		t.Errorf("Describe() = %q", got)
	}
}

func TestGiteaFetchSupportsASubPathBaseURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"sha":"s1","encoding":"base64","content":"Z3JvdXBzOiBbXQ=="}`))
	}))
	defer srv.Close()

	src, err := NewGitea(GiteaOptions{
		BaseURL: srv.URL + "/gitea/", Owner: "acme", Repo: "nav",
		Ref: "main", Path: "config/nav.yaml",
	})
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}
	if _, err := src.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	want := "/gitea/api/v1/repos/acme/nav/contents/config/nav.yaml"
	if gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestGiteaFetchFallsBackToTheRawEndpoint(t *testing.T) {
	const content = "groups: []\n"
	var rawHits int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/contents/"):
			// Above its inline limit Gitea omits the content entirely.
			_, _ = w.Write([]byte(`{"name":"nav.yaml","sha":"s1","size":12,"encoding":"","content":""}`))
		case strings.Contains(r.URL.Path, "/raw/"):
			rawHits++
			_, _ = w.Write([]byte(content))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	src, err := NewGitea(GiteaOptions{BaseURL: srv.URL, Owner: "o", Repo: "r"})
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}
	snap, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if rawHits != 1 {
		t.Errorf("raw endpoint hit %d times, want 1", rawHits)
	}
	if string(snap.Data) != content {
		t.Errorf("Data = %q, want %q", snap.Data, content)
	}
}

func TestGiteaFetchExplainsAMissingFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src, err := NewGitea(GiteaOptions{BaseURL: srv.URL, Owner: "o", Repo: "r", Path: "nav.yaml"})
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}
	_, err = src.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch() succeeded against a 404")
	}
	if !strings.Contains(err.Error(), "nav.yaml") || !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q should name the file and the status", err)
	}
}

func TestGiteaFetchExplainsARejectedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	src, err := NewGitea(GiteaOptions{BaseURL: srv.URL, Owner: "o", Repo: "r", Token: "stale"})
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}
	_, err = src.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want a 401 explanation", err)
	}
}

func TestNewGiteaValidatesOptions(t *testing.T) {
	cases := map[string]GiteaOptions{
		"no url":   {Owner: "o", Repo: "r"},
		"bad url":  {BaseURL: "git.example.com", Owner: "o", Repo: "r"},
		"no owner": {BaseURL: "https://git.example.com", Repo: "r"},
		"no repo":  {BaseURL: "https://git.example.com", Owner: "o"},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewGitea(opts); err == nil {
				t.Error("NewGitea() accepted invalid options")
			}
		})
	}
}

func TestGiteaDefaultsRefAndPath(t *testing.T) {
	src, err := NewGitea(GiteaOptions{BaseURL: "https://git.example.com", Owner: "o", Repo: "r"})
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}
	if got := src.Describe(); got != "gitea:o/r/nav.yaml@main" {
		t.Errorf("Describe() = %q", got)
	}
}

func TestFileSourceHashesContent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/nav.yaml"
	if err := writeFile(path, "groups: []\n"); err != nil {
		t.Fatalf("writeFile() error = %v", err)
	}

	src, err := NewFile(path)
	if err != nil {
		t.Fatalf("NewFile() error = %v", err)
	}
	first, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !strings.HasPrefix(first.Rev, "sha256:") {
		t.Errorf("Rev = %q, want a sha256 prefix", first.Rev)
	}

	// Same bytes, same revision: the watcher relies on this to skip re-parsing.
	again, _ := src.Fetch(context.Background())
	if again.Rev != first.Rev {
		t.Errorf("Rev changed for identical content: %q then %q", first.Rev, again.Rev)
	}

	if err := writeFile(path, "groups: []\n# changed\n"); err != nil {
		t.Fatalf("writeFile() error = %v", err)
	}
	changed, _ := src.Fetch(context.Background())
	if changed.Rev == first.Rev {
		t.Error("Rev did not change after the file changed")
	}
	if got := src.Describe(); got != "file:"+path {
		t.Errorf("Describe() = %q", got)
	}
}

func TestFileSourceReportsAMissingFile(t *testing.T) {
	src, err := NewFile(t.TempDir() + "/nope.yaml")
	if err != nil {
		t.Fatalf("NewFile() error = %v", err)
	}
	if _, err := src.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch() succeeded for a missing file")
	}
}

func TestFileSourceRejectsAnEmptyPath(t *testing.T) {
	if _, err := NewFile(""); err == nil {
		t.Fatal("NewFile(\"\") succeeded")
	}
}

func TestFileSourceReadsSiblings(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir+"/nav.yaml", "groups: []\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(dir+"/custom.css", ".a { color: red }\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir+"/styles", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(dir+"/styles/deep.css", ".b { color: blue }\n"); err != nil {
		t.Fatal(err)
	}

	src, err := NewFile(dir + "/nav.yaml")
	if err != nil {
		t.Fatalf("NewFile() error = %v", err)
	}

	for _, path := range []string{"custom.css", "styles/deep.css"} {
		snap, err := src.FetchPath(context.Background(), path)
		if err != nil {
			t.Errorf("FetchPath(%q) error = %v", path, err)
			continue
		}
		if len(snap.Data) == 0 || !strings.HasPrefix(snap.Rev, "sha256:") {
			t.Errorf("FetchPath(%q) returned %d bytes, rev %q", path, len(snap.Data), snap.Rev)
		}
	}
}

// style.customCss comes out of a config file, so it must not be able to read
// anything outside the config directory.
func TestFileSourceRefusesToEscapeItsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir+"/nav.yaml", "groups: []\n"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Dir(dir) + "/secret.css"
	if err := writeFile(outside, "top secret\n"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	src, err := NewFile(dir + "/nav.yaml")
	if err != nil {
		t.Fatalf("NewFile() error = %v", err)
	}

	for _, path := range []string{
		"../secret.css",
		"../../etc/passwd",
		"/etc/passwd",
		"styles/../../secret.css",
		"./../secret.css",
		"",
		"a\\..\\b.css",
		"C:/x.css",
	} {
		if _, err := src.FetchPath(context.Background(), path); err == nil {
			t.Errorf("FetchPath(%q) succeeded, want it refused", path)
		}
	}
}

func TestGiteaFetchPathValidatesThePath(t *testing.T) {
	src, err := NewGitea(GiteaOptions{BaseURL: "https://git.example.com", Owner: "o", Repo: "r"})
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}
	for _, path := range []string{"../x.css", "/etc/passwd", ""} {
		if _, err := src.FetchPath(context.Background(), path); err == nil {
			t.Errorf("FetchPath(%q) succeeded, want it refused", path)
		}
	}
}

func TestSafeRelativePath(t *testing.T) {
	valid := []string{"a.css", "styles/a.css", "a/b/c.css"}
	for _, v := range valid {
		if !SafeRelativePath(v) {
			t.Errorf("SafeRelativePath(%q) = false, want true", v)
		}
	}
	invalid := []string{"", "/a.css", "../a.css", "a/../b", "./a.css", "a//b", "C:/a.css", "a\\b", "http://x/a.css"}
	for _, v := range invalid {
		if SafeRelativePath(v) {
			t.Errorf("SafeRelativePath(%q) = true, want false", v)
		}
	}
}

func TestNewGiteaRejectsAnEscapingPath(t *testing.T) {
	if _, err := NewGitea(GiteaOptions{
		BaseURL: "https://git.example.com", Owner: "o", Repo: "r", Path: "../../etc/passwd",
	}); err == nil {
		t.Fatal("NewGitea() accepted a path that escapes the repository")
	}
}

// An operator reading /healthz should see the product they actually run.
func TestGiteaDescribeUsesTheConfiguredKind(t *testing.T) {
	base := GiteaOptions{BaseURL: "https://git.example.com", Owner: "acme", Repo: "homepage"}

	gitea, err := NewGitea(base)
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}
	if got := gitea.Describe(); got != "gitea:acme/homepage/nav.yaml@main" {
		t.Errorf("Describe() = %q, want the gitea default", got)
	}

	opts := base
	opts.Kind = "forgejo"
	forgejo, err := NewGitea(opts)
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}
	if got := forgejo.Describe(); got != "forgejo:acme/homepage/nav.yaml@main" {
		t.Errorf("Describe() = %q, want forgejo", got)
	}
}

// CheckRead is the whole permission model for the private links: any answer it
// wrongly accepts hands them over, so it is claimed here case by case.
func TestCheckReadIsStrict(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/nav", func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case "good":
			_, _ = io.WriteString(w, `{"full_name":"acme/nav","private":true}`)
		case "case":
			_, _ = io.WriteString(w, `{"full_name":"ACME/NAV"}`)
		case "other-repo":
			// A 200 that is not about this repository at all: a typo'd
			// --forge-url, or a proxy's default page.
			_, _ = io.WriteString(w, `{"full_name":"someone/else"}`)
		case "not-json":
			_, _ = io.WriteString(w, `<html>login</html>`)
		case "empty":
			// A 200 with no body.
		case "redirect":
			// An SSO or proxy front end answering with a redirect to a page
			// that returns 200.
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<html><body>sign in</body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src, err := NewGitea(GiteaOptions{
		BaseURL: srv.URL, Owner: "acme", Repo: "nav", Ref: "main", Path: "nav.yaml",
	})
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}

	allowed := map[string]bool{
		"good":       true,
		"case":       true, // git full names are case insensitive enough for this
		"":           false,
		"bad":        false,
		"other-repo": false,
		"not-json":   false,
		"empty":      false,
		"redirect":   false,
	}
	for token, want := range allowed {
		got := src.CheckRead(context.Background(), token) == AccessOK
		if got != want {
			t.Errorf("CheckRead(%q) == ok is %v, want %v", token, got, want)
		}
	}
}

// CheckRead has to separate "the forge refused this token" from "the forge
// could not be asked". A caller that cannot tell them apart reports a forge
// outage to the viewer as a bad token -- and deletes the token it just took
// from them.
func TestCheckReadSeparatesRefusalFromOutage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/nav", func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case "good":
			_, _ = io.WriteString(w, `{"full_name":"acme/nav"}`)
		case "forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "missing":
			w.WriteHeader(http.StatusNotFound)
		case "broken":
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src, err := NewGitea(GiteaOptions{
		BaseURL: srv.URL, Owner: "acme", Repo: "nav", Ref: "main", Path: "nav.yaml",
	})
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}

	want := map[string]Access{
		"good":      AccessOK,
		"forbidden": AccessRejected, // the forge answered
		"missing":   AccessRejected, // and so did this one
		"wrong":     AccessRejected,
		"":          AccessRejected,
		"broken":    AccessUnavailable, // 5xx: the forge failed, it did not judge
	}
	for token, expected := range want {
		if got := src.CheckRead(context.Background(), token); got != expected {
			t.Errorf("CheckRead(%q) = %q, want %q", token, got, expected)
		}
	}

	// An unreachable forge is the same answer, not a refusal.
	dead, err := NewGitea(GiteaOptions{
		BaseURL: "http://127.0.0.1:1", Owner: "acme", Repo: "nav",
		Timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}
	if got := dead.CheckRead(context.Background(), "good"); got != AccessUnavailable {
		t.Errorf("CheckRead() against a dead forge = %q, want unavailable", got)
	}
}
