package nav

import (
	"strings"
	"testing"
)

// A link url becomes a.href in the browser, so a scheme that executes is stored
// XSS in every visitor's browser. The denylist is the only thing standing in the
// way, and the interesting question is not what it blocks but whether the
// browser would normalise something into a blocked scheme that Go does not --
// browsers strip tab, newline and carriage return anywhere in a url, and trim
// leading C0 controls and spaces, before they parse it.
func TestCheckURLRejectsExecutableSchemes(t *testing.T) {
	reject := []string{
		"javascript:alert(1)",
		"JaVaScRiPt:alert(1)",
		"JAVASCRIPT:alert(1)",
		"java\tscript:alert(1)",   // browser strips the tab
		"java\nscript:alert(1)",   // browser strips the newline
		"java\rscript:alert(1)",   // browser strips the return
		" javascript:alert(1)",    // browser trims the space
		"\tjavascript:alert(1)",   // browser trims the tab
		"java\x00script:alert(1)", // browser drops the NUL
		"vbscript:msgbox(1)",
		"VBScript:msgbox(1)",
		"data:text/html,<script>alert(1)</script>",
		"DATA:text/html,x",
		"blob:https://example.com/uuid",
		"javascript:",        // nothing after the colon is still a scheme
		"//example.com/path", // protocol relative: no scheme at all
		"example.com/path",   // no scheme
		"/local/path",        // no scheme
		"https://",           // scheme but no host
		"http://",            // scheme but no host
		"java script:alert(1)",
	}
	for _, raw := range reject {
		if err := checkURL(raw); err == nil {
			t.Errorf("checkURL(%q) = nil, want a rejection", raw)
		}
	}

	accept := []string{
		"https://example.com",
		"http://example.com/a?b=c#d",
		"ssh://git@10.0.0.5:2222",
		"vscode://file/Users/you/code",
		"smb://server/share",
		"rdp://host",
		"mailto:team@example.com",
		"tel:+123456",
	}
	for _, raw := range accept {
		if err := checkURL(raw); err != nil {
			t.Errorf("checkURL(%q) = %v, want it accepted", raw, err)
		}
	}
}

// The rejection text travels: into the log, into /healthz, and into the 503
// body of /api/nav. A private link can carry a token in its query, and Go's own
// url.Parse error quotes the whole input, so no url may be repeated back.
func TestCheckURLNeverEchoesTheValue(t *testing.T) {
	bad := []string{
		"https://diary.internal:8080/%zz",    // invalid escape
		"https://internal/%zz?secret=abc123", // invalid escape with a secret
		"https://[not-ipv6/",                 // invalid host
		"https://exa mple.com/\x7f",          // control character
		"javascript:\nalert(1)",
	}
	for _, raw := range bad {
		err := checkURL(raw)
		if err == nil {
			t.Errorf("checkURL(%q) = nil, want a rejection", raw)
			continue
		}
		msg := err.Error()
		if strings.Contains(msg, raw) || strings.Contains(msg, "secret=abc123") || strings.Contains(msg, "%zz") {
			t.Errorf("checkURL(%q) echoed the value back: %q", raw, msg)
		}
	}
}
