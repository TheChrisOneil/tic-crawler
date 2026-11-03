// Package medicare loads public Medicare fee schedules as price benchmarks, so negotiated
// rates can be read as multiples of what Medicare pays for the same service.
//
//   - OPPS Addendum B (hospital outpatient, national unadjusted payment by HCPCS) is the
//     benchmark for institutional (facility) rates.
//   - The Clinical Laboratory Fee Schedule (national limit) is the benchmark for lab codes,
//     which OPPS packages or pays under the CLFS.
//
// Both are CMS public files; nothing here is member data. Neither is wage-adjusted, and
// inpatient (MS-DRG) and physician (PFS) benchmarks are not loaded yet.
package medicare

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Benchmark is one Medicare payment amount for a HCPCS code.
type Benchmark struct {
	Code            string  // HCPCS, upper case
	Source          string  // opps | clfs
	Amount          float64 // dollars
	StatusIndicator string  // OPPS status indicator (empty for CLFS)
	APC             string
	Description     string
	Release         string // e.g. "October 2026" or "26CLABQ4"
}

var Header = []string{"code", "source", "amount", "status_indicator", "apc", "description", "release"}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func norm(s string) string {
	return strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(s), "_"), "_")
}

var hcpcsRe = regexp.MustCompile(`^[A-Z0-9][0-9]{3}[A-Z0-9]$`)

// table finds the header row in a CMS CSV that starts with free-text notices, and returns
// the normalized header and the data rows after it.
func table(r io.Reader, mustHave ...string) ([]string, [][]string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	recs, err := cr.ReadAll()
	if err != nil {
		return nil, nil, err
	}
	for i, rec := range recs {
		h := make([]string, len(rec))
		for j, c := range rec {
			h[j] = norm(c)
		}
		ok := true
		for _, m := range mustHave {
			if find(h, m) < 0 {
				ok = false
				break
			}
		}
		if ok {
			return h, recs[i+1:], nil
		}
	}
	return nil, nil, fmt.Errorf("no header row with %v", mustHave)
}

// find returns the first column equal to, then starting with, one of the names.
func find(h []string, names ...string) int {
	for _, n := range names {
		for i, c := range h {
			if c == n {
				return i
			}
		}
	}
	for _, n := range names {
		for i, c := range h {
			if strings.HasPrefix(c, n) {
				return i
			}
		}
	}
	return -1
}

func money(s string) (float64, bool) {
	s = strings.TrimSpace(strings.NewReplacer("$", "", ",", "").Replace(s))
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil && v > 0
}

func cell(rec []string, i int) string {
	if i < 0 || i >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[i])
}

// ParseOPPS reads an Addendum B CSV. Rows without a payment rate (packaged, not paid under
// OPPS) are left out.
func ParseOPPS(r io.Reader, release string) ([]Benchmark, error) {
	h, rows, err := table(r, "hcpcs", "payment_rate")
	if err != nil {
		return nil, fmt.Errorf("OPPS Addendum B: %w", err)
	}
	code, si, apc, desc, pay := find(h, "hcpcs_code", "hcpcs"), find(h, "si", "status_indicator"), find(h, "apc"),
		find(h, "short_descriptor", "short_description", "description"), find(h, "payment_rate")
	var out []Benchmark
	for _, rec := range rows {
		c := strings.ToUpper(cell(rec, code))
		amt, ok := money(cell(rec, pay))
		if !hcpcsRe.MatchString(c) || !ok {
			continue
		}
		out = append(out, Benchmark{Code: c, Source: "opps", Amount: amt, StatusIndicator: cell(rec, si),
			APC: cell(rec, apc), Description: cell(rec, desc), Release: release})
	}
	if len(out) == 0 {
		return nil, errors.New("OPPS Addendum B: no payable rows")
	}
	return out, nil
}

// ParseCLFS reads a Clinical Laboratory Fee Schedule CSV (national limit amounts). Rows with
// a modifier are left out.
func ParseCLFS(r io.Reader, release string) ([]Benchmark, error) {
	h, rows, err := table(r, "hcpcs", "rate")
	if err != nil {
		return nil, fmt.Errorf("CLFS: %w", err)
	}
	code, mod, rate, desc := find(h, "hcpcs"), find(h, "mod", "modifier"), find(h, "rate", "national_limit"),
		find(h, "shortdesc", "short_description", "short_desc", "descriptor")
	var out []Benchmark
	for _, rec := range rows {
		c := strings.ToUpper(cell(rec, code))
		amt, ok := money(cell(rec, rate))
		if !hcpcsRe.MatchString(c) || !ok || cell(rec, mod) != "" {
			continue
		}
		out = append(out, Benchmark{Code: c, Source: "clfs", Amount: amt, Description: cell(rec, desc), Release: release})
	}
	if len(out) == 0 {
		return nil, errors.New("CLFS: no rows with a rate")
	}
	return out, nil
}

// CSVsFromZip returns the tables inside a zip as CSV, looking one level into nested zips:
// CSV files first (largest first), then XLSX worksheets converted to CSV. CMS packs the same
// table as CSV, XLSX or both, varying by file and quarter.
func CSVsFromZip(data []byte) ([][]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	var csvs, sheets [][]byte
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		name := strings.ToLower(f.Name)
		isCSV, isXLSX, isZip := strings.HasSuffix(name, ".csv"), strings.HasSuffix(name, ".xlsx"), strings.HasSuffix(name, ".zip")
		if !isCSV && !isXLSX && !isZip {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, 512<<20))
		rc.Close()
		if err != nil {
			return nil, err
		}
		switch {
		case isZip:
			inner, err := CSVsFromZip(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.Name, err)
			}
			csvs = append(csvs, inner...)
		case isXLSX:
			if c, err := xlsxToCSV(b); err == nil {
				sheets = append(sheets, c)
			} // an unreadable workbook is skipped; the error below lists what the zip holds
		default:
			csvs = append(csvs, b)
		}
	}
	sort.SliceStable(csvs, func(i, j int) bool { return len(csvs[i]) > len(csvs[j]) })
	out := append(csvs, sheets...)
	if len(out) == 0 {
		return nil, fmt.Errorf("no CSV or XLSX file in the zip (has: %s)", strings.Join(names, ", "))
	}
	return out, nil
}

// ParseFile parses a downloaded CMS file (a zip or a CSV) with parse, trying each CSV in a
// zip until one parses.
func ParseFile(data []byte, release string, parse func(io.Reader, string) ([]Benchmark, error)) ([]Benchmark, error) {
	if len(data) > 4 && bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		csvs, err := CSVsFromZip(data)
		if err != nil {
			return nil, err
		}
		var firstErr error
		for _, c := range csvs {
			b, err := parse(bytes.NewReader(c), release)
			if err == nil {
				return b, nil
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		return nil, firstErr
	}
	return parse(bytes.NewReader(data), release)
}

// Write stores benchmarks as a gzipped CSV, sorted by code and source.
func Write(path string, bs []Benchmark) error {
	sort.SliceStable(bs, func(i, j int) bool {
		if bs[i].Code != bs[j].Code {
			return bs[i].Code < bs[j].Code
		}
		return bs[i].Source < bs[j].Source
	})
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	w := csv.NewWriter(gz)
	w.Write(Header)
	for _, b := range bs {
		w.Write([]string{b.Code, b.Source, strconv.FormatFloat(b.Amount, 'f', 2, 64), b.StatusIndicator, b.APC, b.Description, b.Release})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Load reads a file written by Write into code -> source -> benchmark.
func Load(path string) (map[string]map[string]Benchmark, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	cr := csv.NewReader(gz)
	cr.FieldsPerRecord = len(Header)
	if _, err := cr.Read(); err != nil {
		return nil, err
	}
	out := map[string]map[string]Benchmark{}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		amt, _ := strconv.ParseFloat(rec[2], 64)
		if out[rec[0]] == nil {
			out[rec[0]] = map[string]Benchmark{}
		}
		out[rec[0]][rec[1]] = Benchmark{Code: rec[0], Source: rec[1], Amount: amt, StatusIndicator: rec[3], APC: rec[4],
			Description: rec[5], Release: rec[6]}
	}
}

// HospitalClinicVisit is what Medicare's OPPS pays a hospital for an outpatient clinic visit.
// Office visit codes (99202-99215) are paid to hospitals under this code, not their own.
const HospitalClinicVisit = "G0463"

var officeVisit = regexp.MustCompile(`^992(0[2-5]|1[2-5])$`)

// Facility returns the Medicare benchmark for a facility (institutional) rate for a HCPCS
// code: the CLFS amount for labs paid under the lab fee schedule, otherwise the OPPS payment
// rate. Office visits map to the hospital clinic visit code. ok is false when Medicare has
// no separate outpatient payment for the code (packaged, inpatient only, or not covered).
func Facility(bm map[string]map[string]Benchmark, code string) (Benchmark, string, bool) {
	code = strings.ToUpper(code)
	if b, ok := bm[code]["clfs"]; ok {
		if o, ok := bm[code]["opps"]; !ok || o.StatusIndicator == "A" || strings.HasPrefix(o.StatusIndicator, "Q4") {
			return b, code, true
		}
	}
	if b, ok := bm[code]["opps"]; ok {
		return b, code, true
	}
	if officeVisit.MatchString(code) {
		if b, ok := bm[HospitalClinicVisit]["opps"]; ok {
			return b, HospitalClinicVisit, true
		}
	}
	return Benchmark{}, "", false
}
