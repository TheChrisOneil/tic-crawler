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
	// 73721 via group 1; 99213 three prices via group 1, one via remote group 3.
	// Group 2 (NPI not listed) and the inline group are dropped.
	if len(rates)-1 != 5 {
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
	if m.PlaceholderRows != 1 || m.RateRows != 5 || m.ItemsKept != 2 || m.InNetworkItems != 3 || m.RemoteRefsFetched != 1 {
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

func TestDerivedColumnsAndCoverage(t *testing.T) {
	dir, m := run(t, Options{Codes: codes("HCPCS:73721", "HCPCS:99213", "HCPCS:70553"), FileID: "abc123def456", Today: "2026-11-03"})
	recs := readGz(t, filepath.Join(dir, "rates.csv.gz"))
	col := map[string]int{}
	for i, h := range recs[0] {
		col[h] = i
	}
	for _, h := range []string{"file_id", "network_name", "comparable_key", "flags", "provider_group", "placeholder"} {
		if _, ok := col[h]; !ok {
			t.Fatalf("missing column %s in %v", h, recs[0])
		}
	}
	byRate := map[string][]string{}
	for _, r := range recs[1:] {
		if r[col["file_id"]] != "abc123def456" {
			t.Fatalf("file_id = %q", r[col["file_id"]])
		}
		byRate[r[col["negotiated_rate"]]+"/"+r[col["provider_group"]]] = r
	}
	mri := byRate["1250.5/1"]
	if mri == nil || mri[col["network_name"]] != "PPO" || mri[col["flags"]] != "" ||
		mri[col["comparable_key"]] != "HCPCS:73721||institutional|outpatient|dollar|ffs" {
		t.Fatalf("MRI row: %v", mri)
	}
	if r := byRate["98.12/1"]; r == nil || r[col["comparable_key"]] != "HCPCS:99213|25|professional|both|dollar|ffs" {
		t.Fatalf("modifier row: %v", r)
	}
	if r := byRate["0.01/1"]; r == nil || r[col["flags"]] != "placeholder" {
		t.Fatalf("placeholder row: %v", r)
	}
	if r := byRate["85/1"]; r == nil || r[col["flags"]] != "not_dollar" || !strings.HasSuffix(r[col["comparable_key"]], "|percentage|ffs") {
		t.Fatalf("percentage row: %v", r)
	}
	if len(m.CodesMissing) != 1 || m.CodesMissing[0] != "HCPCS:70553" {
		t.Fatalf("codes_missing = %v", m.CodesMissing)
	}
	var visit *CodeCoverage
	for i := range m.Coverage {
		if m.Coverage[i].Code == "HCPCS:99213" {
			visit = &m.Coverage[i]
		}
	}
	// Usable 99213 rates: 98.12 (groups 1, 2), 120 (inline) and 77 (remote group 3).
	if visit == nil || visit.ProviderGroups != 4 || visit.TINs != 4 || visit.NPIs != 5 || visit.UsableRows != 4 {
		t.Fatalf("99213 coverage = %+v", visit)
	}
	if m.FlaggedRows["placeholder"] != 2 || m.FlaggedRows["not_dollar"] != 2 {
		t.Fatalf("flagged = %v", m.FlaggedRows)
	}
}

func TestRowFlags(t *testing.T) {
	cases := []struct {
		rate, typ, exp, arr, want string
	}{
		{"100", "negotiated", "9999-12-31", "ffs", ""},
		{"100", "negotiated", "2026-01-31", "ffs", "expired"},
		{"100", "per diem", "9999-12-31", "bundle", "not_dollar|non_ffs"},
		{"999999", "fee schedule", "", "", "placeholder"},
		{"abc", "derived", "", "ffs", "placeholder"},
	}
	for _, c := range cases {
		if got := strings.Join(RowFlags(c.rate, c.typ, c.exp, c.arr, "2026-11-03"), "|"); got != c.want {
			t.Errorf("RowFlags(%v) = %q, want %q", c, got, c.want)
		}
	}
}
