package nav

import (
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
	if cfg.Theme != ThemeAuto {
		t.Errorf("Theme = %q, want %q", cfg.Theme, ThemeAuto)
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
		"vscode://file/Users/simon/code",
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
	for _, want := range []string{
		"groups[0].links[0]: name is required",
		"groups[0].links[1] (NoURL): url is required",
		"groups[0].links[2] (BadScheme).url",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q is missing %q", msg, want)
		}
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
theme: "  DARK "
layout: DASHBOARD
groups:
  - name: "  开发  "
    links:
      - name: "  Gitea  "
        url: "  https://git.example.com  "
        tags: ["  git ", "", "   "]
        hotkey: " g "
`)
	if cfg.Theme != ThemeDark || cfg.Layout != LayoutDashboard {
		t.Errorf("Theme/Layout = %q/%q, want dark/dashboard", cfg.Theme, cfg.Layout)
	}
	link := cfg.Groups[0].Links[0]
	if link.Name != "Gitea" || link.URL != "https://git.example.com" || link.Hotkey != "g" {
		t.Errorf("link not trimmed: %+v", link)
	}
	if len(link.Tags) != 1 || link.Tags[0] != "git" {
		t.Errorf("Tags = %v, want [git]", link.Tags)
	}
}

func TestParseRejectsBadLayout(t *testing.T) {
	if _, _, err := Parse([]byte("layout: masonry\n" + minimalDoc)); err == nil {
		t.Fatal("Parse() accepted an unknown layout")
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

func TestParseRejectsMultiCharacterHotkey(t *testing.T) {
	cases := []string{
		"groups:\n  - name: x\n    links:\n      - name: a\n        url: https://a.example.com\n        hotkey: gh\n",
		"search:\n  hotkey: ab\n" + minimalDoc,
	}
	for _, doc := range cases {
		if _, _, err := Parse([]byte(doc)); err == nil {
			t.Error("Parse() accepted a multi-character hotkey")
		}
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
	if cfg.PinnedCount() != 1 {
		t.Errorf("PinnedCount() = %d, want 1", cfg.PinnedCount())
	}

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
