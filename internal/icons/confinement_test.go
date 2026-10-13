package icons

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The allow list is meant to confine fetching to the hosts named in the config.
// The initial URL is checked, but two other paths were not, so one allowed host
// could hand the fetch to any address at all -- including loopback and the cloud
// metadata endpoint, on a service that is expected to sit inside a LAN.
//
// These tests use "localhost" and "127.0.0.1" as the two names on purpose: they
// reach the same machine but are distinct allow-list entries, because hostOf
// keys on the host name with the port dropped. A single name for both servers
// would hide the bug.
//
// otherName returns the URL of s reached by the name that is not allow-listed.
func otherName(s *site) string {
	return strings.Replace(s.URL, "127.0.0.1", "localhost", 1)
}

func TestIconLinkCannotNameAnUnallowedHost(t *testing.T) {
	// The host the page is on is allowed; this one is not.
	target := newSite(t, map[string]http.HandlerFunc{
		"/icon.png": image(pngBytes, "image/png"),
	})
	declaring := newSite(t, map[string]http.HandlerFunc{
		"/": page(`<link rel="icon" href="` + target.URL + `/icon.png">`),
	})

	r := testResolver(t, Options{})
	r.Allow([]string{otherName(declaring)})

	if _, ok := r.Lookup(context.Background(), otherName(declaring)); ok {
		t.Error("Lookup() = true, want an icon declared on an unallowed host to be refused")
	}
	if got := target.total(); got != 0 {
		t.Errorf("the unallowed host was contacted %d times, want 0: "+
			"the page markup moved the fetch off the allow list", got)
	}
}

func TestRedirectCannotLeaveTheAllowList(t *testing.T) {
	target := newSite(t, map[string]http.HandlerFunc{
		"/icon.png": image(pngBytes, "image/png"),
	})
	redirecting := newSite(t, map[string]http.HandlerFunc{
		"/favicon.ico": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/icon.png", http.StatusFound)
		},
	})

	r := testResolver(t, Options{})
	r.Allow([]string{otherName(redirecting)})

	if _, ok := r.Lookup(context.Background(), otherName(redirecting)); ok {
		t.Error("Lookup() = true, want a redirect to an unallowed host to be refused")
	}
	if got := target.total(); got != 0 {
		t.Errorf("the unallowed host was contacted %d times, want 0: "+
			"a redirect moved the fetch off the allow list", got)
	}
}

// A same-host icon must keep working: that is the common case and the fix must
// not break it.
func TestSameHostIconStillResolves(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/":         page(`<link rel="icon" href="/icon.png">`),
		"/icon.png": image(pngBytes, "image/png"),
	})
	r := testResolver(t, Options{})
	r.Allow([]string{s.URL})

	icon, ok := r.Lookup(context.Background(), s.URL)
	if !ok {
		t.Fatal("Lookup() = false, want the same-host icon to resolve")
	}
	if len(icon.Data) == 0 {
		t.Error("icon.Data is empty")
	}
}
