package extract

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readGz(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(gz).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func run(t *testing.T, opt Options) (string, *Meta) {
	t.Helper()
	f, err := os.Open("testdata/in-network.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dir := t.TempDir()
	opt.TempDir = dir
	if opt.FetchRemote == nil {
		opt.FetchRemote = func(u string) (io.ReadCloser, error) {
			if u != "https://example.test/ref3.json" {
				t.Fatalf("unexpected remote %s", u)
			}
			return io.NopCloser(strings.NewReader(`{"provider_groups":[{"npi":[3333333333],"tin":{"type":"ein","value":"33-3333333","business_name":"Remote Group"}}]}`)), nil
		}
	}
	m, err := Run(f, opt, filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "out"), m
}

func codes(keys ...string) map[string]bool {
	m := map[string]bool{}
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func TestFiltersByCodeAndNPI(t *testing.T) {
	out, m := run(t, Options{
		Codes: codes("HCPCS:73721", "HCPCS:99213"),
		NPIs:  map[int64]bool{1234567893: true, 3333333333: true},
	})
	rates := readGz(t, filepath.Join(out, "rates.csv.gz"))
	groups := readGz(t, filepath.Join(out, "groups.csv.gz"))
	// 73721 via group 1; 99213 two prices via group 1, one via remote group 3.
	// Group 2 (NPI not listed) and the inline group are dropped.
	if len(rates)-1 != 4 {
		t.Fatalf("rates: %v", rates)
	}
	for _, r := range rates[1:] {
		if r[1] == "11111" || r[12] == "2" || strings.HasPrefix(r[12], "inline:") {
			t.Fatalf("unexpected row %v", r)
		}
	}
	if len(groups)-1 != 2 || groups[1][1] != "1234567893" || groups[2][0] != "3" {
		t.Fatalf("groups: %v", groups)
	}
	if m.PlaceholderRows != 1 || m.RateRows != 4 || m.ItemsKept != 2 || m.InNetworkItems != 3 || m.RemoteRefsFetched != 1 {
		t.Fatalf("%+v", m)
	}
	if m.ReportingEntityName != "Example Health Plan" || m.Plan["plan_id"] != "12-3456789" || m.SHA256 == "" {
		t.Fatalf("%+v", m)
	}
	if strings.Join(m.CodesFound, ",") != "HCPCS:73721,HCPCS:99213" {
		t.Fatal(m.CodesFound)
	}
}

func TestTINAllowListAndInlineGroups(t *testing.T) {
	out, _ := run(t, Options{Codes: codes("HCPCS:99213"), NPIs: map[int64]bool{}, TINs: map[string]bool{"987654321": true}})
	rates := readGz(t, filepath.Join(out, "rates.csv.gz"))
	if len(rates)-1 != 1 || !strings.HasPrefix(rates[1][12], "inline:") || rates[1][6] != "120" {
		t.Fatalf("%v", rates)
	}
}

func TestNoFiltersKeepsEverythingAndIsDeterministic(t *testing.T) {
	out1, m := run(t, Options{})
	out2, _ := run(t, Options{})
	if m.ItemsKept != 3 {
		t.Fatalf("%+v", m)
	}
	for _, f := range []string{"rates.csv.gz", "groups.csv.gz", "meta.json"} {
		a, _ := os.ReadFile(filepath.Join(out1, f))
		b, _ := os.ReadFile(filepath.Join(out2, f))
		if !bytes.Equal(a, b) {
			t.Fatalf("%s differs between runs", f)
		}
	}
}

func TestCodeKeyAndPlaceholder(t *testing.T) {
	if CodeKey("cpt", " 99213 ") != "HCPCS:99213" || CodeKey("DRG", "470") != "MS-DRG:470" {
		t.Fatal("code key")
	}
	if !IsPlaceholder(0) || !IsPlaceholder(999999) || IsPlaceholder(50) {
		t.Fatal("placeholder")
	}
}

func TestMalformed(t *testing.T) {
	if _, err := Run(strings.NewReader(`{"in_network": [ {"billing_code": `), Options{TempDir: t.TempDir()}, t.TempDir()); err == nil {
		t.Fatal("want error")
	}
}
