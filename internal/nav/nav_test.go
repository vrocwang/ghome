package nav

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const minimalDoc = `
groups:
  - name: 开发
    links:
      - name: Gitea
        url: https://git.example.com
`

func mustParse(t *testing.T, doc string) *Config {
	t.Helper()
	cfg, _, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return cfg
}

func parseWithWarnings(t *testing.T, doc string) (*Config, []string) {
	t.Helper()
	cfg, warnings, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return cfg, warnings
}

func TestParseAppliesDefaults(t *testing.T) {
	cfg := mustParse(t, minimalDoc)

	if cfg.Site.Title != DefaultTitle {
		t.Errorf("Site.Title = %q, want %q", cfg.Site.Title, DefaultTitle)
	}
	if cfg.Layout != LayoutGrid {
		t.Errorf("Layout = %q, want %q", cfg.Layout, LayoutGrid)
	}
	if cfg.Search.Enabled == nil || !*cfg.Search.Enabled {
		t.Error("Search.Enabled should default to true")
	}
	if cfg.Search.Hotkey != "/" {
		t.Errorf("Search.Hotkey = %q, want /", cfg.Search.Hotkey)
	}
	if cfg.Status.Enabled == nil || *cfg.Status.Enabled {
		t.Error("Status.Enabled should default to false: probing is opt-in")
	}
	if cfg.Status.Interval.Std() != 60*time.Second {
		t.Errorf("Status.Interval = %v, want 60s", cfg.Status.Interval.Std())
	}
	if cfg.Status.Timeout.Std() != 5*time.Second {
		t.Errorf("Status.Timeout = %v, want 5s", cfg.Status.Timeout.Std())
	}
	if got := cfg.LinkCount(); got != 1 {
		t.Errorf("LinkCount() = %d, want 1", got)
	}
	if cfg.Groups[0].Links[0].NewTab != nil {
		t.Error("NewTab should stay unset so the UI can apply its own default")
	}
}

// A typo in an optional key should not take the whole navigation page down.
func TestUnknownFieldsBecomeWarnings(t *testing.T) {
	cfg, warnings := parseWithWarnings(t, `
titel: 拼错在顶层
groups:
  - name: 开发
    descrpition: 拼错的字段
    links:
      - name: Gitea
        url: https://git.example.com
        hotkeys: g
`)

	if len(warnings) != 3 {
		t.Fatalf("warnings = %v, want 3 (one per unknown field)", warnings)
	}
	for _, want := range []string{"titel", "descrpition", "hotkeys"} {
		if !containsSubstring(warnings, want) {
			t.Errorf("warnings %v do not mention %q", warnings, want)
		}
	}
	// Line numbers make the typo findable.
	if !containsSubstring(warnings, "line") {
		t.Errorf("warnings %v should carry line numbers", warnings)
	}
	// The rest of the document still applies.
	if cfg.Site.Title != DefaultTitle {
		t.Errorf("Site.Title = %q", cfg.Site.Title)
	}
	if cfg.LinkCount() != 1 {
		t.Errorf("LinkCount() = %d, want the link to survive", cfg.LinkCount())
	}
}

// A wrong type is not a typo: it means the document does not say what its
// author meant, so it stays fatal.
func TestTypeMismatchIsFatal(t *testing.T) {
	_, _, err := Parse([]byte(`
groups:
  - name: 开发
    links: "not a list"
`))
	if err == nil {
		t.Fatal("Parse() accepted a string where a list was expected")
	}
}

func TestMalformedYAMLIsFatal(t *testing.T) {
	if _, _, err := Parse([]byte("groups: [")); err == nil {
		t.Fatal("Parse() accepted malformed yaml")
	}
}

// Only schemes that can execute or embed content are refused. An internal
// navigation page legitimately links to all sorts of app protocols.
func TestURLChecks(t *testing.T) {
	reject := []string{
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox(1)",
		"blob:https://example.com/abc",
		"/relative/path",
		"just-a-host.com",
		"https://",
	}
	for _, bad := range reject {
		t.Run("reject "+bad, func(t *testing.T) {
			_, _, err := Parse([]byte("groups:\n  - name: x\n    links:\n      - name: bad\n        url: \"" + bad + "\"\n"))
			if err == nil {
				t.Fatalf("Parse() accepted url %q", bad)
			}
		})
	}

	accept := []string{
		"https://git.example.com/x?y=1#z",
		"http://192.168.1.10:3000",
		"mailto:team@example.com",
		"tel:+8610000000000",
		// The whole point of dropping the old allow-list: internal tooling
		// links like these used to be rejected.
		"ssh://git@10.0.0.5:2222/srv/repo.git",
		"vscode://file/Users/you/code",
		"smb://fileserver/share",
		"rdp://10.0.0.9",
		"ftp://files.example.com/pub",
	}
	for _, ok := range accept {
		t.Run("accept "+ok, func(t *testing.T) {
			if _, _, err := Parse([]byte("groups:\n  - name: x\n    links:\n      - name: ok\n        url: \"" + ok + "\"\n")); err != nil {
				t.Fatalf("Parse() rejected %q: %v", ok, err)
			}
		})
	}
}

func TestParseReportsEveryProblemAtOnce(t *testing.T) {
	_, _, err := Parse([]byte(`
groups:
  - name: 开发
    links:
      - name: ""
        url: https://ok.example.com
      - name: NoURL
        url: ""
      - name: BadScheme
        url: "javascript:alert(1)"
`))
	if err == nil {
		t.Fatal("Parse() accepted a broken document")
	}
	msg := err.Error()
	// The coordinates, not the names: this text reaches /healthz and the 503
	// body of /api/nav, which answer without a token, and a name is exactly what
	// the private tier withholds. The line can be found from nav.yaml.
	for _, want := range []string{
		"groups[0].links[0]: name is required",
		"groups[0].links[1]: url is required",
		"groups[0].links[2].url",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q is missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "NoURL") || strings.Contains(msg, "BadScheme") {
		t.Errorf("error %q echoes the entry name", msg)
	}
}

func TestParseDropsEmptyGroups(t *testing.T) {
	cfg := mustParse(t, `
groups:
  - name: 空分组
    links: []
  - name: 有内容
    links:
      - name: A
        url: https://a.example.com
`)
	if len(cfg.Groups) != 1 || cfg.Groups[0].Name != "有内容" {
		t.Fatalf("Groups = %+v, want only 有内容", cfg.Groups)
	}
}

func TestParseRequiresAtLeastOneLink(t *testing.T) {
	for _, doc := range []string{"", "groups: []", "groups:\n  - name: 空\n    links: []\n"} {
		if _, _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("Parse(%q) accepted a document with no links", doc)
		}
	}
}

func TestParseTrimsAndCleansLists(t *testing.T) {
	cfg := mustParse(t, `
layout: DASHBOARD
groups:
  - name: "  开发  "
    links:
      - name: "  Gitea  "
        url: "  https://git.example.com  "
        tags: ["  git ", "", "   "]
        hotkey: " g "
`)
	if cfg.Layout != LayoutDashboard {
		t.Errorf("Layout = %q, want dashboard", cfg.Layout)
	}
	link := cfg.Groups[0].Links[0]
	if link.Name != "Gitea" || link.URL != "https://git.example.com" || link.Hotkey != "g" {
		t.Errorf("link not trimmed: %+v", link)
	}
	if len(link.Tags) != 1 || link.Tags[0] != "git" {
		t.Errorf("Tags = %v, want [git]", link.Tags)
	}
}

func TestParseValidatesSearchEngines(t *testing.T) {
	if _, _, err := Parse([]byte(`
search:
  engines:
    - name: NoPlaceholder
      url: "https://example.com/search"
` + minimalDoc)); err == nil {
		t.Fatal("Parse() accepted an engine url without {q}")
	}
}

// A hotkey that cannot work is a cosmetic mistake: the page still renders and
// every link still works, so it warns and falls back rather than rejecting the
// document and freezing the whole site on one typo.
func TestParseWarnsAndFallsBackOnMultiCharacterHotkey(t *testing.T) {
	cfg, warnings, err := Parse([]byte(
		"groups:\n  - name: x\n    links:\n      - name: a\n        url: https://a.example.com\n        hotkey: gh\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v, want the document accepted", err)
	}
	if got := cfg.Groups[0].Links[0].Hotkey; got != "" {
		t.Errorf("link hotkey = %q, want it dropped", got)
	}
	if len(warnings) == 0 {
		t.Error("no warning was reported for the dropped hotkey")
	}

	cfg, warnings, err = Parse([]byte("search:\n  hotkey: ab\n" + minimalDoc))
	if err != nil {
		t.Fatalf("Parse() error = %v, want the document accepted", err)
	}
	if got := cfg.Search.Hotkey; got != "/" {
		t.Errorf("search hotkey = %q, want the default /", got)
	}
	if len(warnings) == 0 {
		t.Error("no warning was reported for the search hotkey")
	}
}

// The same reasoning applies to a misspelled layout: fall back, say so, keep
// serving.
func TestParseWarnsAndFallsBackOnUnknownLayout(t *testing.T) {
	cfg, warnings, err := Parse([]byte("layout: masonry\n" + minimalDoc))
	if err != nil {
		t.Fatalf("Parse() error = %v, want the document accepted", err)
	}
	if cfg.Layout != LayoutGrid {
		t.Errorf("Layout = %q, want the %s fallback", cfg.Layout, LayoutGrid)
	}
	if len(warnings) == 0 {
		t.Error("no warning was reported for the unknown layout")
	}
}

// A misspelled colour word used to be emitted verbatim and dropped by the
// browser, so the page silently kept its default and nothing said why.
func TestParseWarnsOnUnknownColorKeyword(t *testing.T) {
	cfg, warnings, err := Parse([]byte("style:\n  accent: bleu\n" + minimalDoc))
	if err != nil {
		t.Fatalf("Parse() error = %v, want the document accepted", err)
	}
	if cfg.Style.Accent != "" {
		t.Errorf("accent = %q, want the unusable value dropped", cfg.Style.Accent)
	}
	if len(warnings) == 0 {
		t.Error("no warning was reported for the unknown colour")
	}

	// The real colour words must still pass.
	cfg, _, err = Parse([]byte("style:\n  accent: rebeccapurple\n  background: transparent\n" + minimalDoc))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Style.Accent != "rebeccapurple" || cfg.Style.Background != "transparent" {
		t.Errorf("valid colour words were dropped: accent=%q background=%q",
			cfg.Style.Accent, cfg.Style.Background)
	}
}

func TestDurationsAcceptStringsAndSeconds(t *testing.T) {
	cfg := mustParse(t, `
status:
  enabled: true
  interval: 45s
  timeout: 2s
  concurrency: 3
`+minimalDoc)

	if cfg.Status.Enabled == nil || !*cfg.Status.Enabled {
		t.Fatal("Status.Enabled was not set")
	}
	if got := cfg.Status.Interval.Std(); got != 45*time.Second {
		t.Errorf("Interval = %v, want 45s", got)
	}
	if got := cfg.Status.Timeout.Std(); got != 2*time.Second {
		t.Errorf("Timeout = %v, want 2s", got)
	}
	if cfg.Status.Concurrency != 3 {
		t.Errorf("Concurrency = %d, want 3", cfg.Status.Concurrency)
	}

	// A bare number is read as seconds.
	bare := mustParse(t, "status:\n  interval: 90\n"+minimalDoc)
	if got := bare.Status.Interval.Std(); got != 90*time.Second {
		t.Errorf("Interval = %v, want 90s", got)
	}

	// Nonsense is a real error, not something to paper over.
	if _, _, err := Parse([]byte("status:\n  interval: soon\n" + minimalDoc)); err == nil {
		t.Error("Parse() accepted a nonsense duration")
	}
}

func TestStatusIntervalIsClampedWithAWarning(t *testing.T) {
	cfg, warnings := parseWithWarnings(t, "status:\n  interval: 1s\n"+minimalDoc)
	if got := cfg.Status.Interval.Std(); got != 5*time.Second {
		t.Errorf("Interval = %v, want the 5s minimum", got)
	}
	if len(warnings) == 0 {
		t.Error("clamping the interval should say so")
	}
}

func TestStyleTokens(t *testing.T) {
	cfg := mustParse(t, `
style:
  accent: "#ff6600"
  background: "rgb(20, 20, 24)"
  surface: "hsl(220 10% 12%)"
  text: currentColor
  radius: 18
  fontSize: 15
  width: 1280
  density: compact
  cardStyle: flat
  font: "Inter, system-ui, sans-serif"
  customCss: "styles/custom.css"
`+minimalDoc)

	if cfg.Style.Accent != "#ff6600" {
		t.Errorf("Accent = %q", cfg.Style.Accent)
	}
	if cfg.Style.Radius != 18 || cfg.Style.FontSize != 15 || cfg.Style.Width != 1280 {
		t.Errorf("numeric tokens = %+v", cfg.Style)
	}
	if cfg.Style.Density != "compact" || cfg.Style.CardStyle != "flat" {
		t.Errorf("Density/CardStyle = %q/%q", cfg.Style.Density, cfg.Style.CardStyle)
	}
	if cfg.Style.CustomCSS != "styles/custom.css" {
		t.Errorf("CustomCSS = %q", cfg.Style.CustomCSS)
	}
}

// A mistyped colour is cosmetic: warn and drop it, keep the page.
func TestBadStyleTokensWarnAndAreDropped(t *testing.T) {
	cfg, warnings := parseWithWarnings(t, `
style:
  accent: "url(https://evil.example.com/leak)"
  background: "red; background-image: url(x)"
  radius: 900
  density: chunky
  customCss: "../../etc/passwd.css"
  font: "Inter; } body { display:none }"
`+minimalDoc)

	if cfg.Style.Accent != "" {
		t.Errorf("Accent = %q, want it dropped", cfg.Style.Accent)
	}
	if cfg.Style.Background != "" {
		t.Errorf("Background = %q, want it dropped", cfg.Style.Background)
	}
	if cfg.Style.Radius != 0 {
		t.Errorf("Radius = %d, want it dropped as out of range", cfg.Style.Radius)
	}
	if cfg.Style.Density != "" {
		t.Errorf("Density = %q, want it dropped", cfg.Style.Density)
	}
	if cfg.Style.CustomCSS != "" {
		t.Errorf("CustomCSS = %q, want the traversal attempt dropped", cfg.Style.CustomCSS)
	}
	if cfg.Style.Font != "" {
		t.Errorf("Font = %q, want it dropped", cfg.Style.Font)
	}
	if len(warnings) < 5 {
		t.Errorf("warnings = %v, want one per dropped token", warnings)
	}
	// The page itself is intact.
	if cfg.LinkCount() != 1 {
		t.Error("dropping style tokens must not lose links")
	}
}

func TestGroupColorIsValidated(t *testing.T) {
	cfg, warnings := parseWithWarnings(t, `
groups:
  - name: 好颜色
    color: "#2f9e6f"
    links:
      - name: A
        url: https://a.example.com
  - name: 坏颜色
    color: "expression(alert(1))"
    links:
      - name: B
        url: https://b.example.com
`)
	if cfg.Groups[0].Color != "#2f9e6f" {
		t.Errorf("good colour = %q", cfg.Groups[0].Color)
	}
	if cfg.Groups[1].Color != "" {
		t.Errorf("bad colour = %q, want it dropped", cfg.Groups[1].Color)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want exactly one", warnings)
	}
}

func TestIconValidation(t *testing.T) {
	// A short glyph, an image url, an inline data image and a same-origin path
	// are all usable.
	for _, ok := range []string{"🚀", "G", "https://cdn.example.com/i.svg", "data:image/png;base64,AAAA", "/api/icon?h=x"} {
		cfg := mustParse(t, "groups:\n  - name: x\n    links:\n      - name: a\n        url: https://a.example.com\n        icon: \""+ok+"\"\n")
		if cfg.Groups[0].Links[0].Icon != ok {
			t.Errorf("icon %q was dropped", ok)
		}
	}

	// A long non-url string is a mistake: warn and fall back to auto.
	cfg, warnings := parseWithWarnings(t, "groups:\n  - name: x\n    links:\n      - name: a\n        url: https://a.example.com\n        icon: \"this is not an icon\"\n")
	if cfg.Groups[0].Links[0].Icon != "" {
		t.Errorf("icon = %q, want it cleared", cfg.Groups[0].Links[0].Icon)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want one", warnings)
	}

	// A short string with whitespace is not a glyph the browser will draw: it
	// used to pass validation and then be silently ignored by the frontend, so
	// the config looked accepted while the icon never appeared.
	cfg, warnings = parseWithWarnings(t, "groups:\n  - name: x\n    links:\n      - name: a\n        url: https://a.example.com\n        icon: \"A B\"\n")
	if cfg.Groups[0].Links[0].Icon != "" {
		t.Errorf("icon with a space = %q, want it cleared with a warning", cfg.Groups[0].Links[0].Icon)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want one for the whitespace glyph", warnings)
	}
}

// The site logo is rendered as an image when it looks like a url and as text
// otherwise, and it was the one icon-shaped value with no validation at all.
func TestSiteLogoIsBounded(t *testing.T) {
	for _, ok := range []string{"🧭", "My Nav", "https://cdn.example.com/logo.png", "/assets/logo.svg"} {
		cfg := mustParse(t, "site:\n  logo: \""+ok+"\"\ngroups:\n  - name: x\n    links:\n      - name: a\n        url: https://a.example.com\n")
		if cfg.Site.Logo != ok {
			t.Errorf("logo %q was dropped", ok)
		}
	}

	cfg, warnings := parseWithWarnings(t, "site:\n  logo: \""+strings.Repeat("x", 40)+"\"\ngroups:\n  - name: x\n    links:\n      - name: a\n        url: https://a.example.com\n")
	if cfg.Site.Logo != "" {
		t.Errorf("logo = %q, want an overlong text logo cleared", cfg.Site.Logo)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want one", warnings)
	}

	// Whitespace is trimmed, like the other text fields.
	cfg = mustParse(t, "site:\n  logo: \"  🧭  \"\ngroups:\n  - name: x\n    links:\n      - name: a\n        url: https://a.example.com\n")
	if cfg.Site.Logo != "🧭" {
		t.Errorf("logo = %q, want it trimmed", cfg.Site.Logo)
	}
}

func TestPinnedAndProbe(t *testing.T) {
	cfg := mustParse(t, `
groups:
  - name: x
    links:
      - name: pinned
        url: https://a.example.com
        pinned: true
      - name: not-probed
        url: https://b.example.com
        probe: false
      - name: plain
        url: https://c.example.com
`)
	// Probe targets skip the opted-out link and keep the rest in order.
	targets := cfg.ProbeTargets()
	want := []string{"https://a.example.com", "https://c.example.com"}
	if strings.Join(targets, ",") != strings.Join(want, ",") {
		t.Errorf("ProbeTargets() = %v, want %v", targets, want)
	}

	// PageURLs is every link, which is what the icon resolver may serve.
	if got := cfg.PageURLs(); len(got) != 3 {
		t.Errorf("PageURLs() = %v, want all 3", got)
	}
}

func TestProbeTargetsSkipsNonHTTPAndDuplicates(t *testing.T) {
	cfg := mustParse(t, `
groups:
  - name: x
    links:
      - name: http
        url: https://a.example.com
      - name: dup
        url: https://a.example.com
      - name: mail
        url: mailto:team@example.com
      - name: ssh
        url: ssh://git@10.0.0.5/repo
      - name: vscode
        url: vscode://file/x
`)
	got := cfg.ProbeTargets()
	if len(got) != 1 || got[0] != "https://a.example.com" {
		t.Errorf("ProbeTargets() = %v, want just the deduped https link", got)
	}
}

func TestCSSColor(t *testing.T) {
	valid := []string{
		"#fff", "#ffff", "#ff6600", "#ff6600aa",
		"rgb(1,2,3)", "rgba(1, 2, 3, 0.5)", "hsl(220 10% 12%)", "hsla(220,10%,12%,.5)",
		"transparent", "currentColor", "rebeccapurple",
	}
	for _, v := range valid {
		if !CSSColor(v) {
			t.Errorf("CSSColor(%q) = false, want true", v)
		}
	}

	invalid := []string{
		"", "#ff", "#gggggg",
		"url(https://evil.example.com/x)",
		"red; background: url(x)",
		"red} body{display:none",
		"var(--x)",
		"expression(alert(1))",
	}
	for _, v := range invalid {
		if CSSColor(v) {
			t.Errorf("CSSColor(%q) = true, want false", v)
		}
	}
}

func TestCSSFontFamily(t *testing.T) {
	if !CSSFontFamily("Inter, system-ui, sans-serif") {
		t.Error("a normal font stack was rejected")
	}
	if !CSSFontFamily("'Noto Sans CJK SC', 苹方") {
		t.Error("a quoted CJK font stack was rejected")
	}
	for _, bad := range []string{"", "Inter; } body { display:none }", "a{b}", "url(x)"} {
		if CSSFontFamily(bad) {
			t.Errorf("CSSFontFamily(%q) = true, want false", bad)
		}
	}
}

func TestSafeRepoPath(t *testing.T) {
	valid := []string{"custom.css", "styles/custom.css", "a/b/c.css"}
	for _, v := range valid {
		if !safeRepoPath(v) {
			t.Errorf("safeRepoPath(%q) = false, want true", v)
		}
	}
	invalid := []string{
		"", "/etc/passwd", "../custom.css", "styles/../../x.css",
		"custom.txt", "custom.css/../x", "C:/x.css",
	}
	for _, v := range invalid {
		if safeRepoPath(v) {
			t.Errorf("safeRepoPath(%q) = true, want false", v)
		}
	}
}

func containsSubstring(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// The icon endpoint is queried by host and resolves each host once, from the
// first link that uses it. Warming more than one URL per host therefore fetches
// icons that can never be served -- two links to nas:8080 and nas:9090 share a
// single icon.
func TestIconURLsKeepsOnePerHost(t *testing.T) {
	cfg := mustParse(t, `
groups:
  - name: g
    links:
      - {name: a, url: "http://nas:8080/"}
      - {name: b, url: "http://nas:9090/deep/path"}
      - {name: c, url: "https://other.example.com/"}
      - {name: d, url: "ssh://git@host:2222"}
      - {name: e, url: "https://OTHER.example.com/x"}
`)

	got := cfg.IconURLs()
	want := []string{"http://nas:8080/", "https://other.example.com/"}
	if len(got) != len(want) {
		t.Fatalf("IconURLs() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("IconURLs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// PageURLs must keep reporting everything: it is what the page renders.
	if n := len(cfg.PageURLs()); n != 5 {
		t.Errorf("PageURLs() = %d urls, want all 5", n)
	}
}

const privateDoc = `
groups:
  - name: public
    links:
      - {name: a, url: "https://a.example.com"}
  - name: secret
    private: true
    links:
      - {name: b, url: "https://b.example.com"}
      - {name: c, url: "https://c.example.com"}
  - name: mixed
    links:
      - {name: d, url: "https://d.example.com"}
      - {name: e, url: "https://e.example.com", private: true}
  - name: all-private
    links:
      - {name: f, url: "https://f.example.com", private: true}
`

func TestPublicDropsPrivateEntries(t *testing.T) {
	cfg := mustParse(t, privateDoc)

	if got := cfg.PrivateEntries(); got != 4 {
		t.Errorf("PrivateEntries() = %d, want 4 (2 in the private group + 1 mixed + 1 alone)", got)
	}

	pub := cfg.Public()

	var names []string
	for _, g := range pub.Groups {
		names = append(names, g.Name)
	}
	// "secret" is private, and "all-private" is left with nothing. The second
	// one matters as much as the first: an empty card would still show the
	// group's name, and the name is usually the revealing part.
	want := []string{"public", "mixed"}
	if len(names) != len(want) {
		t.Fatalf("groups = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("groups[%d] = %q, want %q", i, names[i], want[i])
		}
	}

	var links []string
	for _, g := range pub.Groups {
		for _, l := range g.Links {
			links = append(links, l.Name)
		}
	}
	wantLinks := []string{"a", "d"}
	if len(links) != len(wantLinks) || links[0] != "a" || links[1] != "d" {
		t.Errorf("links = %v, want %v", links, wantLinks)
	}

	// The original must be untouched: it is what an unlocked caller gets, and
	// it is the same value the store serves to everyone else.
	if len(cfg.Groups) != 4 || cfg.PrivateEntries() != 4 {
		t.Errorf("Public() mutated the receiver: %d groups, %d private", len(cfg.Groups), cfg.PrivateEntries())
	}
}

func TestPublicOnConfigWithNoPrivateEntries(t *testing.T) {
	cfg := mustParse(t, minimalDoc)
	if got := cfg.PrivateEntries(); got != 0 {
		t.Errorf("PrivateEntries() = %d, want 0", got)
	}
	if got := len(cfg.Public().Groups); got != len(cfg.Groups) {
		t.Errorf("Public() dropped %d groups from a config with nothing private",
			len(cfg.Groups)-got)
	}
}

// Warnings name the entry they came from, so handing them to an anonymous caller
// would give away what the filtering just removed.
func TestPublicPayloadDropsWarnings(t *testing.T) {
	cfg := mustParse(t, `
groups:
  - name: secret
    private: true
    links:
      - {name: 日记, url: "https://diary.example.com", hotkey: "TOOLONG"}
`)
	payload := Payload{
		Config: *cfg,
		Meta:   Meta{Revision: "rev-1", Warnings: []string{"groups[0].links[0] (日记).hotkey: bad"}},
	}

	if len(payload.Public().Meta.Warnings) != 0 {
		t.Errorf("Public() kept warnings: %v", payload.Public().Meta.Warnings)
	}
	if len(payload.Meta.Warnings) != 1 {
		t.Error("Public() mutated the payload it was called on")
	}
}

// Tags used to be trimmed to two in the browser while the search box still
// matched all of them, so a config could carry words that matched nothing
// visible. The cap belongs here: it is applied once and it is announced.
func TestTagsAreCappedWithAWarning(t *testing.T) {
	doc := `
groups:
  - name: g
    links:
      - {name: a, url: https://a.example.com, tags: [t1, t2, t3, t4, t5, t6, t7, t8, t9, t10]}
      - {name: b, url: https://b.example.com, tags: ["", "  x  ", "   "]}
`
	cfg, warnings, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if got := len(cfg.Groups[0].Links[0].Tags); got != maxTags {
		t.Errorf("tags on the first link = %d, want the cap %d", got, maxTags)
	}
	if got := cfg.Groups[0].Links[0].Tags; got[len(got)-1] != "t8" {
		t.Errorf("tags = %v, want the first %d kept in order", got, maxTags)
	}
	if !containsSubstring(warnings, "keeping the first 8 of 10") {
		t.Errorf("warnings = %v, want one that says the list was cut", warnings)
	}

	// Blank entries are dropped, not counted, and surviving ones are trimmed.
	short := cfg.Groups[0].Links[1].Tags
	if len(short) != 1 || short[0] != "x" {
		t.Errorf("tags = %v, want [x]", short)
	}
	if containsSubstring(warnings, "links[1]") {
		t.Errorf("warnings = %v: a link within the cap must not be warned about", warnings)
	}
}

// A boolean has to be written as a real YAML boolean. Reading the written
// spellings ("false", yes, off, 1) was this package's most elaborate piece and
// is gone: the document is refused instead, by field and line, so the author
// fixes it in one push. Guessing is the thing that must not come back, because
// `private` defaults to publishing the entry -- a `"true"` this code read as
// anything but true would turn a hidden link into a public one.
func TestBooleansMustBeWrittenAsBooleans(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want string
	}{
		{"quoted false", "search:\n  enabled: \"false\"\n", "search.enabled: expected true or false (line 2)"},
		{"uppercase word", "search:\n  enabled: \"YES\"\n", "search.enabled"},
		{"bare yes", "groups:\n  - name: g\n    private: yes\n", "groups[0].private"},
		{"bare off", "groups:\n  - name: g\n    links:\n      - {name: a, url: https://a.example.com, pinned: on}\n", "groups[0].links[0].pinned"},
		{"number", "groups:\n  - name: g\n    links:\n      - {name: a, url: https://a.example.com, probe: 0}\n", "groups[0].links[0].probe"},
		{"list", "groups:\n  - name: g\n    links:\n      - {name: a, url: https://a.example.com, newTab: [false]}\n", "groups[0].links[0].newTab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Parse([]byte(tc.doc))
			if err == nil {
				t.Fatal("Parse() accepted a value that is not a boolean")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}

	// The value itself is never repeated back: this text reaches /healthz and
	// the 503 body of /api/nav, and neither is authenticated.
	if _, _, err := Parse([]byte("groups:\n  - name: g\n    private: \"https://secret.example.com?t=abc\"\n")); err == nil {
		t.Fatal("a URL in a boolean field was accepted")
	} else if strings.Contains(err.Error(), "secret.example.com") {
		t.Errorf("the error quoted the value: %v", err)
	}

	// The real thing still works, including a bare TRUE and a boolean behind an
	// alias. Everything outside a boolean field is untouched, so a link named
	// "no" is still a link named "no".
	cfg, _, err := Parse([]byte(`
search:
  enabled: false
status:
  enabled: &b true
groups:
  - name: g
    private: false
    links:
      - {name: "no", url: https://a.example.com, newTab: false, pinned: *b, probe: false}
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Search.Enabled == nil || *cfg.Search.Enabled {
		t.Errorf("search.enabled = %v, want false", cfg.Search.Enabled)
	}
	if cfg.Status.Enabled == nil || !*cfg.Status.Enabled {
		t.Errorf("status.enabled = %v, want true", cfg.Status.Enabled)
	}
	link := cfg.Groups[0].Links[0]
	if link.Name != "no" {
		t.Errorf("name = %q, want the boolean word to survive as text", link.Name)
	}
	if link.NewTab == nil || *link.NewTab {
		t.Errorf("newTab = %v, want false", link.NewTab)
	}
	if !link.Pinned {
		t.Error("pinned = false, want true (an alias to a real boolean)")
	}
	if link.Probe == nil || *link.Probe {
		t.Errorf("probe = %v, want false", link.Probe)
	}
}

// Link.ProbeTarget is the one answer the browser gets, so it has to agree with
// what the prober is actually given.
func TestProbeTargetMatchesTheProbeList(t *testing.T) {
	cfg, _, err := Parse([]byte(`
groups:
  - name: g
    links:
      - {name: http, url: "https://a.example.com"}
      - {name: 关闭, url: "https://b.example.com", probe: false}
      - {name: ssh, url: "ssh://git@10.0.0.5:2222"}
      - {name: 邮件, url: "mailto:team@example.com"}
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := []bool{true, false, false, false}
	links := cfg.Groups[0].Links
	for i, l := range links {
		if l.ProbeTarget != want[i] {
			t.Errorf("%s.ProbeTarget = %v, want %v", l.Name, l.ProbeTarget, want[i])
		}
	}

	targets := cfg.ProbeTargets()
	if len(targets) != 1 || targets[0] != "https://a.example.com" {
		t.Fatalf("ProbeTargets() = %v, want just the probeable http link", targets)
	}
	for _, l := range links {
		listed := slices.Contains(targets, l.URL)
		if listed != l.ProbeTarget {
			t.Errorf("%s: ProbeTargets contains it = %v, ProbeTarget = %v", l.Name, listed, l.ProbeTarget)
		}
	}
}

// Cosmetic text fields are trimmed, like every other cosmetic value.
func TestDescriptionsAndGroupIconsAreTrimmed(t *testing.T) {
	cfg, _, err := Parse([]byte(`
groups:
  - name: "  g  "
    icon: "  🔧  "
    description: "  组说明  "
    links:
      - {name: "  a  ", url: https://a.example.com, description: "  リンク説明  "}
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	g := cfg.Groups[0]
	if g.Name != "g" || g.Icon != "🔧" || g.Description != "组说明" {
		t.Errorf("group = %q / %q / %q, want trimmed", g.Name, g.Icon, g.Description)
	}
	if got := g.Links[0].Description; got != "リンク説明" {
		t.Errorf("link description = %q, want it trimmed", got)
	}
}

// style.dark is validated like the light colours: usable keys survive with
// their values canonicalised, and everything else is reported rather than
// silently dropped -- a typo here would look like "dark mode ignores me".
func TestDarkStyleOverrides(t *testing.T) {
	cfg, warnings := parseWithWarnings(t, `
style:
  accent: "#ff6600"
  dark:
    accent: " #6f95ff "
    Background: "#0b0d11"
    radius: 30
    nonsense: "#123456"
    muted: "url(https://evil.example.com/leak)"
`+minimalDoc)

	if got := cfg.Style.Dark["accent"]; got != "#6f95ff" {
		t.Errorf("dark accent = %q, want it trimmed", got)
	}
	// Keys are case-insensitive, which is worth pinning: the CSS property they
	// map to is spelled lowercase and a lookup miss would drop the colour.
	if got := cfg.Style.Dark["background"]; got != "#0b0d11" {
		t.Errorf("dark background = %q, want it canonicalised to lowercase", got)
	}
	if _, ok := cfg.Style.Dark["radius"]; ok {
		t.Error("dark radius survived; geometry is not themeable")
	}
	if _, ok := cfg.Style.Dark["nonsense"]; ok {
		t.Error("an unknown colour key survived")
	}
	if _, ok := cfg.Style.Dark["muted"]; ok {
		t.Error("a dangerous colour value survived")
	}
	if len(warnings) < 3 {
		t.Errorf("warnings = %v, want one per rejected dark token", warnings)
	}
}

// A config with no dark block must leave Style.Dark nil rather than an empty
// map, so the renderer emits no dark selector at all.
func TestNoDarkBlockLeavesNoOverrides(t *testing.T) {
	cfg := mustParse(t, `
style:
  accent: "#ff6600"
`+minimalDoc)
	if cfg.Style.Dark != nil {
		t.Errorf("Style.Dark = %v, want nil", cfg.Style.Dark)
	}
}
