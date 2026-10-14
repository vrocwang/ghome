// Package nav defines the navigation document that lives in git as nav.yaml.
//
// The document is the single source of truth for the page: the server parses
// and validates it, then hands it to the browser as JSON.
//
// Two classes of problem are treated differently, and deliberately so:
//
//   - Structural problems (a link with no url, a dangerous url scheme, a
//     malformed document) fail the whole document, because serving it would
//     produce a page that lies about what its author wrote.
//   - Cosmetic problems (an unknown field, an unusable colour value) only
//     produce warnings, because rejecting an entire navigation page over one
//     typo in an optional key is a worse outcome than serving the page without
//     that one key applied. Warnings surface through /healthz and the UI.
package nav

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Layout values accepted by Config.Layout.
const (
	// LayoutGrid is the refined card layout: roomy, one card per group.
	LayoutGrid = "grid"
	// LayoutDashboard is dense and information-first, with a summary bar.
	LayoutDashboard = "dashboard"
	// LayoutList is a single column of compact wrappable links.
	LayoutList = "list"
)

// DefaultTitle is used when site.title is empty.
const DefaultTitle = "导航"

// Config is the root of nav.yaml.
type Config struct {
	Site   Site    `yaml:"site" json:"site"`
	Layout string  `yaml:"layout" json:"layout"`
	Style  Style   `yaml:"style" json:"style"`
	Search Search  `yaml:"search" json:"search"`
	Status Status  `yaml:"status" json:"status"`
	Groups []Group `yaml:"groups" json:"groups"`
}

// Site holds page level branding.
type Site struct {
	Title    string `yaml:"title" json:"title"`
	Subtitle string `yaml:"subtitle" json:"subtitle"`
	Logo     string `yaml:"logo" json:"logo"`
	Footer   string `yaml:"footer" json:"footer"`
}

// Style holds design tokens. They are emitted as CSS custom properties by
// /theme.css, which is what lets the look be changed from git without
// rebuilding the binary.
//
// Every field is optional. A value that does not parse is ignored with a
// warning rather than rejected, because a mistyped colour should not take the
// whole page down.
type Style struct {
	Accent     string `yaml:"accent" json:"accent,omitempty"`
	Background string `yaml:"background" json:"background,omitempty"`
	Surface    string `yaml:"surface" json:"surface,omitempty"`
	Text       string `yaml:"text" json:"text,omitempty"`
	Muted      string `yaml:"muted" json:"muted,omitempty"`
	Border     string `yaml:"border" json:"border,omitempty"`
	Font       string `yaml:"font" json:"font,omitempty"`

	// Numeric tokens, in pixels.
	Radius   int `yaml:"radius" json:"radius,omitempty"`
	FontSize int `yaml:"fontSize" json:"fontSize,omitempty"`
	Width    int `yaml:"width" json:"width,omitempty"`
	IconSize int `yaml:"iconSize" json:"iconSize,omitempty"`

	// Density is "comfortable" (default) or "compact".
	Density string `yaml:"density" json:"density,omitempty"`

	// CardStyle is "soft" (default), "flat" or "strong".
	CardStyle string `yaml:"cardStyle" json:"cardStyle,omitempty"`

	// CustomCSS names a file in the same repository, for example
	// "custom.css". Its contents are appended to /theme.css, so a full
	// restyle rides the same push-to-update path as the rest of the config.
	// Not shipped to the browser as a path: the server resolves it.
	CustomCSS string `yaml:"customCss" json:"-"`
}

// Search configures the filter box and the optional web search engines.
type Search struct {
	Enabled *bool    `yaml:"enabled" json:"enabled"`
	Hotkey  string   `yaml:"hotkey" json:"hotkey"`
	Engines []Engine `yaml:"engines" json:"engines,omitempty"`
}

// Engine is a web search engine. {q} in URL is replaced with the query.
type Engine struct {
	Name    string `yaml:"name" json:"name"`
	URL     string `yaml:"url" json:"url"`
	Keyword string `yaml:"keyword" json:"keyword,omitempty"`
}

// Status configures the optional service health prober. It is off by default:
// when disabled the server issues no probe traffic at all.
type Status struct {
	Enabled     *bool    `yaml:"enabled" json:"enabled"`
	Interval    Duration `yaml:"interval" json:"interval,omitempty"`
	Timeout     Duration `yaml:"timeout" json:"timeout,omitempty"`
	Concurrency int      `yaml:"concurrency" json:"concurrency,omitempty"`
}

// Group is one card of links.
type Group struct {
	Name        string `yaml:"name" json:"name"`
	Icon        string `yaml:"icon" json:"icon"`
	Description string `yaml:"description" json:"description"`
	// Color tints this group's card and heading. Same syntax as style.accent.
	Color string `yaml:"color" json:"color,omitempty"`
	// Private hides the whole group, links included, from a caller that has not
	// proved it may see it.
	Private bool   `yaml:"private" json:"private,omitempty"`
	Links   []Link `yaml:"links" json:"links"`
}

// Link is a single navigation entry.
type Link struct {
	Name        string   `yaml:"name" json:"name"`
	URL         string   `yaml:"url" json:"url"`
	Icon        string   `yaml:"icon" json:"icon"`
	Description string   `yaml:"description" json:"description"`
	Tags        []string `yaml:"tags" json:"tags,omitempty"`
	// Hotkey is a single character that jumps straight to this link.
	Hotkey string `yaml:"hotkey" json:"hotkey,omitempty"`
	// NewTab defaults to true when omitted.
	NewTab *bool `yaml:"newTab" json:"newTab,omitempty"`
	// Pinned lifts the link into the pinned strip at the top of the page.
	Pinned bool `yaml:"pinned" json:"pinned,omitempty"`
	// Probe is false to exclude this link from status probing. Nil means the
	// default, which is to probe.
	Probe *bool `yaml:"probe" json:"probe,omitempty"`
	// Private hides this link from a caller that has not proved it may see it.
	Private bool `yaml:"private" json:"private,omitempty"`
}

// Meta describes where the loaded revision came from. It is filled in by the
// server, never by nav.yaml.
type Meta struct {
	Revision string `json:"revision"`
	// ThemeRevision changes whenever either the document or its stylesheet
	// changes. The browser compares it to decide whether its copy of
	// /theme.css is stale. Using Revision alone would miss a push that only
	// touched custom.css, because the document's blob SHA does not move.
	ThemeRevision string    `json:"themeRevision,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
	Source        string    `json:"source"`
	GroupCount    int       `json:"groupCount"`
	LinkCount     int       `json:"linkCount"`
	PinnedCount   int       `json:"pinnedCount"`
	Warnings      []string  `json:"warnings,omitempty"`
}

// Payload is the JSON shape served by GET /api/nav: the config inlined plus
// its revision metadata.
type Payload struct {
	Config
	Meta Meta `json:"meta"`
	// Private is filled per request, because whether the private entries are
	// included depends on the caller's token. It is absent when the config has
	// nothing private, which is how the frontend knows not to offer a lock.
	Private *PrivateView `json:"private,omitempty"`
}

// PrivateView tells the frontend whether the lock affordance is relevant.
type PrivateView struct {
	// Total counts the entries marked private, revealed or not.
	Total int `json:"total"`
	// Locked is true when they were withheld from this response.
	Locked bool `json:"locked"`
}

// Duration is a time.Duration that reads and writes Go duration strings
// ("60s", "5m") in YAML instead of raw nanoseconds. A bare number is accepted
// and read as seconds.
type Duration time.Duration

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// MarshalJSON renders the duration the way it was written.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalYAML accepts either a duration string or a number of seconds.
//
// The tag is inspected rather than just trying a string decode: yaml.v3 will
// happily decode an integer scalar into a string, which would turn
// `interval: 90` into the unparseable duration "90".
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	switch node.Tag {
	case "!!int", "!!float":
		var seconds float64
		if err := node.Decode(&seconds); err != nil {
			return errors.New("expected a duration such as 60s, or a number of seconds")
		}
		*d = Duration(time.Duration(seconds * float64(time.Second)))
		return nil
	}

	var s string
	if err := node.Decode(&s); err != nil {
		return errors.New("expected a duration such as 60s, or a number of seconds")
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("invalid duration %q: expected something like 60s or 5m", s)
	}
	*d = Duration(parsed)
	return nil
}

// LinkCount totals the links across all groups.
func (c *Config) LinkCount() int {
	n := 0
	for _, g := range c.Groups {
		n += len(g.Links)
	}
	return n
}

// PinnedCount totals the links marked pinned.
func (c *Config) PinnedCount() int {
	n := 0
	for _, g := range c.Groups {
		for _, l := range g.Links {
			if l.Pinned {
				n++
			}
		}
	}
	return n
}

// ProbeTargets returns the URLs worth health probing: http(s) links that did
// not opt out. Everything else (mailto:, ssh:, vscode:, ...) has nothing to
// probe and is left alone.
func (c *Config) ProbeTargets() []string {
	seen := make(map[string]bool)
	var out []string
	for _, g := range c.Groups {
		for _, l := range g.Links {
			if l.Probe != nil && !*l.Probe {
				continue
			}
			lower := strings.ToLower(l.URL)
			if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
				continue
			}
			if seen[l.URL] {
				continue
			}
			seen[l.URL] = true
			out = append(out, l.URL)
		}
	}
	return out
}

// PageURLs returns every configured link URL, which is the set the icon
// resolver is allowed to serve.
func (c *Config) PageURLs() []string {
	var out []string
	for _, g := range c.Groups {
		for _, l := range g.Links {
			out = append(out, l.URL)
		}
	}
	return out
}

// IconURLs returns one page URL per host: the URL whose icon will actually be
// served for that host.
//
// /api/icon is queried by host, and a host's icon is resolved once, from the
// first configured link that uses it. So these are the only URLs worth
// fetching. Everything that fetches icons must use this rather than PageURLs():
// warming every link on a host costs a page fetch and an icon fetch per extra
// link, and none of those results can ever be reached — two links to nas:8080
// and nas:9090 share a single icon.
func (c *Config) IconURLs() []string {
	seen := make(map[string]struct{}, len(c.Groups))
	out := make([]string, 0, len(c.Groups))
	for _, raw := range c.PageURLs() {
		host := hostKey(raw)
		if host == "" {
			continue
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, raw)
	}
	return out
}

// hostKey is the hostname a URL is filed under: lowercase, port stripped, and
// empty for anything that is not http(s). /api/icon is keyed by this, so every
// place that groups links by host has to agree on it.
func hostKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// PrivateOnlyHosts lists the hostnames that no public entry uses.
//
// Those are the ones an anonymous caller must not be able to probe for: the
// icon endpoint answering for such a host would confirm the host is in the
// config, which is the same disclosure that filtering /api/status avoids.
func (c *Config) PrivateOnlyHosts() map[string]struct{} {
	public := make(map[string]struct{}, len(c.Groups))
	private := make(map[string]struct{}, len(c.Groups))

	for _, g := range c.Groups {
		for _, l := range g.Links {
			host := hostKey(l.URL)
			if host == "" {
				continue
			}
			if g.Private || l.Private {
				private[host] = struct{}{}
				continue
			}
			public[host] = struct{}{}
		}
	}

	out := make(map[string]struct{}, len(private))
	for host := range private {
		if _, shared := public[host]; !shared {
			out[host] = struct{}{}
		}
	}
	return out
}

// Public returns a copy with every private entry removed.
//
// A group left with no links is dropped as well as a private one: an empty card
// would still show the group's name, and the name is usually the revealing part.
func (c *Config) Public() *Config {
	out := *c
	out.Groups = make([]Group, 0, len(c.Groups))
	for _, g := range c.Groups {
		if g.Private {
			continue
		}
		links := make([]Link, 0, len(g.Links))
		for _, l := range g.Links {
			if l.Private {
				continue
			}
			links = append(links, l)
		}
		if len(links) == 0 {
			continue
		}
		g.Links = links
		out.Groups = append(out.Groups, g)
	}
	return &out
}

// PrivateEntries counts the entries a public view withholds: private links,
// plus every link of a private group.
func (c *Config) PrivateEntries() int {
	n := 0
	for _, g := range c.Groups {
		if g.Private {
			n += len(g.Links)
			continue
		}
		for _, l := range g.Links {
			if l.Private {
				n++
			}
		}
	}
	return n
}

// Public returns the payload with the private entries removed, keeping Meta and
// the per-request view intact.
func (p Payload) Public() Payload {
	out := p
	out.Config = *p.Config.Public()
	// Warnings name the entry they came from -- "groups[0].links[0] (日记).hotkey:
	// ..." -- so they would give away exactly what the filtering just removed.
	// The operator still sees them in /healthz, and in the page once unlocked.
	out.Meta.Warnings = nil
	return out
}

// VisibleURLs lists the URLs a response exposes, which is what /api/status is
// allowed to talk about: a probe result for a withheld link would give the link
// away.
func (p Payload) VisibleURLs() map[string]struct{} {
	out := make(map[string]struct{}, 16)
	for _, g := range p.Groups {
		for _, l := range g.Links {
			out[l.URL] = struct{}{}
		}
	}
	return out
}

// Parse decodes, fills defaults into, and validates a nav.yaml document.
//
// It returns the config, any non-fatal warnings, and an error. Warnings never
// prevent the config from being used.
func Parse(data []byte) (*Config, []string, error) {
	warnings, err := collectUnknownFields(data)
	if err != nil {
		return nil, nil, err
	}

	// Decode leniently: the strict pass above may have stopped filling the
	// struct at the first unknown key.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, warnings, fmt.Errorf("invalid yaml: %w", err)
	}

	normalizeWarnings, err := c.normalize()
	warnings = append(warnings, normalizeWarnings...)
	if err != nil {
		return nil, warnings, err
	}
	return &c, warnings, nil
}

// collectUnknownFields runs a strict decode purely to catch typo'd keys, which
// are reported as warnings rather than failures. Type mismatches and malformed
// documents stay fatal.
func collectUnknownFields(data []byte) ([]string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var probe Config
	err := dec.Decode(&probe)
	if err == nil {
		return nil, nil
	}

	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return nil, fmt.Errorf("invalid yaml: %w", err)
	}

	var warnings, fatal []string
	for _, msg := range typeErr.Errors {
		if isUnknownField(msg) {
			warnings = append(warnings, "ignored unknown field ("+msg+")")
			continue
		}
		fatal = append(fatal, msg)
	}
	if len(fatal) > 0 {
		return warnings, fmt.Errorf("invalid yaml: %s", strings.Join(fatal, "; "))
	}
	return warnings, nil
}

func isUnknownField(msg string) bool {
	return strings.Contains(msg, "not found in type")
}

// problems accumulates every validation error so one push reports all of them
// at once instead of one per round trip.
type problems []string

func (p *problems) addf(format string, args ...any) {
	*p = append(*p, fmt.Sprintf(format, args...))
}

func (c *Config) normalize() ([]string, error) {
	var p problems
	var warnings []string
	warn := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	c.Site.Title = strings.TrimSpace(c.Site.Title)
	if c.Site.Title == "" {
		c.Site.Title = DefaultTitle
	}

	c.Layout = strings.ToLower(strings.TrimSpace(c.Layout))
	if c.Layout == "" {
		c.Layout = LayoutGrid
	}
	switch c.Layout {
	case LayoutGrid, LayoutDashboard, LayoutList:
	default:
		// Cosmetic: the page renders fine in the fallback layout, so rejecting
		// the document over it would freeze the whole site on a typo.
		warn("layout: %q is not one of grid, dashboard, list, using %s", c.Layout, LayoutGrid)
		c.Layout = LayoutGrid
	}

	c.normalizeStyle(warn)

	if c.Search.Enabled == nil {
		c.Search.Enabled = boolPtr(true)
	}
	c.Search.Hotkey = strings.TrimSpace(c.Search.Hotkey)
	if c.Search.Hotkey == "" {
		c.Search.Hotkey = "/"
	}
	if len([]rune(c.Search.Hotkey)) > 1 {
		warn("search.hotkey: %q is not a single character, using /", c.Search.Hotkey)
		c.Search.Hotkey = "/"
	}

	engines := make([]Engine, 0, len(c.Search.Engines))
	for i, e := range c.Search.Engines {
		e.Name = strings.TrimSpace(e.Name)
		e.URL = strings.TrimSpace(e.URL)
		e.Keyword = strings.TrimSpace(e.Keyword)
		if e.Name == "" {
			p.addf("search.engines[%d]: name is required", i)
		}
		if !strings.Contains(e.URL, "{q}") {
			p.addf("search.engines[%d] (%s): url must contain the {q} placeholder", i, e.Name)
		} else if err := checkURL(strings.ReplaceAll(e.URL, "{q}", "x")); err != nil {
			p.addf("search.engines[%d] (%s).url: %v", i, e.Name, err)
		}
		engines = append(engines, e)
	}
	c.Search.Engines = engines

	if c.Status.Enabled == nil {
		c.Status.Enabled = boolPtr(false)
	}
	if c.Status.Interval <= 0 {
		c.Status.Interval = Duration(60 * time.Second)
	}
	if c.Status.Interval < Duration(5*time.Second) {
		warn("status.interval: raised to the 5s minimum")
		c.Status.Interval = Duration(5 * time.Second)
	}
	if c.Status.Timeout <= 0 {
		c.Status.Timeout = Duration(5 * time.Second)
	}
	if c.Status.Concurrency <= 0 {
		c.Status.Concurrency = 8
	}
	if c.Status.Concurrency > 64 {
		warn("status.concurrency: capped at 64")
		c.Status.Concurrency = 64
	}

	groups := make([]Group, 0, len(c.Groups))
	for gi, g := range c.Groups {
		g.Name = strings.TrimSpace(g.Name)
		if g.Name == "" {
			p.addf("groups[%d]: name is required", gi)
		}
		if g.Color = strings.TrimSpace(g.Color); g.Color != "" && !CSSColor(g.Color) {
			warn("groups[%d] (%s).color: %q is not a usable css colour, ignored", gi, g.Name, g.Color)
			g.Color = ""
		}

		links := make([]Link, 0, len(g.Links))
		for li, l := range g.Links {
			l.Name = strings.TrimSpace(l.Name)
			l.URL = strings.TrimSpace(l.URL)
			l.Hotkey = strings.TrimSpace(l.Hotkey)
			if l.Name == "" {
				p.addf("groups[%d].links[%d]: name is required", gi, li)
			}
			if l.URL == "" {
				p.addf("groups[%d].links[%d] (%s): url is required", gi, li, l.Name)
			} else if err := checkURL(l.URL); err != nil {
				p.addf("groups[%d].links[%d] (%s).url: %v", gi, li, l.Name, err)
			}
			if len([]rune(l.Hotkey)) > 1 {
				warn("groups[%d].links[%d] (%s).hotkey: %q is not a single character, ignored",
					gi, li, l.Name, l.Hotkey)
				l.Hotkey = ""
			}
			l.Icon = strings.TrimSpace(l.Icon)
			if l.Icon != "" && !usableIcon(l.Icon) {
				warn("groups[%d].links[%d] (%s).icon: %q is neither an image url nor a short glyph, falling back to auto",
					gi, li, l.Name, l.Icon)
				l.Icon = ""
			}
			l.Tags = cleanList(l.Tags)
			links = append(links, l)
		}
		g.Links = links
		if len(links) == 0 {
			// An empty group is almost always work in progress; drop it rather
			// than rejecting the whole document.
			continue
		}
		groups = append(groups, g)
	}
	c.Groups = groups

	if len(groups) == 0 {
		p.addf("no links found: nav.yaml needs at least one group containing at least one link")
	}

	if len(p) > 0 {
		return warnings, fmt.Errorf("%s", strings.Join(p, "; "))
	}
	return warnings, nil
}

func (c *Config) normalizeStyle(warn func(string, ...any)) {
	s := &c.Style

	for name, v := range map[string]*string{
		"accent":     &s.Accent,
		"background": &s.Background,
		"surface":    &s.Surface,
		"text":       &s.Text,
		"muted":      &s.Muted,
		"border":     &s.Border,
	} {
		*v = strings.TrimSpace(*v)
		if *v != "" && !CSSColor(*v) {
			warn("style.%s: %q is not a usable css colour, ignored", name, *v)
			*v = ""
		}
	}

	s.Font = strings.TrimSpace(s.Font)
	if s.Font != "" && !CSSFontFamily(s.Font) {
		warn("style.font: %q is not a usable font-family list, ignored", s.Font)
		s.Font = ""
	}

	s.Density = strings.ToLower(strings.TrimSpace(s.Density))
	switch s.Density {
	case "", "comfortable", "compact":
	default:
		warn("style.density: %q is not one of comfortable, compact, ignored", s.Density)
		s.Density = ""
	}

	s.CardStyle = strings.ToLower(strings.TrimSpace(s.CardStyle))
	switch s.CardStyle {
	case "", "soft", "flat", "strong":
	default:
		warn("style.cardStyle: %q is not one of soft, flat, strong, ignored", s.CardStyle)
		s.CardStyle = ""
	}

	for name, tok := range map[string]struct {
		v   *int
		min int
		max int
	}{
		"radius":   {&s.Radius, 0, 48},
		"fontSize": {&s.FontSize, 11, 24},
		"width":    {&s.Width, 480, 3840},
		"iconSize": {&s.IconSize, 12, 64},
	} {
		if *tok.v == 0 {
			continue
		}
		if *tok.v < tok.min || *tok.v > tok.max {
			warn("style.%s: %d is outside %d..%d, ignored", name, *tok.v, tok.min, tok.max)
			*tok.v = 0
		}
	}

	s.CustomCSS = strings.TrimSpace(s.CustomCSS)
	if s.CustomCSS != "" && !safeRepoPath(s.CustomCSS) {
		warn("style.customCss: %q is not a safe repository path, ignored", s.CustomCSS)
		s.CustomCSS = ""
	}
}

// dangerousSchemes are the url schemes that can execute or embed content when
// they end up in an href. Everything else is allowed, because a navigation
// page legitimately links to all sorts of things: ssh://, vscode://, smb://,
// rdp://, and whatever internal protocol a team happens to use.
//
// Blocking these four is what keeps a careless or hostile nav.yaml from
// becoming stored XSS in every visitor's browser, without narrowing the list
// of useful links.
var dangerousSchemes = map[string]bool{
	"javascript": true,
	"data":       true,
	"vbscript":   true,
	"blob":       true,
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a valid url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch {
	case scheme == "":
		// A relative url in a navigation page is almost always a mistake: the
		// link would resolve against ghome itself.
		return errors.New("missing scheme (expected something like https://)")
	case dangerousSchemes[scheme]:
		return fmt.Errorf("scheme %q is not allowed", u.Scheme)
	case (scheme == "http" || scheme == "https") && u.Host == "":
		return errors.New("missing host")
	}
	return nil
}

// usableIcon reports whether an icon value is something the browser can render:
// an absolute image url, or a short glyph such as an emoji.
func usableIcon(v string) bool {
	if isImageURL(v) {
		return true
	}
	return len([]rune(v)) <= 4
}

func isImageURL(v string) bool {
	lower := strings.ToLower(v)
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "data:image/") ||
		strings.HasPrefix(lower, "/")
}

// CSSColor accepts only colour syntax that cannot break out of the CSS
// declaration it is placed in. Rejecting parentheses-bearing values (other
// than the numeric colour functions) is what stops a url() from being smuggled
// in and used to exfiltrate anything.
func CSSColor(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}

	if strings.HasPrefix(v, "#") {
		hex := v[1:]
		switch len(hex) {
		case 3, 4, 6, 8:
		default:
			return false
		}
		for _, r := range hex {
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
		return true
	}

	if i := strings.IndexByte(v, '('); i > 0 {
		if !strings.HasSuffix(v, ")") {
			return false
		}
		switch strings.ToLower(strings.TrimSpace(v[:i])) {
		case "rgb", "rgba", "hsl", "hsla":
		default:
			return false
		}
		args := v[i+1 : len(v)-1]
		if args == "" {
			return false
		}
		for _, r := range args {
			if !strings.ContainsRune("0123456789.,% \t/+-", r) {
				return false
			}
		}
		return true
	}

	// A bare keyword. It must be one CSS actually defines: a typo such as
	// "bleu" is syntactically fine, gets emitted verbatim, and is then dropped
	// by the browser, so the page silently keeps its default and nothing says
	// why.
	return cssColorKeywords[strings.ToLower(v)]
}

// cssColorKeywords is every colour word CSS defines, plus the global keywords
// that are valid in any property. Kept as one string so the list is easy to
// read and impossible to miss an entry in.
var cssColorKeywords = func() map[string]bool {
	const names = "aliceblue antiquewhite aqua aquamarine azure beige bisque black blanchedalmond blue " +
		"blueviolet brown burlywood cadetblue chartreuse chocolate coral cornflowerblue cornsilk crimson " +
		"cyan darkblue darkcyan darkgoldenrod darkgray darkgreen darkgrey darkkhaki darkmagenta " +
		"darkolivegreen darkorange darkorchid darkred darksalmon darkseagreen darkslateblue darkslategray " +
		"darkslategrey darkturquoise darkviolet deeppink deepskyblue dimgray dimgrey dodgerblue firebrick " +
		"floralwhite forestgreen fuchsia gainsboro ghostwhite gold goldenrod gray green greenyellow grey " +
		"honeydew hotpink indianred indigo ivory khaki lavender lavenderblush lawngreen lemonchiffon " +
		"lightblue lightcoral lightcyan lightgoldenrodyellow lightgray lightgreen lightgrey lightpink " +
		"lightsalmon lightseagreen lightskyblue lightslategray lightslategrey lightsteelblue lightyellow " +
		"lime limegreen linen magenta maroon mediumaquamarine mediumblue mediumorchid mediumpurple " +
		"mediumseagreen mediumslateblue mediumspringgreen mediumturquoise mediumvioletred midnightblue " +
		"mintcream mistyrose moccasin navajowhite navy oldlace olive olivedrab orange orangered orchid " +
		"palegoldenrod palegreen paleturquoise palevioletred papayawhip peachpuff peru pink plum " +
		"powderblue purple rebeccapurple red rosybrown royalblue saddlebrown salmon sandybrown seagreen " +
		"seashell sienna silver skyblue slateblue slategray slategrey snow springgreen steelblue tan teal " +
		"thistle tomato turquoise violet wheat white whitesmoke yellow yellowgreen"
	const globals = "transparent currentcolor inherit initial unset revert revert-layer"
	m := make(map[string]bool, 160)
	for _, n := range strings.Fields(names + " " + globals) {
		m[n] = true
	}
	return m
}()

// CSSFontFamily allows a font stack but nothing that could close the property
// and start a new one.
func CSSFontFamily(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	for _, r := range v {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsSpace(r):
		case r == ',', r == '-', r == '_', r == '\'', r == '"':
		default:
			return false
		}
	}
	return true
}

// safeRepoPath keeps style.customCss from pointing outside the repository.
func safeRepoPath(v string) bool {
	if !strings.HasSuffix(strings.ToLower(v), ".css") {
		return false
	}
	if strings.HasPrefix(v, "/") || strings.Contains(v, "..") || strings.Contains(v, "\\") {
		return false
	}
	// The same segment rules the sources apply, so a path that passes here can
	// actually be fetched. Accepting "a//b.css" or "./x.css" only produced a
	// config that validated and then never loaded its stylesheet.
	for _, part := range strings.Split(v, "/") {
		if part == "" || part == "." {
			return false
		}
	}
	for _, r := range v {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsSpace(r):
		case r == '/', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func boolPtr(b bool) *bool { return &b }
