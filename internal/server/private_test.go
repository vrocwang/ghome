package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vrocwang/ghome/internal/status"
	"github.com/vrocwang/ghome/internal/store"
)

const privateDoc = `
groups:
  - name: public
    links:
      - {name: a, url: "https://a.example.com", pinned: true}
  - name: secret
    private: true
    links:
      - {name: b, url: "https://b.example.com", pinned: true}
      - {name: e, url: "https://e.example.com"}
  - name: mixed
    links:
      - {name: c, url: "https://c.example.com"}
      - {name: d, url: "https://d.example.com", private: true}
`

const goodToken = "let-me-in"

func privateServer(t *testing.T) *Server {
	t.Helper()
	st := store.New()
	if err := st.Load([]byte(privateDoc), "rev-1", "test"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	srv, _ := newTestServer(t, Options{
		Store: st,
		Verify: func(_ context.Context, token string) bool {
			return token == goodToken
		},
	})
	return srv
}

// shape is the little of the payload these tests care about.
type shape struct {
	Groups []struct {
		Name  string `json:"name"`
		Links []struct {
			Name string `json:"name"`
		} `json:"links"`
	} `json:"groups"`
	Meta struct {
		GroupCount  int `json:"groupCount"`
		LinkCount   int `json:"linkCount"`
		PinnedCount int `json:"pinnedCount"`
	} `json:"meta"`
	Private *struct {
		Total  int  `json:"total"`
		Locked bool `json:"locked"`
	} `json:"private"`
}

func getNav(t *testing.T, srv *Server, token string) shape {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/nav", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got shape
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func groupNames(s shape) []string {
	out := make([]string, 0, len(s.Groups))
	for _, g := range s.Groups {
		out = append(out, g.Name)
	}
	return out
}

func linkNames(s shape) []string {
	var out []string
	for _, g := range s.Groups {
		for _, l := range g.Links {
			out = append(out, l.Name)
		}
	}
	return out
}

// A link that reaches the browser is not private any more: hiding it in the
// page would still hand it to anyone with curl.
func TestNavWithholdsPrivateEntries(t *testing.T) {
	srv := privateServer(t)

	t.Run("no token", func(t *testing.T) {
		got := getNav(t, srv, "")
		if got.Private == nil || !got.Private.Locked {
			t.Fatalf("private = %+v, want locked", got.Private)
		}
		if got.Private.Total != 3 {
			t.Errorf("total = %d, want 3", got.Private.Total)
		}
		if names := groupNames(got); len(names) != 2 || names[0] != "public" || names[1] != "mixed" {
			t.Errorf("groups = %v, want [public mixed]", names)
		}
		if names := linkNames(got); len(names) != 2 || names[0] != "a" || names[1] != "c" {
			t.Errorf("links = %v, want [a c]", names)
		}
		// The counts describe what is on the page. Leaving them whole-config
		// made the footer claim links the reader cannot see, and told an
		// anonymous caller how many private groups exist.
		if got.Meta.GroupCount != 2 || got.Meta.LinkCount != 2 || got.Meta.PinnedCount != 1 {
			t.Errorf("meta counts = %d groups / %d links / %d pinned, want the visible 2/2/1",
				got.Meta.GroupCount, got.Meta.LinkCount, got.Meta.PinnedCount)
		}
	})

	t.Run("token that cannot read the repository", func(t *testing.T) {
		got := getNav(t, srv, "wrong")
		if got.Private == nil || !got.Private.Locked {
			t.Fatalf("private = %+v, want locked", got.Private)
		}
		if names := linkNames(got); len(names) != 2 {
			t.Errorf("links = %v, want only the public pair", names)
		}
	})

	t.Run("token that can", func(t *testing.T) {
		got := getNav(t, srv, goodToken)
		if got.Private == nil || got.Private.Locked {
			t.Fatalf("private = %+v, want unlocked", got.Private)
		}
		if got.Private.Total != 3 {
			t.Errorf("total = %d, want 3", got.Private.Total)
		}
		if names := groupNames(got); len(names) != 3 {
			t.Errorf("groups = %v, want all three", names)
		}
		if names := linkNames(got); len(names) != 5 {
			t.Errorf("links = %v, want all five", names)
		}
	})

	// A config with nothing private must not grow a lock the frontend would
	// render as a dead button.
	t.Run("nothing private", func(t *testing.T) {
		srv, _ := newTestServer(t, Options{})
		if got := getNav(t, srv, ""); got.Private != nil {
			t.Errorf("private = %+v, want absent", got.Private)
		}
	})
}

// Without a verifier the private entries stay hidden for everyone, which is the
// safe direction for a source that cannot check a token (file mode).
func TestNavKeepsPrivateHiddenWithoutAVerifier(t *testing.T) {
	st := store.New()
	if err := st.Load([]byte(privateDoc), "rev-1", "test"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	srv, _ := newTestServer(t, Options{Store: st})

	got := getNav(t, srv, goodToken)
	if got.Private == nil || !got.Private.Locked {
		t.Fatalf("private = %+v, want locked even with a token", got.Private)
	}
	if names := linkNames(got); len(names) != 2 {
		t.Errorf("links = %v, want only the public pair", names)
	}
}

// A probe result names the service that was probed, so a withheld link must not
// have one.
func TestStatusDoesNotLeakWithheldLinks(t *testing.T) {
	st := store.New()
	if err := st.Load([]byte(`
status:
  enabled: true
groups:
  - name: g
    links:
      - {name: a, url: "https://a.example.com"}
      - {name: b, url: "https://b.example.com", private: true}
`), "rev-1", "test"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	results := map[string]status.Result{
		"https://a.example.com": {State: status.StateUp},
		"https://b.example.com": {State: status.StateUp},
	}
	srv, _ := newTestServer(t, Options{
		Store:  st,
		Status: fakeStatus{results: results},
		Verify: func(_ context.Context, token string) bool { return token == goodToken },
	})

	fetch := func(token string) []string {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		var body struct {
			Results map[string]json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := make([]string, 0, len(body.Results))
		for url := range body.Results {
			out = append(out, url)
		}
		return out
	}

	locked := fetch("")
	if len(locked) != 1 || !strings.Contains(locked[0], "a.example.com") {
		t.Errorf("locked results = %v, want only the public link", locked)
	}
	unlocked := fetch(goodToken)
	if len(unlocked) != 2 {
		t.Errorf("unlocked results = %v, want both", unlocked)
	}
}

// Verification is cached so a page load does not reach the forge, and so a wrong
// token cannot be used to make ghome hammer it.
func TestVerifierCachesVerdicts(t *testing.T) {
	var calls atomic.Int64
	now := time.Now()
	v := newVerifier(func(_ context.Context, token string) bool {
		calls.Add(1)
		return token == goodToken
	})
	v.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		if !v.allowed(context.Background(), goodToken) {
			t.Fatal("a good token was refused")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("good token caused %d checks, want 1", got)
	}

	for i := 0; i < 5; i++ {
		if v.allowed(context.Background(), "wrong") {
			t.Fatal("a wrong token was accepted")
		}
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("wrong token caused %d checks total, want 2", got)
	}

	// Successes outlive failures, so the good token survives a gap that has
	// already expired the wrong one.
	now = now.Add(verifyFailureTTL + time.Second)
	v.allowed(context.Background(), goodToken)
	if got := calls.Load(); got != 2 {
		t.Errorf("good verdict expired early: %d checks", got)
	}
	v.allowed(context.Background(), "wrong")
	if got := calls.Load(); got != 3 {
		t.Errorf("failure verdict outlived its TTL: %d checks", got)
	}

	now = now.Add(verifySuccessTTL + time.Second)
	v.allowed(context.Background(), goodToken)
	if got := calls.Load(); got != 4 {
		t.Errorf("success verdict never expires: %d checks", got)
	}
}

func TestVerifierStaysBounded(t *testing.T) {
	v := newVerifier(func(_ context.Context, _ string) bool { return false })
	// Every token fails, so nothing but the bound keeps the map in check.
	for i := 0; i < verifyMaxEntries*3; i++ {
		v.allowed(context.Background(), "token-"+string(rune('a'+i%26))+string(rune('a'+i/26%26)))
	}
	v.mu.Lock()
	size := len(v.seen)
	v.mu.Unlock()
	if size > verifyMaxEntries {
		t.Errorf("cache grew to %d entries, want at most %d", size, verifyMaxEntries)
	}
}

func TestNilVerifierRefusesEverything(t *testing.T) {
	var v *verifier
	if v.allowed(context.Background(), goodToken) {
		t.Error("a nil verifier accepted a token")
	}
	if newVerifier(nil).allowed(context.Background(), goodToken) {
		t.Error("a verifier with no check function accepted a token")
	}
	if newVerifier(func(context.Context, string) bool { return true }).allowed(context.Background(), "") {
		t.Error("an empty token was accepted")
	}
}

// A host only a private entry uses must answer exactly like a host that is not
// in the config at all -- same status, and now the same cost. Consulting the
// verifier here instead made the 404 slow for a hidden host and quick for an
// unknown one, which confirmed hidden host names to anyone who timed it, and it
// bought nothing: an <img> cannot carry the viewer token, so a browser could
// never use an unlocked answer. A host a public entry also uses is fine to
// serve.
func TestIconEndpointWithholdsPrivateOnlyHosts(t *testing.T) {
	st := store.New()
	if err := st.Load([]byte(`
groups:
  - name: public
    links:
      - {name: shared, url: "https://shared.example.com/"}
      - {name: local, url: "https://only-private.example.com/", private: true}
  - name: secret
    private: true
    links:
      - {name: hidden, url: "https://shared.example.com/other"}
`), "rev-1", "test"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	var checks atomic.Int64
	srv, _ := newTestServer(t, Options{
		Store: st,
		Icons: &fakeIcons{data: []byte("GIF89a"), ct: "image/gif", ok: true},
		Verify: func(_ context.Context, token string) bool {
			checks.Add(1)
			return token == goodToken
		},
	})

	icon := func(host, token string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/icon?h="+host, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}

	// Every caller gets the same answer, including a caller holding a token
	// that would unlock the link in the page.
	for _, token := range []string{"", "wrong", goodToken} {
		if got := icon("only-private.example.com", token); got != http.StatusNotFound {
			t.Errorf("a private-only host answered %d (token %q), want 404", got, token)
		}
	}
	// Not asking the forge is what makes the two 404s indistinguishable.
	if got := checks.Load(); got != 0 {
		t.Errorf("the verifier was consulted %d times for a private-only host, want 0", got)
	}
	// A public entry uses this host, so hiding it would break the public page.
	if got := icon("shared.example.com", ""); got != http.StatusOK {
		t.Errorf("a host a public link also uses answered %d, want 200", got)
	}
}

// /healthz is unauthenticated. It served the raw warning list -- whose text
// names the entry it came from -- and probe totals that counted every target,
// private ones included. Both have to describe the public view only.
func TestHealthzDoesNotLeakPrivateEntries(t *testing.T) {
	st := store.New()
	if err := st.Load([]byte(`
status:
  enabled: true
groups:
  - name: 公开
    links:
      - {name: 百度, url: "https://public.example.com"}
  - name: 私密
    private: true
    links:
      - {name: 财务系统, url: "https://finance.internal", hotkey: "xy"}
`), "rev-1", "test"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	results := map[string]status.Result{
		"https://public.example.com": {State: status.StateUp},
		"https://finance.internal":   {State: status.StateDown},
	}
	srv, _ := newTestServer(t, Options{
		Store:  st,
		Status: fakeStatus{results: results},
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{"财务系统", "finance.internal"} {
		if strings.Contains(body, secret) {
			t.Errorf("/healthz leaks %q:\n%s", secret, body)
		}
	}

	var state healthState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	// Only the public probe is counted, so the totals cannot report a withheld
	// service; the private one is down.
	if state.ProbesUp != 1 || state.ProbesDown != 0 {
		t.Errorf("probes up/down = %d/%d, want 1/0", state.ProbesUp, state.ProbesDown)
	}
	if state.Groups != 1 || state.Links != 1 {
		t.Errorf("groups/links = %d/%d, want the public 1/1", state.Groups, state.Links)
	}
	if len(state.Warnings) != 0 {
		t.Errorf("warnings = %v, want none: they name the entry they came from", state.Warnings)
	}
}

// A client that gives up mid-request cancels the request context. If that
// cancelled call decided the verdict, the failure would be cached and lock a
// legitimate viewer out for the whole failure TTL.
func TestVerifierIgnoresACancelledCaller(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	var calls atomic.Int64
	v := newVerifier(func(ctx context.Context, _ string) bool {
		calls.Add(1)
		// Stands in for the forge call: a cancelled context would fail here.
		return ctx.Err() == nil
	})

	if !v.allowed(cancelled, goodToken) {
		t.Fatal("a cancelled caller turned a good token into a refusal")
	}
	// The success has to be cached, otherwise the caller would still be checked
	// again on every request.
	if !v.allowed(context.Background(), goodToken) {
		t.Fatal("the cached success was not reused")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("check ran %d times, want 1 (the verdict must be cached)", got)
	}
}

// A config that fails to parse is reported through the unauthenticated /healthz
// and through the 503 body of /api/nav. Neither may repeat the offending value:
// a private link's url can carry a token, and Go's url.Parse error quotes the
// whole input.
func TestParseErrorsDoNotEchoPrivateURLs(t *testing.T) {
	st := store.New()
	loadErr := st.Load([]byte(`
groups:
  - name: public
    links:
      - {name: a, url: "https://a.example.com"}
  - name: secret
    private: true
    links:
      - {name: 日记, url: "https://diary.internal/%zz?token=SUPERSECRET"}
`), "rev-1", "test")
	if loadErr == nil {
		t.Fatal("the broken url was accepted")
	}

	srv, _ := newTestServer(t, Options{Store: st})
	for _, path := range []string{"/healthz", "/api/nav"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		body := rec.Body.String()
		for _, secret := range []string{"SUPERSECRET", "diary.internal", "%zz"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s (%d) leaks %q:\n%s", path, rec.Code, secret, body)
			}
		}
	}
}

// A structural error is reported through /healthz and the 503 body of /api/nav,
// both without a token. The coordinate identifies the line; the entry's name
// must not appear, because a private entry's name is what the tier withholds.
func TestParseErrorsDoNotEchoTheEntryName(t *testing.T) {
	st := store.New()
	loadErr := st.Load([]byte(`
groups:
  - name: public
    links:
      - {name: a, url: "https://a.example.com"}
  - name: secret
    private: true
    links:
      - {name: 财务系统, url: ""}
`), "rev-1", "test")
	if loadErr == nil {
		t.Fatal("the entry with no url was accepted")
	}
	if !strings.Contains(loadErr.Error(), "groups[1].links[0]") {
		t.Errorf("error = %v, want it to carry the coordinate", loadErr)
	}

	srv, _ := newTestServer(t, Options{Store: st})
	for _, path := range []string{"/healthz", "/api/nav"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if body := rec.Body.String(); strings.Contains(body, "财务系统") {
			t.Errorf("%s (%d) leaks the entry name:\n%s", path, rec.Code, body)
		}
	}
}
