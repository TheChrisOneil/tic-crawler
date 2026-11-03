package discovery

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/TheChrisOneil/tic-crawler/internal/config"
	"github.com/TheChrisOneil/tic-crawler/internal/fetch"
	"github.com/TheChrisOneil/tic-crawler/internal/model"
)

const pufCSV = "State,Issuer ID,Issuer Name,URL Submitted\n" +
	"TX,12345,Blue Cross and Blue Shield of Texas,https://a\n" +
	"TX,12345,Blue Cross and Blue Shield of Texas,https://a\n" +
	"OK,67890,\"Aetna Life Insurance Company\",https://b\n" +
	"FL,67891,Aetna Life Insurance Co.,https://b\n" +
	",,,\n"

const seed = `
payers:
  - payer_id: aetna
    legal_name: Aetna Inc.
    aliases: [Aetna Life Insurance Company]
    landing_url: https://www.aetna.com/tic
    toc_url: https://mrf.aetna.com/{date:%Y-%m}-01_toc.json
  - legal_name: The Example Health Plan, LLC
    hios_ids: [12345]
    kind: tpa
    priority: 5
`

func TestNormalizeAndSlug(t *testing.T) {
	cases := map[string]string{
		"Aetna Life Insurance Company":            "aetna life insurance",
		"Aetna Life Insurance Co.":                "aetna life insurance",
		"The Cigna Group, Inc.":                   "cigna group",
		"Blue Cross & Blue Shield of Texas":       "blue cross and blue shield of texas",
		"UnitedHealthcare Insurance Company, LLC": "unitedhealthcare insurance",
	}
	for in, want := range cases {
		if got := NormalizeName(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
	if Slugify("The Example Health Plan, LLC") != "example-health-plan" || Slugify("!!!") != "payer" {
		t.Fatal(Slugify("The Example Health Plan, LLC"))
	}
}

func TestSeedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "payers.yaml")
	os.WriteFile(p, []byte(seed), 0o644)
	ps, err := LoadSeedFile(p)
	if err != nil || len(ps) != 2 {
		t.Fatal(ps, err)
	}
	if ps[1].PayerID != "example-health-plan" || ps[1].HIOSIDs[0] != "12345" || ps[1].Kind != "tpa" || ps[1].Priority != 5 || ps[0].Priority != 100 {
		t.Fatalf("%+v", ps[1])
	}
	if _, err := LoadSeedFile("../../seeds/payers.yaml"); err != nil {
		t.Fatal("repo seeds:", err)
	}
}

func TestParsePUFCSVAndZip(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "puf.csv")
	os.WriteFile(csvPath, []byte(pufCSV), 0o644)
	zipPath := filepath.Join(dir, "puf.zip")
	f, _ := os.Create(zipPath)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("data/puf.csv")
	w.Write([]byte(pufCSV))
	zw.Close()
	f.Close()
	for _, p := range []string{csvPath, zipPath} {
		ps, err := ParsePUF(p)
		if err != nil || len(ps) != 3 || ps[0].PayerID != "hios-12345" || !reflect.DeepEqual(ps[0].States, []string{"TX"}) {
			t.Fatalf("%s: %+v %v", p, ps, err)
		}
	}
}

func TestDedupe(t *testing.T) {
	in := []model.Payer{
		{PayerID: "aetna", LegalName: "Aetna Inc.", Aliases: []string{"Aetna Life Insurance Company"}, TocURL: "https://t", Sources: []string{"seed"}, Priority: 100},
		{PayerID: "ex", LegalName: "Example Plan", HIOSIDs: []string{"12345"}, Sources: []string{"seed"}, Priority: 100},
		{PayerID: "hios-67890", LegalName: "Aetna Life Insurance Co", HIOSIDs: []string{"67890"}, States: []string{"OK"}, LandingURL: "https://l", Sources: []string{"exchange_puf"}, Priority: 100},
		{PayerID: "hios-12345", LegalName: "Different Legal Name Inc", HIOSIDs: []string{"12345"}, States: []string{"TX"}, Sources: []string{"exchange_puf"}, Priority: 1},
	}
	out := Dedupe(in)
	if len(out) != 2 || out[0].PayerID != "aetna" || out[1].PayerID != "ex" {
		t.Fatalf("%+v", out)
	}
	if !reflect.DeepEqual(out[0].HIOSIDs, []string{"67890"}) || out[0].TocURL != "https://t" || out[0].LandingURL != "https://l" ||
		!reflect.DeepEqual(out[0].Sources, []string{"seed", "exchange_puf"}) {
		t.Fatalf("%+v", out[0])
	}
	if out[1].Priority != 1 || out[1].Aliases[0] != "Different Legal Name Inc" {
		t.Fatalf("%+v", out[1])
	}
}

func TestRootTxt(t *testing.T) {
	tocs, contact := ParseRootTxt("toc: https://a/b.json\ncontact: mrf@payer.com\n")
	if len(tocs) != 1 || tocs[0] != "https://a/b.json" || contact != "mrf@payer.com" {
		t.Fatal(tocs, contact)
	}
	r := &RootTxt{From: time.Date(2027, 9, 6, 0, 0, 0, 0, time.UTC)}
	if r.Active(time.Date(2027, 9, 5, 0, 0, 0, 0, time.UTC)) || !r.Active(time.Date(2027, 9, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("active")
	}
}

type broken struct{}

func (broken) Name() string          { return "broken_test_source" }
func (broken) Order() int            { return 50 }
func (broken) Active(time.Time) bool { return true }
func (broken) Discover(context.Context, *fetch.Client, []model.Payer) ([]model.Payer, error) {
	return nil, errors.New("down")
}

func TestRunIsolatesFailures(t *testing.T) {
	p := filepath.Join(t.TempDir(), "payers.yaml")
	os.WriteFile(p, []byte(seed), 0o644)
	s := config.Defaults()
	s.SeedFile = p
	Register("broken_test_source", func(config.Settings) Source { return broken{} })
	defer Unregister("broken_test_source")
	payers, status := Run(context.Background(), s, nil, time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), nil)
	if status["seed"] != "ok:2" || status["root_txt"] != "inactive" || status["exchange_puf"] != "ok:0" ||
		status["broken_test_source"] != "error:*errors.errorString: down" || len(payers) != 2 {
		t.Fatal(status, len(payers))
	}
	_, status = Run(context.Background(), s, nil, time.Now(), []string{"seed"})
	if len(status) != 1 {
		t.Fatal(status)
	}
}
