// Package reference downloads public CMS provider data for the price-quality join: the NPPES
// NPI registry and CMS Provider Data Catalog datasets (Care Compare). Rows are filtered to the
// configured states while streaming; nothing here is member data.
package reference

import (
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/TheChrisOneil/tic-crawler/internal/fetch"
)

type Config struct {
	States          []string          `toml:"states"`
	NPPESIndexURL   string            `toml:"nppes_index_url"`
	NPPESMaxBytes   int64             `toml:"nppes_max_bytes"`
	ProviderDataAPI string            `toml:"provider_data_api"`
	Datasets        map[string]string `toml:"datasets"`
}

func LoadConfig(path string) (Config, error) {
	var f struct {
		Reference Config `toml:"reference"`
	}
	_, err := toml.DecodeFile(path, &f)
	if err == nil && len(f.Reference.States) == 0 {
		err = errors.New("config has no [reference] states")
	}
	return f.Reference, err
}

func (c Config) stateSet() map[string]bool {
	m := map[string]bool{}
	for _, s := range c.States {
		m[strings.ToUpper(s)] = true
	}
	return m
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// NormHeader turns "Provider Business Practice Location Address State Name" into
// "provider_business_practice_location_address_state_name".
func NormHeader(h string) string {
	return strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(strings.TrimPrefix(h, "\ufeff")), "_"), "_")
}

// Columns finds column indexes by normalized header name; the first candidate present wins.
type Columns map[string]int

func NewColumns(header []string) Columns {
	c := Columns{}
	for i, h := range header {
		c[NormHeader(h)] = i
	}
	return c
}

func (c Columns) Find(candidates ...string) int {
	for _, k := range candidates {
		if i, ok := c[NormHeader(k)]; ok {
			return i
		}
	}
	return -1
}

// GzCSV is a deterministic gzipped CSV writer.
type GzCSV struct {
	*csv.Writer
	gz *gzip.Writer
	f  *os.File
}

func CreateGzCSV(path string, header []string) (*GzCSV, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	w := &GzCSV{csv.NewWriter(gz), gz, f}
	return w, w.Write(header)
}

func (w *GzCSV) Close() error {
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	if err := w.gz.Close(); err != nil {
		return err
	}
	return w.f.Close()
}

// OpenGzCSV opens a gzipped CSV written by CreateGzCSV and returns its reader and columns.
func OpenGzCSV(path string) (*csv.Reader, Columns, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, nil, nil, err
	}
	r := csv.NewReader(bufio.NewReaderSize(gz, 1<<20))
	r.FieldsPerRecord = -1
	h, err := r.Read()
	if err != nil {
		f.Close()
		return nil, nil, nil, err
	}
	return r, NewColumns(h), f.Close, nil
}

// ---------- NPPES ----------

var NPPESHeader = []string{"npi", "entity_type", "name", "address", "city", "state", "zip5", "primary_taxonomy"}

var nppesZipRx = regexp.MustCompile(`href="([^"]*NPPES_Data_Dissemination_[A-Za-z]+_\d{4}(?:_V2)?\.zip)"`)

// FindNPPESZip reads the NPPES files page and returns the full monthly file's URL.
func FindNPPESZip(page, base string) (string, error) {
	m := nppesZipRx.FindStringSubmatch(page)
	if m == nil {
		return "", errors.New("no monthly NPPES_Data_Dissemination zip link on the NPPES files page")
	}
	u := m[1]
	if strings.HasPrefix(u, "./") {
		u = u[2:]
	}
	if !strings.HasPrefix(u, "http") {
		u = strings.TrimSuffix(base[:strings.LastIndex(base, "/")+1], "/") + "/" + strings.TrimPrefix(u, "/")
	}
	return u, nil
}

// FilterNPPES streams the npidata CSV and keeps active providers practicing in the given states.
func FilterNPPES(r io.Reader, states map[string]bool, w *GzCSV) (int, error) {
	cr := csv.NewReader(bufio.NewReaderSize(r, 1<<20))
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	h, err := cr.Read()
	if err != nil {
		return 0, err
	}
	c := NewColumns(h)
	idx := map[string]int{
		"npi":   c.Find("NPI"),
		"etype": c.Find("Entity Type Code"),
		"org":   c.Find("Provider Organization Name (Legal Business Name)"),
		"last":  c.Find("Provider Last Name (Legal Name)"),
		"first": c.Find("Provider First Name"),
		"addr":  c.Find("Provider First Line Business Practice Location Address"),
		"city":  c.Find("Provider Business Practice Location Address City Name"),
		"state": c.Find("Provider Business Practice Location Address State Name"),
		"zip":   c.Find("Provider Business Practice Location Address Postal Code"),
		"deact": c.Find("NPI Deactivation Date"),
		"react": c.Find("NPI Reactivation Date"),
	}
	for k, v := range idx {
		if v < 0 && k != "react" {
			return 0, fmt.Errorf("NPPES file has no column for %s; the file layout changed", k)
		}
	}
	var taxCols, switchCols []int
	for i := 1; i <= 15; i++ {
		taxCols = append(taxCols, c.Find(fmt.Sprintf("Healthcare Provider Taxonomy Code_%d", i)))
		switchCols = append(switchCols, c.Find(fmt.Sprintf("Healthcare Provider Primary Taxonomy Switch_%d", i)))
	}
	n := 0
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		get := func(i int) string {
			if i < 0 || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}
		if !states[strings.ToUpper(get(idx["state"]))] {
			continue
		}
		if get(idx["deact"]) != "" && get(idx["react"]) == "" {
			continue
		}
		name := get(idx["org"])
		if name == "" {
			name = strings.TrimSpace(get(idx["first"]) + " " + get(idx["last"]))
		}
		tax := ""
		for i, tc := range taxCols {
			if get(switchCols[i]) == "Y" {
				tax = get(tc)
				break
			}
		}
		if tax == "" {
			tax = get(taxCols[0])
		}
		zip := get(idx["zip"])
		if len(zip) > 5 {
			zip = zip[:5]
		}
		if err := w.Write([]string{get(idx["npi"]), get(idx["etype"]), name, get(idx["addr"]), get(idx["city"]),
			strings.ToUpper(get(idx["state"])), zip, tax}); err != nil {
			return n, err
		}
		n++
	}
}

// NPPESFromZip finds the npidata file inside the monthly zip and filters it.
func NPPESFromZip(zipPath string, states map[string]bool, out string) (int, string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, "", err
	}
	defer zr.Close()
	rx := regexp.MustCompile(`(?i)^npidata_pfile_\d+-\d+\.csv$`)
	for _, f := range zr.File {
		if !rx.MatchString(filepath.Base(f.Name)) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return 0, "", err
		}
		defer rc.Close()
		w, err := CreateGzCSV(out, NPPESHeader)
		if err != nil {
			return 0, "", err
		}
		n, err := FilterNPPES(rc, states, w)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		return n, f.Name, err
	}
	return 0, "", errors.New("no npidata_pfile_*.csv in the NPPES zip")
}

// ---------- Provider Data Catalog ----------

type DatasetMeta struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Modified    string `json:"modified"`
	DownloadURL string `json:"download_url"`
	Rows        int    `json:"rows_kept"`
	File        string `json:"file"`
}

// DatasetInfo reads a dataset's metastore record and checks its title.
func DatasetInfo(ctx context.Context, c *fetch.Client, api, id, mustContain string) (DatasetMeta, error) {
	txt, err := c.GetText(ctx, api+id, 4<<20)
	if err != nil {
		return DatasetMeta{}, err
	}
	var rec struct {
		Title        string `json:"title"`
		Modified     string `json:"modified"`
		Distribution []struct {
			DownloadURL string `json:"downloadURL"`
			MediaType   string `json:"mediaType"`
		} `json:"distribution"`
	}
	if err := json.Unmarshal([]byte(txt), &rec); err != nil {
		return DatasetMeta{}, fmt.Errorf("%s: metastore record: %w", id, err)
	}
	if !strings.Contains(strings.ToLower(rec.Title), strings.ToLower(mustContain)) {
		return DatasetMeta{}, fmt.Errorf("%s: title %q does not contain %q; check the dataset id", id, rec.Title, mustContain)
	}
	m := DatasetMeta{ID: id, Title: rec.Title, Modified: rec.Modified}
	for _, d := range rec.Distribution {
		if strings.HasSuffix(strings.ToLower(d.DownloadURL), ".csv") || strings.Contains(d.MediaType, "csv") {
			m.DownloadURL = d.DownloadURL
			break
		}
	}
	if m.DownloadURL == "" {
		return m, fmt.Errorf("%s: no CSV distribution", id)
	}
	return m, nil
}

// FilterDataset copies CSV rows whose state column is in states. Without a state column, rows
// are kept when their NPI column is in npis (for national files such as facility affiliations).
func FilterDataset(r io.Reader, states map[string]bool, npis map[string]bool, out string) (int, error) {
	cr := csv.NewReader(bufio.NewReaderSize(r, 1<<20))
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	h, err := cr.Read()
	if err != nil {
		return 0, err
	}
	c := NewColumns(h)
	norm := make([]string, len(h))
	for i, x := range h {
		norm[i] = NormHeader(x)
	}
	stateCol := c.Find("State", "st")
	npiCol := c.Find("NPI")
	if stateCol < 0 && (npiCol < 0 || npis == nil) {
		return 0, errors.New("dataset has neither a state column nor an NPI column to filter on")
	}
	w, err := CreateGzCSV(out, norm)
	if err != nil {
		return 0, err
	}
	n := 0
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			w.Close()
			return n, err
		}
		keep := false
		if stateCol >= 0 && stateCol < len(rec) {
			keep = states[strings.ToUpper(strings.TrimSpace(rec[stateCol]))]
		} else if npiCol >= 0 && npiCol < len(rec) {
			keep = npis[strings.TrimSpace(rec[npiCol])]
		}
		if keep {
			w.Write(rec)
			n++
		}
	}
	return n, w.Close()
}

// LoadNPISet reads the npi column of the filtered NPPES file.
func LoadNPISet(path string) (map[string]bool, error) {
	r, c, closeFn, err := OpenGzCSV(path)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	i := c.Find("npi")
	out := map[string]bool{}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out[rec[i]] = true
	}
}

// SortedKeys returns map keys in order, for deterministic iteration.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
