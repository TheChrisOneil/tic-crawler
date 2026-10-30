package quality

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/TheChrisOneil/tic-crawler/internal/extract"
	"github.com/TheChrisOneil/tic-crawler/internal/reference"
)

func writeGz(t *testing.T, path string, header []string, rows ...[]string) {
	t.Helper()
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

func TestHelpers(t *testing.T) {
	if AddressKey("55 Fruit Street") != "55 fruit" || AddressKey("Fruit St") != "" {
		t.Fatal(AddressKey("55 Fruit Street"))
	}
	if j := Jaccard("Massachusetts General Hospital", "MASSACHUSETTS GENERAL HOSPITAL, THE"); j != 1 {
		t.Fatal(j)
	}
}

func TestCrosswalkAndJoin(t *testing.T) {
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref")
	writeGz(t, filepath.Join(ref, "nppes.csv.gz"), reference.NPPESHeader,
		[]string{"1234567893", "2", "MASSACHUSETTS GENERAL HOSPITAL", "55 FRUIT ST", "BOSTON", "MA", "02114", "282N00000X"},
		[]string{"3333333333", "1", "JOHN DOE", "2 ELM ST", "CONCORD", "NH", "03301", "207Q00000X"},
		[]string{"4444444444", "2", "UNRELATED CLINIC", "9 OAK ST", "BOSTON", "MA", "02114", "261Q00000X"})
	writeGz(t, filepath.Join(ref, "hgi.csv.gz"), []string{"facility_id", "facility_name", "address", "citytown", "state", "zip_code", "hospital_type", "hospital_overall_rating"},
		[]string{"220071", "MASSACHUSETTS GENERAL HOSPITAL", "55 FRUIT STREET", "BOSTON", "MA", "02114", "Acute Care Hospitals", "5"},
		[]string{"300001", "CONCORD HOSPITAL", "250 PLEASANT ST", "CONCORD", "NH", "03301", "Acute Care Hospitals", "Not Available"})
	writeGz(t, filepath.Join(ref, "aff.csv.gz"), []string{"npi", "facility_type", "facility_afl_ccn"},
		[]string{"3333333333", "Hospital", "300001"}, []string{"3333333333", "Nursing home", "999999"})
	providers, err := LoadProviders(filepath.Join(ref, "nppes.csv.gz"))
	if err != nil {
		t.Fatal(err)
	}
	hospitals, err := LoadHospitals(filepath.Join(ref, "hgi.csv.gz"))
	if err != nil {
		t.Fatal(err)
	}
	aff, err := LoadAffiliations(filepath.Join(ref, "aff.csv.gz"))
	if err != nil || len(aff["3333333333"]) != 1 {
		t.Fatal(aff, err)
	}
	links := BuildCrosswalk(providers, hospitals, aff)
	if len(links) != 2 || links[0].Method != "address" || links[0].CCN != "220071" || links[1].Method != "affiliation" {
		t.Fatalf("%+v", links)
	}
	if err := WriteCrosswalk(filepath.Join(dir, "cw.csv.gz"), links, providers, hospitals); err != nil {
		t.Fatal(err)
	}

	// Extract the shared fixture, then join it.
	f, _ := os.Open("../extract/testdata/in-network.json")
	defer f.Close()
	rates := filepath.Join(dir, "rates", "p")
	if _, err := extract.Run(f, extract.Options{TempDir: dir}, filepath.Join(rates, "file1")); err != nil {
		t.Fatal(err)
	}
	if npis, err := InstitutionalNPIs(rates); err != nil || len(npis) != 2 || npis[0] != "1111111111" {
		t.Fatal(npis, err) // the MRI price uses group 1 (two NPIs) and is institutional
	}
	labels := map[string]string{"HCPCS:99213": "Office visit"}
	sum, err := JoinPrices("p", rates, filepath.Join(dir, "q"), providers, hospitals, links, labels)
	if err != nil {
		t.Fatal(err)
	}
	// Only NPI 1234567893 (MA) is in both the file and the state list.
	if sum.Files != 1 || sum.Providers != 1 || sum.ProvidersWithCCN != 1 || sum.Hospitals != 1 || sum.RowsInStates != 4 {
		b, _ := json.Marshal(sum)
		t.Fatalf("%s", b)
	}
	var visit *CodeStats
	for _, c := range sum.Codes {
		if c.Code == "HCPCS:99213" {
			visit = c
		}
	}
	if visit == nil || visit.Label != "Office visit" || visit.ProvidersWithStars != 1 || visit.Min != 98.12 || visit.Max != 98.12 {
		t.Fatalf("%+v", visit)
	}

	mw, _ := reference.CreateGzCSV(filepath.Join(dir, "m.csv.gz"), MeasureHeader)
	writeGz(t, filepath.Join(ref, "hcahps.csv.gz"), []string{"facility_id", "hcahps_measure_id", "hcahps_question", "patient_survey_star_rating", "hcahps_answer_percent"},
		[]string{"220071", "H_STAR_RATING", "Summary star rating", "4", "Not Applicable"})
	n, err := AppendMeasures(mw, filepath.Join(ref, "hcahps.csv.gz"), "hcahps", nil)
	mw.Close()
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
