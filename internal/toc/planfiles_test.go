package toc

import (
	"compress/gzip"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritePlanFilesUncapped(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"reporting_entity_name":"X","reporting_structure":[{"reporting_plans":[`)
	for i := 0; i < 120; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"plan_name":"Plan ` + strings.Repeat("a", i%3) + string(rune('A'+i%26)) + `","plan_id_type":"ein","plan_id":"` + string(rune('0'+i%10)) + `","plan_market_type":"group"}`)
	}
	b.WriteString(`],"in_network_files":[{"description":"d","location":"https://x.test/a.json"},{"description":"d2","location":"https://x.test/b.json"}],` +
		`"allowed_amount_file":{"description":"aa","location":"https://x.test/aa.json"}}]}`)
	dest := filepath.Join(t.TempDir(), "plans.csv.gz")
	n, err := WritePlanFiles(strings.NewReader(b.String()), dest)
	if err != nil {
		t.Fatal(err)
	}
	if n != 360 {
		t.Fatalf("rows = %d, want 360 (120 plans x 3 files)", n)
	}
	f, _ := os.Open(dest)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(gz).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(recs[0], ",") != strings.Join(PlanFilesHeader, ",") || len(recs) != 361 {
		t.Fatalf("header %v, %d rows", recs[0], len(recs))
	}
	if recs[1][7] != FileID("https://x.test/a.json") || recs[3][6] != "allowed-amount" {
		t.Fatalf("rows: %v / %v", recs[1], recs[3])
	}
}

func TestWritePlanFilesBadTOCLeavesNothing(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "plans.csv.gz")
	if _, err := WritePlanFiles(strings.NewReader(`[1,2]`), dest); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("file left behind")
	}
}
