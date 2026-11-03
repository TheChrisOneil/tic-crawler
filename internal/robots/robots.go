// Package robots parses robots.txt and matches URLs per RFC 9309: the longest matching rule
// wins, allow wins ties, `*` and `$` are supported, and groups for the same agent are combined.
package robots

import (
	"net/url"
	"regexp"
	"strings"
)

type rule struct {
	allow   bool
	pattern string
	rx      *regexp.Regexp
}

type group struct {
	agents []string
	rules  []rule
}

// Rules is a parsed robots.txt. AllowAll is set when robots.txt returned 4xx.
type Rules struct {
	groups   []*group
	AllowAll bool
}

func normalize(p string) string {
	// Compare decoded paths so %2F-style escapes and literal characters match the same way.
	if u, err := url.PathUnescape(p); err == nil {
		return u
	}
	return p
}

func compile(pattern string) *regexp.Regexp {
	anchored := strings.HasSuffix(pattern, "$")
	body := strings.TrimSuffix(pattern, "$")
	var b strings.Builder
	b.WriteString("^")
	for _, ch := range normalize(body) {
		if ch == '*' {
			b.WriteString(".*")
		} else {
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	if anchored {
		b.WriteString("$")
	}
	return regexp.MustCompile(b.String())
}

var tokenRx = regexp.MustCompile(`^[A-Za-z_-]+`)

// ProductToken turns "EverBetter-TiC-Crawler/1.0 (+x)" into "everbetter-tic-crawler".
func ProductToken(userAgent string) string {
	ua := strings.TrimSpace(userAgent)
	if m := tokenRx.FindString(ua); m != "" {
		return strings.ToLower(m)
	}
	return strings.ToLower(ua)
}

// Parse reads robots.txt text.
func Parse(text string) *Rules {
	r := &Rules{}
	var current *group
	lastWasAgent := false
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(strings.SplitN(raw, "#", 2)[0])
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		switch key {
		case "user-agent":
			if current == nil || !lastWasAgent {
				current = &group{}
				r.groups = append(r.groups, current)
			}
			current.agents = append(current.agents, strings.ToLower(value))
			lastWasAgent = true
		case "allow", "disallow":
			lastWasAgent = false
			if current == nil || value == "" {
				continue // an empty Disallow allows everything; rules outside a group are ignored
			}
			current.rules = append(current.rules, rule{key == "allow", value, compile(value)})
		default:
			lastWasAgent = false
		}
	}
	return r
}

func (r *Rules) rulesFor(userAgent string) []rule {
	token := ProductToken(userAgent)
	var out []rule
	found := false
	for _, want := range []string{token, "*"} {
		for _, g := range r.groups {
			for _, a := range g.agents {
				if a == want {
					out = append(out, g.rules...)
					found = true
					break
				}
			}
		}
		if found {
			return out
		}
	}
	return nil
}

// CanFetch reports whether userAgent may fetch rawURL.
func (r *Rules) CanFetch(userAgent, rawURL string) bool {
	if r.AllowAll {
		return true
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	p = normalize(p)
	if p == "/robots.txt" {
		return true
	}
	bestLen, bestAllow, matched := -1, true, false
	for _, ru := range r.rulesFor(userAgent) {
		if !ru.rx.MatchString(p) {
			continue
		}
		n := len(ru.pattern)
		if !matched || n > bestLen || (n == bestLen && ru.allow) {
			bestLen, bestAllow, matched = n, ru.allow, true
		}
	}
	return bestAllow
}
