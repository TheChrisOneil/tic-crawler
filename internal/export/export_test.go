package export

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TheChrisOneil/tic-crawler/internal/config"
	"github.com/TheChrisOneil/tic-crawler/internal/model"
	"github.com/TheChrisOneil/tic-crawler/internal/state"
)

const manifest = "payer_id,file_type,url,description,plan_count,plan_names,plans_truncated,plan_id_types," +
	"plan_market_types,http_status,check_method,content_length,last_modified,etag,check_error\r\n" +
	"a,in-network,https://a/1.json.gz,,3,P1 | P2,False,ein,group,200,HEAD,100,,,\r\n" +
	"a,in-network,https://a/2.json.gz,,1,P1,False,ein,group,403,GET,,,,http_403\r\n" +
	"a,allowed-amount,ftp://a/3,,1,P1,False,ein,group,,,,,,invalid_url\r\n"

func TestBuild(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.LocalDir = filepath.Join(dir, "out")
	st, err := state.Open(filepath.Join(dir, "tic.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	payers := map[string]model.Payer{
		"a": {PayerID: "a", LegalName: "A Health", TocURL: "https://a/toc.json", Cohorts: []string{"ma"}},
		"b": {PayerID: "b", LegalName: "B Health", LandingURL: "https://b/tic"},
		"c": {PayerID: "c", LegalName: "C Health"},
	}
	run, _, err := st.OpenRun(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	valid := false
	ra := model.NewPayerResult("a")
	ra.Status, ra.AccessStatus, ra.TocURL, ra.MrfCount, ra.TotalBytes = "ok", "ok", "https://a/toc.json", 3, 100
	ra.TocLastUpdatedOn, ra.SchemaValid = "2026-01-01", &valid
	ra.Validation = json.RawMessage(`{"valid":false,"error_count":2,"item_count":5,"invalid_items":1,"file_version":"2.0.0",
		"error_kinds":[{"kind":"x: bad","count":2,"examples":["x"]}]}`)
	rb := model.NewPayerResult("b")
	rb.Status, rb.AccessStatus, rb.Error = "failed", "waf_blocked", "waf_blocked 403"
	for _, r := range []model.PayerResult{ra, rb} {
		if err := st.RecordResult(run, r); err != nil {
			t.Fatal(err)
		}
	}
	mdir := filepath.Join(cfg.LocalDir, "manifests", "a")
	os.MkdirAll(mdir, 0o755)
	os.WriteFile(filepath.Join(mdir, "2026-01-01.csv"), []byte("old"), 0o644)
	if err := os.WriteFile(filepath.Join(mdir, "2026-02-01.csv"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	dest, err := Build(cfg, st, payers, time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Totals map[string]int64
		Payers []map[string]any
		Errors map[string][]map[string]any
	}
	b, _ := os.ReadFile(filepath.Join(dest, "data.json"))
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"payers": 3, "with_toc_source": 1, "crawled": 2, "ok": 1, "failed": 1,
		"stale": 1, "mrf_urls": 3, "mrf_bytes": 100, "schema_invalid": 1}
	for k, v := range want {
		if data.Totals[k] != v {
			t.Errorf("totals[%s] = %d, want %d", k, data.Totals[k], v)
		}
	}
	if got := data.Payers[2]["status"]; got != "never" {
		t.Errorf("payer c status %v", got)
	}
	us := data.Payers[0]["url_status"].(map[string]any)
	if us["200"] != 1.0 || us["403"] != 1.0 || us["invalid_url"] != 1.0 {
		t.Errorf("url_status %v", us)
	}
	// One payer-level failure plus two MRF check errors.
	if n := len(data.Errors["access"]); n != 3 {
		t.Errorf("access errors %d: %v", n, data.Errors["access"])
	}
	if r := data.Errors["records"]; len(r) != 1 || r[0]["payer_id"] != "a" || r[0]["count"] != 2.0 {
		t.Errorf("records %v", r)
	}
	if data.Errors["access"][2]["url"] != "https://b/tic" {
		t.Errorf("failed payer without TOC should point at landing page: %v", data.Errors["access"][2])
	}
	var detail map[string]any
	b, _ = os.ReadFile(filepath.Join(dest, "payers", "a.json"))
	json.Unmarshal(b, &detail)
	if detail["manifest_file"] != "manifests/a/2026-02-01.csv" || len(detail["rows"].([]any)) != 3 {
		t.Errorf("detail %v %v", detail["manifest_file"], detail["rows"])
	}
}
