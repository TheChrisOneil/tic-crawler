package state

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/TheChrisOneil/tic-crawler/internal/model"
)

func open(t *testing.T) *Store {
	s, err := Open(filepath.Join(t.TempDir(), "s", "tic.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPayersRoundTrip(t *testing.T) {
	s := open(t)
	in := []model.Payer{{PayerID: "b", LegalName: "B", Priority: 5}, {PayerID: "a", LegalName: "A", States: []string{"MA"}}}
	if err := s.SavePayers(in); err != nil {
		t.Fatal(err)
	}
	out, err := s.LoadPayers()
	if err != nil || len(out) != 2 || out[0].PayerID != "a" || out[1].Priority != 5 || out[0].States[0] != "MA" {
		t.Fatal(out, err)
	}
}

func TestRunsResumeAndIDs(t *testing.T) {
	s := open(t)
	Now = func() time.Time { return time.Date(2026, 10, 30, 1, 2, 3, 0, time.UTC) }
	defer func() { Now = func() time.Time { return time.Now().UTC() } }()
	id, resumed, _ := s.OpenRun(map[string]any{"all": true})
	if resumed || id != "20261030T010203Z" {
		t.Fatal(id, resumed)
	}
	id2, resumed, _ := s.OpenRun(map[string]any{"all": true})
	if !resumed || id2 != id {
		t.Fatal(id2)
	}
	s.FinishRun(id, "finished")
	id3, _, _ := s.OpenRun(map[string]any{"all": true})
	if id3 != id+"-2" {
		t.Fatal(id3)
	}
}

func TestRecordKeepsCountsOnFailure(t *testing.T) {
	s := open(t)
	ok := model.NewPayerResult("p")
	ok.Status, ok.TocSHA256, ok.TocURL, ok.MrfCount, ok.TotalBytes = "ok", "abc", "https://t", 10, 99
	yes := true
	ok.SchemaValid = &yes
	s.RecordResult("r1", ok)
	failed := model.NewPayerResult("p")
	failed.Status, failed.Error = "deferred", "network"
	s.RecordResult("r2", failed)
	l, _ := s.Latest("p")
	if l["mrf_count"].(float64) != 10 || l["toc_url"] != "https://t" || l["toc_sha256"] != "abc" || l["status"] != "deferred" {
		t.Fatal(l)
	}
	if sha, _ := s.LatestSHA("p"); sha != "abc" {
		t.Fatal(sha)
	}
	done, _ := s.DonePayers("r2")
	res, _ := s.RunResults("r2")
	all, _ := s.AllLatest()
	if !done["p"] || len(res) != 1 || len(all) != 1 {
		t.Fatal(done, res, all)
	}
	pending := model.NewPayerResult("q")
	s.RecordResult("r2", pending)
	if l, _ := s.Latest("q"); l != nil {
		t.Fatal("pending must not touch latest")
	}
	s.MarkUploaded("toc/p.json.gz", "h", "id1")
	if sha, _ := s.UploadedSHA("toc/p.json.gz"); sha != "h" {
		t.Fatal(sha)
	}
	if sha, _ := s.UploadedSHA("x"); sha != "" {
		t.Fatal(sha)
	}
}
