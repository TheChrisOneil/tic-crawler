package main

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readGzCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(zr).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestExtractRatesFromTOC(t *testing.T) {
	inNet, err := os.ReadFile("../../internal/extract/testdata/in-network.json")
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(404)
		case "/toc.json":
			fmt.Fprintf(w, `{"reporting_entity_name":"X","reporting_entity_type":"issuer","version":"2.0.0",
			  "reporting_structure":[{"reporting_plans":[{"plan_name":"P","plan_id_type":"EIN","plan_id":"1","plan_market_type":"group"}],
			  "in_network_files":[{"description":"d","location":"%s/in.json.gz"}]},
			  {"reporting_plans":[{"plan_name":"Q HMO","plan_id_type":"EIN","plan_id":"2","plan_market_type":"group"}],
			  "in_network_files":[{"description":"other","location":"%s/other.json"}]}]}`, srv.URL, srv.URL)
		case "/in.json.gz":
			if gz.Len() == 0 {
				zw := gzip.NewWriter(&gz)
				zw.Write(bytes.ReplaceAll(inNet, []byte("https://example.test"), []byte(srv.URL)))
				zw.Close()
			}
			w.Header().Set("Content-Length", fmt.Sprint(gz.Len()))
			w.Write(gz.Bytes())
		case "/ref3.json":
			w.Write([]byte(`{"provider_groups":[]}`))
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.toml")
	os.WriteFile(cfg, []byte("[http]\nrequests_per_second_per_host = 0\nmax_retries = 0\n"), 0o644)
	codes := filepath.Join(dir, "codes.yaml")
	os.WriteFile(codes, []byte("codes:\n  - {type: CPT, code: \"99213\"}\n"), 0o644)
	cfgPath = cfg
	cmd := extractRatesCmd()
	cmd.SetArgs([]string{"--payer", "p", "--toc-url", srv.URL + "/toc.json", "--codes", codes, "--out", dir})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err, out.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "p", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Files []fileResult `json:"files"`
	}
	json.Unmarshal(b, &idx)
	if len(idx.Files) != 2 || idx.Files[1].Status != "not_tried" || idx.Files[0].Status != "ok" || idx.Files[0].PlanCount != 1 || idx.Files[0].Meta.ItemsKept != 1 {
		t.Fatalf("%s", b)
	}
	if !strings.HasPrefix(out.String(), "ok ") {
		t.Fatal(out.String())
	}
	// Every plan is in the uncapped index, and coverage says which plans have no extracted file.
	pf := readGzCSV(t, filepath.Join(dir, "p", "plan_files.csv.gz"))
	if len(pf) != 3 || pf[2][2] != "Q HMO" || pf[1][7] != idx.Files[0].Dir {
		t.Fatalf("plan_files: %v", pf)
	}
	cov := readGzCSV(t, filepath.Join(dir, "p", "plan_coverage.csv.gz"))
	got := map[string]string{}
	for _, r := range cov[1:] {
		got[r[2]+"/"+r[6]] = r[4] + "," + r[5] + "," + r[7]
	}
	// P: 1 file, extracted; 99213 has usable rates for NPIs 1234567893, 1111111111, 2222222222, 1999999999
	// (the remote group resolves to no providers here). Q: its only file was not extracted.
	if got["P/HCPCS:99213"] != "1,1,4" || got["Q HMO/"] != "1,0," || len(got) != 2 {
		t.Fatalf("plan_coverage: %v", cov)
	}
	// A budget smaller than the file skips it by size.
	cmd = extractRatesCmd()
	cmd.SetArgs([]string{"--payer", "q", "--url", srv.URL + "/in.json.gz", "--codes", codes, "--out", dir, "--max-bytes", "10"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("want no-file error")
	}
}
