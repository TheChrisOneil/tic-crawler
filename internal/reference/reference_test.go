package reference

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TheChrisOneil/tic-crawler/internal/config"
	"github.com/TheChrisOneil/tic-crawler/internal/fetch"
)

const nppesCSV = `"NPI","Entity Type Code","Provider Organization Name (Legal Business Name)","Provider Last Name (Legal Name)","Provider First Name","Provider First Line Business Practice Location Address","Provider Business Practice Location Address City Name","Provider Business Practice Location Address State Name","Provider Business Practice Location Address Postal Code","NPI Deactivation Date","NPI Reactivation Date","Healthcare Provider Taxonomy Code_1","Healthcare Provider Primary Taxonomy Switch_1","Healthcare Provider Taxonomy Code_2","Healthcare Provider Primary Taxonomy Switch_2"
"1234567893","2","MASS GENERAL HOSPITAL","","","55 FRUIT ST","BOSTON","MA","021142696","","","261QM0801X","N","282N00000X","Y"
"2222222222","1","","SMITH","JANE","1 MAIN ST","DALLAS","TX","75201","","","207Q00000X","Y","",""
"3333333333","1","","DOE","JOHN","2 ELM ST","CONCORD","NH","03301","01/01/2020","","207Q00000X","Y","",""
`

func TestFilterNPPES(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.csv.gz")
	w, _ := CreateGzCSV(path, NPPESHeader)
	n, err := FilterNPPES(strings.NewReader(nppesCSV), map[string]bool{"MA": true, "NH": true}, w)
	w.Close()
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	r, c, closeFn, _ := OpenGzCSV(path)
	defer closeFn()
	rec, _ := r.Read()
	if rec[c.Find("zip5")] != "02114" || rec[c.Find("primary_taxonomy")] != "282N00000X" || rec[c.Find("name")] != "MASS GENERAL HOSPITAL" {
		t.Fatal(rec)
	}
	set, err := LoadNPISet(path)
	if err != nil || !set["1234567893"] || len(set) != 1 {
		t.Fatal(set, err)
	}
}

func TestFilterNPPESLayoutChange(t *testing.T) {
	w, _ := CreateGzCSV(filepath.Join(t.TempDir(), "n.csv.gz"), NPPESHeader)
	defer w.Close()
	if _, err := FilterNPPES(strings.NewReader("NPI,Foo\n1,2\n"), nil, w); err == nil {
		t.Fatal("want layout error")
	}
}

func TestFindNPPESZip(t *testing.T) {
	page := `<a href="./NPPES_Data_Dissemination_Weekly_102026_V2.zip">w</a> <a href="NPPES_Data_Dissemination_October_2026_V2.zip">m</a>`
	u, err := FindNPPESZip(page, "https://download.cms.gov/nppes/NPI_Files.html")
	if err != nil || u != "https://download.cms.gov/nppes/NPPES_Data_Dissemination_October_2026_V2.zip" {
		t.Fatal(u, err)
	}
	if _, err := FindNPPESZip("nothing", "https://x/y.html"); err == nil {
		t.Fatal("want error")
	}
}

func TestFilterDatasetByStateAndNPI(t *testing.T) {
	dir := t.TempDir()
	n, err := FilterDataset(strings.NewReader("Facility ID,State,Score\n220071,MA,1\n450000,TX,2\n"), map[string]bool{"MA": true}, nil,
		filepath.Join(dir, "a.csv.gz"))
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	r, c, closeFn, _ := OpenGzCSV(filepath.Join(dir, "a.csv.gz"))
	defer closeFn()
	if c.Find("facility_id") != 0 {
		t.Fatal(c)
	}
	rec, _ := r.Read()
	if rec[0] != "220071" {
		t.Fatal(rec)
	}
	n, err = FilterDataset(strings.NewReader("NPI,facility_afl_ccn\n1,220071\n2,450000\n"), nil, map[string]bool{"1": true},
		filepath.Join(dir, "b.csv.gz"))
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err := FilterDataset(strings.NewReader("a,b\n1,2\n"), nil, nil, filepath.Join(dir, "c.csv.gz")); err == nil {
		t.Fatal("want error")
	}
}

func TestDatasetInfoChecksTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, `{"title":"Hospital General Information","modified":"2026-10-01",
		  "distribution":[{"downloadURL":"https://x/Hospital_General_Information.csv","mediaType":"text/csv"}]}`)
	}))
	defer srv.Close()
	s := config.Defaults()
	s.RequestsPerSecondHost = 0
	c := fetch.New(s)
	c.Sleep = func(time.Duration) {}
	m, err := DatasetInfo(context.Background(), c, srv.URL+"/items/", "xubh-q36u", "hospital general information")
	if err != nil || m.DownloadURL != "https://x/Hospital_General_Information.csv" || m.Modified != "2026-10-01" {
		t.Fatal(m, err)
	}
	if _, err := DatasetInfo(context.Background(), c, srv.URL+"/items/", "xubh-q36u", "hcahps"); err == nil {
		t.Fatal("want title mismatch")
	}
}

func TestLoadConfig(t *testing.T) {
	rc, err := LoadConfig("../../config.toml")
	if err != nil || len(rc.Datasets) < 7 || rc.States[0] != "MA" {
		t.Fatal(rc, err)
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "none.toml")); err == nil || !errors.Is(err, err) {
		t.Fatal("want error")
	}
}

func TestParseNPIResponse(t *testing.T) {
	body := `{"result_count":1,"results":[{"number":1234567893,"enumeration_type":"NPI-2",
	  "basic":{"organization_name":"MASSACHUSETTS GENERAL HOSPITAL","status":"A"},
	  "addresses":[{"address_purpose":"MAILING","address_1":"PO BOX 1","city":"X","state":"TX","postal_code":"75201"},
	               {"address_purpose":"LOCATION","address_1":"55 FRUIT ST","city":"BOSTON","state":"MA","postal_code":"021142696"}],
	  "taxonomies":[{"code":"261QM0801X","primary":false},{"code":"282N00000X","primary":true}]}]}`
	row, err := ParseNPIResponse([]byte(body))
	want := []string{"1234567893", "2", "MASSACHUSETTS GENERAL HOSPITAL", "55 FRUIT ST", "BOSTON", "MA", "02114", "282N00000X"}
	if err != nil || strings.Join(row, "|") != strings.Join(want, "|") {
		t.Fatal(row, err)
	}
	if row, err := ParseNPIResponse([]byte(`{"result_count":0,"results":[]}`)); row != nil || err != nil {
		t.Fatal(row, err)
	}
	if _, err := ParseNPIResponse([]byte(`{"Errors":[{"description":"bad"}]}`)); err == nil {
		t.Fatal("want error")
	}
}
