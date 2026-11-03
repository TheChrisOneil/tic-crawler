package fetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TheChrisOneil/tic-crawler/internal/config"
)

func client(t *testing.T) *Client {
	s := config.Defaults()
	s.RequestsPerSecondHost = 0
	s.MaxRetries = 2
	s.ContactEmail = "t@example.com"
	c := New(s)
	c.Sleep = func(time.Duration) {}
	return c
}

func TestRobotsDisallowAndUserAgent(t *testing.T) {
	var ua atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.UserAgent())
		if r.URL.Path == "/robots.txt" {
			io.WriteString(w, "User-agent: *\nDisallow: /private\n")
			return
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	c := client(t)
	if _, err := c.GetText(context.Background(), srv.URL+"/private/x", 100); err == nil {
		t.Fatal("want robots error")
	} else {
		var rd *RobotsDisallowed
		if !errors.As(err, &rd) || rd.Unreachable {
			t.Fatal(err)
		}
	}
	txt, err := c.GetText(context.Background(), srv.URL+"/public", 100)
	if err != nil || txt != "ok" {
		t.Fatal(err, txt)
	}
	if ua.Load().(string) != "EverBetter-TiC-Crawler/1.0 (+t@example.com)" {
		t.Fatal(ua.Load())
	}
	if c.RobotsStatus(srv.URL+"/x") != "robots.txt 200" {
		t.Fatal(c.RobotsStatus(srv.URL))
	}
}

func TestRobots404AllowsAnd5xxIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	if _, err := client(t).GetText(context.Background(), srv.URL+"/a", 10); err != nil {
		t.Fatal(err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	_, err := client(t).GetText(context.Background(), bad.URL+"/a", 10)
	var rd *RobotsDisallowed
	if !errors.As(err, &rd) || !rd.Unreachable {
		t.Fatal(err)
	}
}

func TestRetryThenSuccessAndWall(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(404)
		case "/flaky":
			if n.Add(1) < 2 {
				w.WriteHeader(503)
				return
			}
			io.WriteString(w, "ok")
		case "/wall":
			w.Header().Set("cf-mitigated", "challenge")
			w.WriteHeader(403)
			io.WriteString(w, "<title>Just a moment...</title>")
		}
	}))
	defer srv.Close()
	c := client(t)
	if txt, err := c.GetText(context.Background(), srv.URL+"/flaky", 10); err != nil || txt != "ok" {
		t.Fatal(err, txt)
	}
	_, err := c.GetText(context.Background(), srv.URL+"/wall", 10)
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Wall != "cloudflare_challenge" || fe.Status != 403 {
		t.Fatal(err)
	}
}

func TestHeadFallsBackToRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/robots.txt":
			w.WriteHeader(404)
		case r.Method == "HEAD":
			w.WriteHeader(403)
		case r.Header.Get("Range") == "bytes=0-0":
			w.Header().Set("Content-Range", "bytes 0-0/12345")
			w.WriteHeader(206)
			io.WriteString(w, "x")
		}
	}))
	defer srv.Close()
	uc := client(t).HeadOrRange(context.Background(), srv.URL+"/f.json.gz")
	if uc.Method != "RANGE_GET" || uc.ContentLength != 12345 || uc.Status != 206 {
		t.Fatalf("%+v", uc)
	}
}

func TestHostLimiterSpacing(t *testing.T) {
	l := NewHostLimiter(2)
	now := time.Unix(0, 0)
	var slept time.Duration
	l.now = func() time.Time { return now }
	l.sleep = func(d time.Duration) { slept += d }
	l.Wait("a")
	l.Wait("a")
	l.Wait("b")
	if slept != 500*time.Millisecond {
		t.Fatal(slept)
	}
}
