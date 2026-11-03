package audit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/TheChrisOneil/tic-crawler/internal/config"
	"github.com/TheChrisOneil/tic-crawler/internal/fetch"
)

// hosts serves each request from the handler for its host, without a network.
type hosts map[string]http.HandlerFunc

func (h hosts) RoundTrip(r *http.Request) (*http.Response, error) {
	fn, ok := h[r.URL.Host]
	if !ok {
		return nil, errors.New("connect refused")
	}
	rec := httptest.NewRecorder()
	fn(rec, r)
	return rec.Result(), nil
}

func site(robots func(w http.ResponseWriter), page func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			robots(w)
			return
		}
		if page == nil {
			w.WriteHeader(404)
			return
		}
		page(w, r)
	}
}

func TestRunVerdicts(t *testing.T) {
	s := config.Defaults()
	s.RequestsPerSecondHost = 0
	c := fetch.New(s)
	var gotRange string
	c.HTTP.Transport = hosts{
		"ok.com": site(func(w http.ResponseWriter) {
			io.WriteString(w, "User-agent: *\nDisallow: /private\nUser-agent: GPTBot\nDisallow: /\n")
		}, func(w http.ResponseWriter, r *http.Request) {
			gotRange = r.Header.Get("Range")
			w.WriteHeader(206)
			io.WriteString(w, "{")
		}),
		"cf.com": site(func(w http.ResponseWriter) {}, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Server", "cloudflare")
			w.WriteHeader(403)
			io.WriteString(w, "<title>Attention Required! | Cloudflare</title>")
		}),
		"down.com": site(func(w http.ResponseWriter) { w.WriteHeader(503) }, nil),
		"nf.com":   site(func(w http.ResponseWriter) { w.WriteHeader(404) }, nil),
	}
	targets := []Target{{"a", "toc", "https://ok.com/toc.json"}, {"a", "landing", "https://ok.com/private/p"},
		{"b", "toc", "https://cf.com/toc"}, {"c", "toc", "https://down.com/t"},
		{"d", "toc", "https://nf.com/x"}, {"e", "toc", "https://err.com/x"}, {"f", "toc", "Missing file"}}
	rep := Run(context.Background(), c, targets)
	if len(rep.Probes) != 6 {
		t.Fatalf("probes %d", len(rep.Probes))
	}
	v := map[string]UrlProbe{}
	for _, p := range rep.Probes {
		v[p.PayerID+"/"+p.Role] = p
	}
	want := map[string]string{"a/toc": "accessible", "a/landing": "robots_blocked", "b/toc": "bot_wall",
		"c/toc": "robots_blocked", "d/toc": "http_error", "e/toc": "robots_blocked"}
	for k, w := range want {
		if v[k].Verdict != w {
			t.Errorf("%s verdict %q, want %q", k, v[k].Verdict, w)
		}
	}
	if gotRange != "bytes=0-4095" {
		t.Errorf("range %q", gotRange)
	}
	if !reflect.DeepEqual(v["a/toc"].RobotsAllows, map[string]bool{"*": true, "Googlebot": true, "GPTBot": false, "CCBot": true}) {
		t.Errorf("robots_allows %v", v["a/toc"].RobotsAllows)
	}
	if v["a/landing"].Fetched || v["b/toc"].Wall != "cloudflare_challenge" {
		t.Errorf("landing fetched=%v wall=%q", v["a/landing"].Fetched, v["b/toc"].Wall)
	}
	pol := map[string]string{}
	for _, r := range rep.Robots {
		pol[r.Origin] = r.Policy
	}
	wantPol := map[string]string{"https://ok.com": "rules", "https://cf.com": "rules",
		"https://down.com": "disallow_all", "https://nf.com": "allow_all", "https://err.com": "disallow_all"}
	if !reflect.DeepEqual(pol, wantPol) {
		t.Errorf("policies %v", pol)
	}
	if !strings.Contains(rep.Robots[0].Text, "GPTBot") {
		t.Errorf("robots text %q", rep.Robots[0].Text)
	}
}
