package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/TheChrisOneil/tic-crawler/internal/config"
	"github.com/TheChrisOneil/tic-crawler/internal/fetch"
	"github.com/TheChrisOneil/tic-crawler/internal/model"
	"github.com/TheChrisOneil/tic-crawler/internal/state"
)

const seed = `
payers:
  - payer_id: acme
    legal_name: Acme Health Inc.
    hios_ids: [11111]
    landing_url: https://mrf.example.com/
    toc_url: https://mrf.example.com/index.json
  - payer_id: nourl
    legal_name: No URL Health
`

// fakeWeb answers like the Python CLI tests' mock site, without a network.
type fakeWeb struct{ toc []byte }

func (f fakeWeb) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	switch {
	case r.URL.Path == "/robots.txt":
		rec.WriteHeader(404)
	case r.URL.Host == "mrf.example.com" && r.URL.Path == "/index.json":
		rec.Write(f.toc)
	case r.URL.Host == "mrf.example.com":
		rec.WriteString("<html>")
	case r.URL.Host == "www.some_site.com":
		rec.Header().Set("Content-Length", "10")
		if r.Method == "GET" {
			rec.WriteHeader(206)
			rec.WriteString("{")
		}
	default:
		return nil, errors.New("connect refused")
	}
	return rec.Result(), nil
}

func cliEnv(t *testing.T) string {
	t.Helper()
	toc, err := os.ReadFile("../../testdata/toc/cms-sample.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	os.WriteFile(cfg, []byte(`
[http]
requests_per_second_per_host = 0
backoff_base_seconds = 0
backoff_max_seconds = 0
max_retries = 1
[discovery]
seed_file = "`+filepath.Join(dir, "payers.yaml")+`"
exchange_puf_url = ""
[output]
local_dir = "`+filepath.Join(dir, "out")+`"
state_db = "`+filepath.Join(dir, "state.sqlite")+`"
`), 0o644)
	os.WriteFile(filepath.Join(dir, "payers.yaml"), []byte(seed), 0o644)
	cfgPath = cfg
	t.Setenv("TIC_CONTACT_EMAIL", "t@example.com")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	newClient = func(s config.Settings) *fetch.Client {
		c := fetch.New(s)
		c.HTTP.Transport = fakeWeb{toc}
		return c
	}
	t.Cleanup(func() { newClient = fetch.New; cfgPath = "" })
	return dir
}

func run(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func mustRun(t *testing.T, cmd *cobra.Command, args ...string) string {
	t.Helper()
	out, err := run(t, cmd, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return out
}

func TestDiscoverCrawlReport(t *testing.T) {
	dir := cliEnv(t)
	out := mustRun(t, discoverCmd())
	if !strings.Contains(out, "payers: 2 (with TOC URL: 1)") || !strings.Contains(out, "root_txt: inactive") {
		t.Fatal(out)
	}
	out = mustRun(t, crawlCmd(), "--all")
	if !strings.Contains(out, "1 crawled now, 1 ok") {
		t.Fatal(out)
	}
	logs, _ := filepath.Glob(filepath.Join(dir, "out", "runs", "*.json"))
	if len(logs) != 1 {
		t.Fatal(logs)
	}
	var log struct{ Summary map[string]any }
	b, _ := os.ReadFile(logs[0])
	json.Unmarshal(b, &log)
	if log.Summary["mrf_urls"] != 5.0 || log.Summary["mrf_bytes"] != 50.0 {
		t.Fatal(log.Summary)
	}
	reg, _ := os.ReadFile(filepath.Join(dir, "out", "registry.csv"))
	lines := strings.Split(strings.TrimSpace(string(reg)), "\r\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "payer_id,legal_name,hios_ids") ||
		!strings.HasPrefix(lines[1], "acme,Acme Health Inc.,11111") {
		t.Fatalf("%q", reg)
	}

	// Within the recrawl window --all skips; --payer crawls and finds it unchanged.
	if out = mustRun(t, crawlCmd(), "--all"); !strings.Contains(out, "0 crawled now") {
		t.Fatal(out)
	}
	if out = mustRun(t, crawlCmd(), "--payer", "acme"); !strings.Contains(out, "unchanged") {
		t.Fatal(out)
	}
	if out = mustRun(t, reportCmd()); !strings.Contains(out, "payers 2, crawled 1, failed 0") {
		t.Fatal(out)
	}
	var rows []map[string]any
	json.Unmarshal([]byte(mustRun(t, reportCmd(), "--json")), &rows)
	if len(rows) != 2 || rows[0]["mrf_count"] != 5.0 || rows[0]["stale"] != true || rows[1]["status"] != "never" {
		t.Fatal(rows)
	}

	out = mustRun(t, auditAccessCmd(), "--mrf-samples", "1")
	if !strings.Contains(out, "map[accessible:3]") {
		t.Fatal(out)
	}
	mustRun(t, exportDashboardCmd())
	var data struct{ Totals map[string]float64 }
	b, _ = os.ReadFile(filepath.Join(dir, "out", "dashboard", "data.json"))
	json.Unmarshal(b, &data)
	if data.Totals["payers"] != 2 || data.Totals["ok"] != 1 || data.Totals["mrf_bytes"] != 50 {
		t.Fatal(data.Totals)
	}
}

func TestCrawlArgs(t *testing.T) {
	cliEnv(t)
	if _, err := run(t, crawlCmd()); err == nil {
		t.Fatal("want error without a target")
	}
	if _, err := run(t, crawlCmd(), "--payer", "zzz"); err == nil || !strings.Contains(err.Error(), "zzz") {
		t.Fatal(err)
	}
	if _, err := run(t, crawlCmd(), "--all", "--toc-file", "x.json"); err == nil {
		t.Fatal("want --toc-file error")
	}
	if _, err := run(t, syncDriveCmd()); err == nil || !strings.Contains(err.Error(), "GOOGLE_APPLICATION_CREDENTIALS") {
		t.Fatal(err)
	}
}

func TestCrawlResumesUnfinishedRun(t *testing.T) {
	dir := cliEnv(t)
	st, err := state.Open(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	runID, _, _ := st.OpenRun(map[string]any{"payers": []string{"acme"}, "raw": false, "force": false})
	r := model.NewPayerResult("acme")
	r.Status, r.CrawledAt = "ok", "2026-01-01T00:00:00Z"
	st.RecordResult(runID, r)
	st.Close()
	out := mustRun(t, crawlCmd(), "--payer", "acme")
	if !strings.Contains(out, "resuming run "+runID+": 1 payers already done") || !strings.Contains(out, "0 crawled now") {
		t.Fatal(out)
	}
}

type fakeUploader map[string]string

func (f fakeUploader) PutFile(_ context.Context, rel, _ string) (string, error) {
	f[rel] = "remote:" + rel
	return f[rel], nil
}

func TestSyncOutputsUploadsOnce(t *testing.T) {
	dir := cliEnv(t)
	mustRun(t, crawlCmd(), "--all")
	s, _ := settings()
	st, _ := state.Open(filepath.Join(dir, "state.sqlite"))
	defer st.Close()
	up := fakeUploader{}
	counts, err := syncOutputs(context.Background(), s, st, up)
	if err != nil || counts["uploaded"] != 3 || counts["skipped"] != 0 {
		t.Fatal(counts, err, up)
	}
	counts, _ = syncOutputs(context.Background(), s, st, fakeUploader{})
	if counts["uploaded"] != 0 || counts["skipped"] != 3 {
		t.Fatal(counts)
	}
}

func TestProbeMarkdown(t *testing.T) {
	cliEnv(t)
	md := filepath.Join(t.TempDir(), "probe.md")
	out := mustRun(t, probeCmd(), "--name", "Acme Health", "--toc-url", "https://mrf.example.com/index.json",
		"--landing-url", "https://mrf.example.com/", "--markdown", md)
	for _, want := range []string{"## Probe: Acme Health", "| toc | https://mrf.example.com/index.json | True | 200 | accessible |",
		"**TOC:** ok;", "payer_id: acme-health", "verified_by: http"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if b, _ := os.ReadFile(md); string(b)+"\n" != out {
		t.Fatal("markdown file differs from output")
	}
}
