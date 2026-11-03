// Package audit records what a polite automated client sees on each payer host.
//
// For every origin it records the robots.txt response and rules, then probes each URL of
// interest once (only when robots.txt allows it) and looks for bot walls (Cloudflare, Akamai,
// CAPTCHA pages). Results feed the dashboard and the robots/accessibility assessment.
package audit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/TheChrisOneil/tic-crawler/internal/fetch"
	"github.com/TheChrisOneil/tic-crawler/internal/robots"
)

const (
	RobotsTextCap  = 8000
	BodySniffBytes = 4096
)

var OtherAgents = []string{"*", "Googlebot", "GPTBot", "CCBot"}

type RobotsResult struct {
	Origin string `json:"origin"`
	Status *int   `json:"status"`
	Error  string `json:"error"`
	Server string `json:"server"`
	Policy string `json:"policy"` // allow_all (4xx) | disallow_all (5xx/unreachable) | rules
	Text   string `json:"text"`
	Wall   string `json:"wall"`
}

type UrlProbe struct {
	PayerID        string          `json:"payer_id"`
	Role           string          `json:"role"` // landing | toc | mrf_sample
	URL            string          `json:"url"`
	RobotsAllowsUs *bool           `json:"robots_allows_us"`
	RobotsAllows   map[string]bool `json:"robots_allows"`
	Fetched        bool            `json:"fetched"`
	Status         *int            `json:"status"`
	Server         string          `json:"server"`
	ContentType    string          `json:"content_type"`
	Wall           string          `json:"wall"`
	Error          string          `json:"error"`
	Verdict        string          `json:"verdict"` // accessible | robots_blocked | bot_wall | http_error | unreachable
}

type Target struct{ PayerID, Role, URL string }

type Report struct {
	Robots []RobotsResult `json:"robots"`
	Probes []UrlProbe     `json:"probes"`
}

func errText(err error) string {
	if ue, ok := err.(*url.Error); ok {
		err = ue.Err
	}
	msg := fmt.Sprintf("%T: %v", err, err)
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

func sniff(ctx context.Context, c *fetch.Client, u string, headers map[string]string) (*http.Response, []byte, error) {
	resp, err := c.Raw(ctx, "GET", u, headers)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, BodySniffBytes))
	return resp, body, nil
}

func originOf(raw string) (string, string) {
	p, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	return p.Scheme + "://" + p.Host, strings.ToLower(p.Host)
}

func intp(n int) *int { return &n }

// Run fetches each origin's robots.txt once, then probes every target it allows.
func Run(ctx context.Context, c *fetch.Client, targets []Target) Report {
	var kept []Target
	for _, t := range targets {
		l := strings.ToLower(t.URL)
		if strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
			kept = append(kept, t)
		}
	}
	rep := Report{Robots: []RobotsResult{}, Probes: []UrlProbe{}}
	parsers := map[string]*robots.Rules{} // nil value: disallow all
	seen := map[string]bool{}
	for _, t := range kept {
		origin, host := originOf(t.URL)
		if seen[origin] {
			continue
		}
		seen[origin] = true
		rr := RobotsResult{Origin: origin}
		c.Limiter.Wait(host)
		resp, body, err := sniff(ctx, c, origin+"/robots.txt", nil)
		switch {
		case err != nil:
			rr.Error, rr.Policy = errText(err), "disallow_all"
			parsers[origin] = nil
		default:
			rr.Status, rr.Server = intp(resp.StatusCode), resp.Header.Get("Server")
			rr.Wall = fetch.DetectWall(resp, body)
			switch {
			case resp.StatusCode < 400 && rr.Wall == "":
				full, ferr := c.Raw(ctx, "GET", origin+"/robots.txt", nil)
				if ferr != nil {
					rr.Error, rr.Policy = errText(ferr), "disallow_all"
					parsers[origin] = nil
					break
				}
				b, _ := io.ReadAll(io.LimitReader(full.Body, 1<<20))
				full.Body.Close()
				text := string(b)
				rr.Text = text
				if len(rr.Text) > RobotsTextCap {
					rr.Text = rr.Text[:RobotsTextCap]
				}
				parsers[origin], rr.Policy = robots.Parse(text), "rules"
			case resp.StatusCode >= 500:
				parsers[origin], rr.Policy = nil, "disallow_all"
			default:
				parsers[origin], rr.Policy = &robots.Rules{AllowAll: true}, "allow_all"
			}
		}
		rep.Robots = append(rep.Robots, rr)
	}

	ua := c.S.UserAgent()
	for _, t := range kept {
		origin, host := originOf(t.URL)
		rp := parsers[origin]
		pr := UrlProbe{PayerID: t.PayerID, Role: t.Role, URL: t.URL, RobotsAllows: map[string]bool{}}
		allowed := false
		if rp != nil {
			allowed = rp.CanFetch(ua, t.URL)
			for _, a := range OtherAgents {
				pr.RobotsAllows[a] = rp.CanFetch(a, t.URL)
			}
		}
		pr.RobotsAllowsUs = &allowed
		if !allowed {
			pr.Verdict = "robots_blocked"
			rep.Probes = append(rep.Probes, pr)
			continue
		}
		c.Limiter.Wait(host)
		var headers map[string]string
		if t.Role != "landing" {
			headers = map[string]string{"Range": "bytes=0-4095"}
		}
		resp, body, err := sniff(ctx, c, t.URL, headers)
		if err != nil {
			pr.Error, pr.Verdict = errText(err), "unreachable"
			rep.Probes = append(rep.Probes, pr)
			continue
		}
		pr.Fetched, pr.Status = true, intp(resp.StatusCode)
		pr.Server = resp.Header.Get("Server")
		pr.ContentType = resp.Header.Get("Content-Type")
		if len(pr.ContentType) > 80 {
			pr.ContentType = pr.ContentType[:80]
		}
		pr.Wall = fetch.DetectWall(resp, body)
		switch {
		case pr.Wall != "":
			pr.Verdict = "bot_wall"
		case resp.StatusCode < 400:
			pr.Verdict = "accessible"
		default:
			pr.Verdict = "http_error"
		}
		rep.Probes = append(rep.Probes, pr)
	}
	return rep
}
