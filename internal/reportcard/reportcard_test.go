package reportcard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TheChrisOneil/tic-crawler/internal/extract"
	"github.com/TheChrisOneil/tic-crawler/internal/medicare"
	"github.com/TheChrisOneil/tic-crawler/internal/reference"
)

func writeGz(t *testing.T, path string, header []string, rows [][]string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	w, err := reference.CreateGzCSV(path, header)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		w.Write(r)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func rate(code, class, ntype, v, group, flags string) []string {
	return []string{"CPT", code, "2026", "ffs", "x", ntype, v, class, "outpatient", "", "", "9999-12-31", group, "0",
		"f1", "PPO", extract.ComparableKey("CPT", code, "", class, "outpatient", ntype, "ffs"), flags}
}

func TestBuild(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"f1", "f2"} {
		writeGz(t, filepath.Join(dir, f, "groups.csv.gz"), []string{"provider_group", "npi", "tin_type", "tin_value", "business_name", "network_name"}, [][]string{
			{"1", "100", "ein", "11-1", "Mass General", "PPO"}, {"1", "101", "ein", "11-1", "Mass General", "PPO"}, // two NPIs, one TIN
			{"2", "200", "ein", "22-2", "Small Hospital", "HMO"},
			{"3", "300", "ein", "33-3", "Doctor Group", "PPO"}, // not a hospital
		})
	}
	writeGz(t, filepath.Join(dir, "f1", "rates.csv.gz"), extract.RateHeader, [][]string{
		rate("73721", "institutional", "negotiated", "500", "1", ""),  // MGH 2x
		rate("73721", "institutional", "negotiated", "1000", "1", ""), // MGH 4x
		rate("73721", "institutional", "negotiated", "300", "2", ""),  // small 1.2x
		rate("73721", "institutional", "negotiated", "9000", "2", ""), // 36x: outlier
		rate("73721", "institutional", "percentage", "60", "2", "not_dollar"),
		rate("73721", "professional", "negotiated", "80", "1", ""),   // professional: ignored
		rate("73721", "institutional", "negotiated", "500", "3", ""), // no hospital link
		rate("99213", "institutional", "negotiated", "135", "1", ""), // -> G0463 1x
		rate("99385", "institutional", "negotiated", "200", "1", ""), // no benchmark
	})
	writeGz(t, filepath.Join(dir, "f2", "rates.csv.gz"), extract.RateHeader, [][]string{
		rate("73721", "institutional", "negotiated", "1500", "1", ""), // MGH 6x in the other file
	})
	os.WriteFile(filepath.Join(dir, "index.json"), []byte(`{"files":[{"dir":"f1","plan_count":2,"plan_names":["Gold PPO","Silver PPO"],"status":"ok",
	  "meta":{"reporting_entity_name":"Example Health","last_updated_on":"2026-10-01"}}]}`), 0o644)
	links := []Link{{"100", "220071", "high"}, {"101", "220071", "medium"}, {"200", "300001", "high"}, {"300", "220071", "low"}}
	hospitals := map[string]Hospital{"220071": {"220071", "Mass General", "Boston", "MA"}, "300001": {"300001", "Small Hospital", "Nashua", "NH"}}
	bm := map[string]map[string]medicare.Benchmark{
		"73721": {"opps": {Code: "73721", Source: "opps", Amount: 250, Release: "OPPS Oct 2026"}},
		"G0463": {"opps": {Code: "G0463", Source: "opps", Amount: 135, Release: "OPPS Oct 2026"}},
	}
	c, err := Build("ex", dir, links, hospitals, bm, map[string]string{"HCPCS:73721": "Knee MRI"}, "2026-11-03T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if c.PayerName != "Example Health" || len(c.Files) != 2 || len(c.Hospitals) != 2 || c.HospitalsAll != 2 {
		t.Fatalf("card: %+v", c)
	}
	mgh := c.Hospitals[0].Cells["HCPCS:73721"]
	// MGH: 500 and 1000 in f1 (deduped across its two NPIs) and 1500 in f2 -> median 1000 = 4x.
	if mgh == nil || mgh.Rates != 3 || mgh.Median != 1000 || mgh.Multiple != 4 || mgh.ByFile["f1"] != 3 || mgh.ByFile["f2"] != 6 {
		t.Fatalf("MGH cell: %+v", mgh)
	}
	small := c.Hospitals[1].Cells["HCPCS:73721"]
	if small == nil || small.Rates != 1 || small.Multiple != 1.2 || small.Outliers != 1 {
		t.Fatalf("small cell: %+v", small)
	}
	if v := c.Hospitals[0].Cells["HCPCS:99213"]; v == nil || v.Multiple != 1 {
		t.Fatalf("office visit vs G0463: %+v", v)
	}
	if c.Hospitals[0].FlatCodes != 0 {
		t.Fatalf("flat rate flagged with 2 services: %+v", c.Hospitals[0])
	}
	if c.Excluded["not_dollar"] != 1 || c.Excluded["outlier_vs_medicare"] != 1 {
		t.Fatalf("excluded: %v", c.Excluded)
	}
	if len(c.NoBenchmark) != 1 || c.NoBenchmark[0] != "HCPCS:99385" {
		t.Fatalf("no benchmark: %v", c.NoBenchmark)
	}
	if len(c.Codes) != 2 || c.Codes[0].Code != "HCPCS:73721" || c.Codes[0].Label != "Knee MRI" || c.Codes[0].Hospitals != 2 ||
		c.Codes[0].MinMultiple != 1.2 || c.Codes[0].MaxMultiple != 4 || c.Codes[1].MedicareCode != "G0463" {
		t.Fatalf("codes: %+v %+v", c.Codes[0], c.Codes[1])
	}
}

func TestFlatRateFlag(t *testing.T) {
	dir := t.TempDir()
	writeGz(t, filepath.Join(dir, "f1", "groups.csv.gz"), []string{"provider_group", "npi", "tin_type", "tin_value", "business_name", "network_name"},
		[][]string{{"1", "100", "ein", "11-1", "H", ""}})
	var rows [][]string
	for _, c := range []string{"99202", "99203", "99204", "99205", "99213"} {
		rows = append(rows, rate(c, "institutional", "negotiated", "2004", "1", ""))
	}
	writeGz(t, filepath.Join(dir, "f1", "rates.csv.gz"), extract.RateHeader, rows)
	bm := map[string]map[string]medicare.Benchmark{"G0463": {"opps": {Code: "G0463", Source: "opps", Amount: 135}}}
	c, err := Build("ex", dir, []Link{{"100", "1", "high"}}, map[string]Hospital{"1": {CCN: "1", Name: "H"}}, bm, nil, "2026-11-03T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if h := c.Hospitals[0]; h.FlatRate != 2004 || h.FlatCodes != 5 {
		t.Fatalf("flat: %+v", h)
	}
}
