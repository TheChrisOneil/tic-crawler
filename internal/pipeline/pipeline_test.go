package pipeline

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TheChrisOneil/tic-crawler/internal/config"
	"github.com/TheChrisOneil/tic-crawler/internal/fetch"
	"github.com/TheChrisOneil/tic-crawler/internal/model"
	"github.com/TheChrisOneil/tic-crawler/internal/state"
)

var today = time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)

type site struct {
	srv      *httptest.Server
	toc      []byte
	tocCode  int
	robots   string
	robots5x bool
	heads    atomic.Int32
	landing  string
	cfBlock  bool
}

func newSite(t *testing.T) *site {
	s := &site{tocCode: 200}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/robots.txt":
			if s.robots5x {
				w.WriteHeader(503)
			} else if s.robots != "" {
				io.WriteString(w, s.robots)
			} else {
				w.WriteHeader(404)
			}
		case strings.HasPrefix(r.URL.Path, "/files/"):
			s.heads.Add(1)
			w.Header().Set("Content-Length", "1000")
			w.Header().Set("ETag", `"e"`)
			if r.Method == "GET" {
				w.Write(bytes.Repeat([]byte("x"), 1000))
			}
		case r.URL.Path == "/landing":
			io.WriteString(w, s.landing)
		case strings.HasSuffix(r.URL.Path, "_index.json") || r.URL.Path == "/toc.json":
			if s.cfBlock {
				w.Header().Set("Server", "cloudflare")
				w.WriteHeader(403)
				return
			}
			if r.URL.Path == "/2026-10-01_index.json" && s.tocCode != 200 {
				w.WriteHeader(s.tocCode)
				return
			}
			if r.URL.Path == "/2026-09-01_index.json" || r.URL.Path == "/2026-10-01_index.json" || r.URL.Path == "/toc.json" {
				w.Write(s.toc)
				return
			}
			w.WriteHeader(404)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.srv.Close)
	sample, err := os.ReadFile("../../tests/fixtures/toc/cms-sample.json")
	if err != nil {
		t.Fatal(err)
	}
	s.toc = bytes.ReplaceAll(sample, []byte("https://www.some_site.com"), []byte(s.srv.URL))
	return s
}

func setup(t *testing.T, s *site) (*TocCrawl, string) {
	cfg := config.Defaults()
	cfg.RequestsPerSecondHost = 0
	cfg.MaxRetries = 1
	c := fetch.New(cfg)
	c.HTTP = s.srv.Client()
	c.Sleep = func(time.Duration) {}
	st, err := state.Open(filepath.Join(t.TempDir(), "s.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	out := filepath.Join(t.TempDir(), "out")
	return &TocCrawl{S: cfg, HTTP: c, State: st, OutDir: out, Today: today}, out
}

func payer(s *site, path string) model.Payer {
	return model.Payer{PayerID: "acme", LegalName: "Acme", TocURL: s.srv.URL + path}
}

func TestCrawlWritesTOCAndManifest(t *testing.T) {
	for _, gz := range []bool{false, true} {
		s := newSite(t)
		if gz {
			var b bytes.Buffer
			w := gzip.NewWriter(&b)
			w.Write(s.toc)
			w.Close()
			s.toc = b.Bytes()
		}
		p, out := setup(t, s)
		r := Run(context.Background(), p, payer(s, "/toc.json"), today)
		if r.Status != "ok" || r.SchemaValid == nil || !*r.SchemaValid || r.AccessStatus != "ok" || r.MrfCount == 0 {
			t.Fatalf("%+v", r)
		}
		if r.TotalBytes != int64(1000*r.MrfCount) || len(r.Outputs) != 2 {
			t.Fatalf("%+v", r)
		}
		f, _ := os.Open(filepath.Join(out, r.Outputs[1]))
		rows, _ := csv.NewReader(f).ReadAll()
		f.Close()
		if len(rows) != r.MrfCount+1 || rows[1][6] != "True" || rows[1][17] != "200" || rows[1][19] != "1000" {
			t.Fatalf("%v", rows[:2])
		}
		gzf, _ := os.Open(filepath.Join(out, r.Outputs[0]))
		zr, err := gzip.NewReader(gzf)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, zr)
		gzf.Close()
		// A second run with the same TOC is unchanged.
		p.State.RecordResult("run1", r)
		heads := s.heads.Load()
		r2 := Run(context.Background(), p, payer(s, "/toc.json"), today)
		if r2.Status != "unchanged" || s.heads.Load() != heads {
			t.Fatalf("%+v", r2)
		}
	}
}

func TestPatternFallsBackToPreviousMonth(t *testing.T) {
	s := newSite(t)
	s.tocCode = 404
	p, _ := setup(t, s)
	r := Run(context.Background(), p, payer(s, "/{date:%Y-%m}-01_index.json"), today)
	if r.Status != "ok" || !strings.HasSuffix(r.TocURL, "2026-09-01_index.json") {
		t.Fatalf("%+v", r)
	}
}

func TestAccessOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		mod    func(*site)
		status string
		access string
	}{
		{"http 404 fails", func(s *site) { s.tocCode = 404 }, "failed", "http_error"},
		{"5xx deferred", func(s *site) { s.tocCode = 503 }, "deferred", "http_error"},
		{"robots disallow fails", func(s *site) { s.robots = "User-agent: *\nDisallow: /\n" }, "failed", "robots_disallow"},
		{"robots unreachable deferred", func(s *site) { s.robots5x = true }, "deferred", "robots_unreachable"},
		{"cloudflare is waf", func(s *site) { s.cfBlock = true }, "failed", "waf_blocked"},
	}
	for _, c := range cases {
		s := newSite(t)
		c.mod(s)
		p, _ := setup(t, s)
		r := Run(context.Background(), p, payer(s, "/2026-10-01_index.json"), today)
		if r.Status != c.status || r.AccessStatus != c.access {
			t.Errorf("%s: %+v", c.name, r)
		}
	}
}

func TestInvalidAndMalformedTOCs(t *testing.T) {
	s := newSite(t)
	s.toc = []byte(`{"reporting_entity_name":"X","reporting_entity_type":"issuer","last_updated_on":"2026-10-01","version":"2.0.0",
	  "reporting_structure":[{"reporting_plans":[],"in_network_files":[{"description":"d","location":"Missing file"}]}]}`)
	p, _ := setup(t, s)
	r := Run(context.Background(), p, payer(s, "/toc.json"), today)
	if r.Status != "ok" || *r.SchemaValid || r.MrfCount != 1 || s.heads.Load() != 0 {
		t.Fatalf("%+v", r)
	}
	s.toc = []byte(`{"reporting_structure": [`)
	r = Run(context.Background(), p, payer(s, "/toc.json"), today)
	if r.Status != "ok" || *r.SchemaValid || !strings.Contains(string(r.Validation), "JSON parse error") {
		t.Fatalf("%+v %s", r, r.Validation)
	}
}

func TestSkipsAndManual(t *testing.T) {
	s := newSite(t)
	p, _ := setup(t, s)
	r := Run(context.Background(), p, model.Payer{PayerID: "x", LegalName: "X"}, today)
	if r.Status != "skipped" || r.AccessStatus != "no_toc_url" {
		t.Fatalf("%+v", r)
	}
	p.S.MaxTOCBytes = 10
	r = Run(context.Background(), p, payer(s, "/toc.json"), today)
	if r.Status != "skipped" || r.AccessStatus != "too_large" {
		t.Fatalf("%+v", r)
	}
	p.S.MaxTOCBytes = 1 << 30
	manual := filepath.Join(t.TempDir(), "acme.json")
	os.WriteFile(manual, s.toc, 0o644)
	r = Run(context.Background(), p, model.Payer{PayerID: "m", LegalName: "M", TocFile: manual}, today)
	if r.Status != "ok" || r.AccessStatus != "manual" || !strings.HasPrefix(r.TocURL, "manual:") {
		t.Fatalf("%+v", r)
	}
}

func TestScrapeLandingPage(t *testing.T) {
	s := newSite(t)
	s.landing = fmt.Sprintf(`<a href="/2026-09-01_index.json">old</a><a href="%s/2026-10-01_index.json">new</a>`, s.srv.URL)
	p, _ := setup(t, s)
	pay := model.Payer{PayerID: "a", LegalName: "A", LandingURL: s.srv.URL + "/landing", TocLinkPattern: `[^"]*\d{4}-\d{2}-\d{2}_index\.json`}
	r := Run(context.Background(), p, pay, today)
	if r.Status != "ok" || !strings.HasSuffix(r.TocURL, "2026-10-01_index.json") {
		t.Fatalf("%+v", r)
	}
}

type memRaw struct{ files map[string]*bytes.Buffer }

type nopCloser struct{ *bytes.Buffer }

func (nopCloser) Close() error { return nil }

func (m *memRaw) OpenWriter(rel string) (io.WriteCloser, error) {
	b := &bytes.Buffer{}
	m.files[rel] = b
	return nopCloser{b}, nil
}

func TestRawDownloadAndCap(t *testing.T) {
	s := newSite(t)
	p, _ := setup(t, s)
	raw := &memRaw{files: map[string]*bytes.Buffer{}}
	rd := &RawDownload{TocCrawl: p, Raw: raw, MaxRunBytes: 1 << 20}
	r := Run(context.Background(), rd, payer(s, "/toc.json"), today)
	if r.Status != "ok" || r.RawBytes != int64(1000*r.MrfCount) || len(raw.files) != r.MrfCount {
		t.Fatalf("%+v", r)
	}
	for k := range raw.files {
		if !strings.HasPrefix(k, "raw/acme/2026-10-29/") {
			t.Fatal(k)
		}
	}
	p.Force = true
	small := &RawDownload{TocCrawl: p, Raw: raw, MaxRunBytes: 10}
	r = Run(context.Background(), small, payer(s, "/toc.json"), today)
	if r.Status != "skipped" || !strings.HasPrefix(r.Error, "raw_download_over_cap") {
		t.Fatalf("%+v", r)
	}
}

func TestProbeSkipsChecks(t *testing.T) {
	s := newSite(t)
	p, _ := setup(t, s)
	r := Run(context.Background(), &Probe{p}, payer(s, "/toc.json"), today)
	if r.Status != "ok" || s.heads.Load() != 0 {
		t.Fatalf("%+v", r)
	}
}
