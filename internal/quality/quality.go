// Package quality links TiC providers to CMS hospitals and joins extracted prices to public
// CMS quality measures. Every link records how it was made; nothing is guessed.
package quality

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/TheChrisOneil/tic-crawler/internal/extract"
	"github.com/TheChrisOneil/tic-crawler/internal/reference"
)

var stopWords = map[string]bool{"the": true, "of": true, "inc": true, "llc": true, "and": true, "corp": true,
	"corporation": true, "co": true, "dba": true}

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if !stopWords[t] {
			out[t] = true
		}
	}
	return out
}

// Jaccard similarity of two names' word sets.
func Jaccard(a, b string) float64 {
	ta, tb := tokens(a), tokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(ta)+len(tb)-inter)
}

var streetAbbrev = map[string]string{"street": "st", "avenue": "ave", "road": "rd", "drive": "dr", "boulevard": "blvd",
	"place": "pl", "lane": "ln", "court": "ct", "highway": "hwy", "parkway": "pkwy", "square": "sq"}

// AddressKey is "number word" from a street address, e.g. "55 fruit" for "55 Fruit Street".
func AddressKey(addr string) string {
	f := strings.Fields(strings.ToLower(strings.NewReplacer(",", " ", ".", " ", "#", " ").Replace(addr)))
	if len(f) < 2 || f[0][0] < '0' || f[0][0] > '9' {
		return ""
	}
	w := f[1]
	if v, ok := streetAbbrev[w]; ok {
		w = v
	}
	return f[0] + " " + w
}

func zip5(z string) string {
	z = strings.TrimSpace(z)
	if len(z) > 5 {
		z = z[:5]
	}
	return z
}

type Hospital struct {
	CCN, Name, Address, City, State, Zip, Type, Rating string
}

// LoadHospitals reads the filtered Hospital General Information file.
func LoadHospitals(path string) (map[string]Hospital, error) {
	r, c, closeFn, err := reference.OpenGzCSV(path)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	col := func(names ...string) int { return c.Find(names...) }
	ccn, name, addr, city, st, zip := col("facility_id", "provider_id", "ccn"), col("facility_name", "hospital_name"),
		col("address"), col("citytown", "city"), col("state"), col("zip_code")
	typ, rating := col("hospital_type"), col("hospital_overall_rating")
	if ccn < 0 || name < 0 || addr < 0 || zip < 0 {
		return nil, errors.New("hospital general information: expected columns missing")
	}
	out := map[string]Hospital{}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		g := func(i int) string {
			if i < 0 || i >= len(rec) {
				return ""
			}
			return rec[i]
		}
		h := Hospital{g(ccn), g(name), g(addr), g(city), g(st), zip5(g(zip)), g(typ), g(rating)}
		out[h.CCN] = h
	}
}

type Provider struct {
	NPI, EntityType, Name, Address, City, State, Zip, Taxonomy string
}

// LoadProviders reads NPPES-format files (bulk NPPES or NPI Registry lookups); later files
// do not override earlier ones. Missing files are skipped.
func LoadProviders(paths ...string) (map[string]Provider, error) {
	out := map[string]Provider{}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		m, err := loadProviderFile(p)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			if _, ok := out[k]; !ok {
				out[k] = v
			}
		}
	}
	return out, nil
}

// LoadDACProviders reads the filtered Doctors and Clinicians file as individual providers.
func LoadDACProviders(path string, into map[string]Provider) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	r, c, closeFn, err := reference.OpenGzCSV(path)
	if err != nil {
		return err
	}
	defer closeFn()
	npi, first, last := c.Find("npi"), c.Find("provider_first_name", "frst_nm"), c.Find("provider_last_name", "lst_nm")
	addr, city, st, zip := c.Find("adr_ln_1", "address_line_1"), c.Find("citytown", "cty"), c.Find("state", "st"), c.Find("zip_code", "zip")
	spec := c.Find("pri_spec", "primary_specialty")
	if npi < 0 || st < 0 {
		return errors.New("doctors and clinicians file: expected columns missing")
	}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		g := func(i int) string {
			if i < 0 || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}
		if _, ok := into[g(npi)]; ok {
			continue
		}
		into[g(npi)] = Provider{NPI: g(npi), EntityType: "1", Name: strings.TrimSpace(g(first) + " " + g(last)),
			Address: g(addr), City: g(city), State: strings.ToUpper(g(st)), Zip: zip5(g(zip)), Taxonomy: g(spec)}
	}
}

func loadProviderFile(path string) (map[string]Provider, error) {
	r, c, closeFn, err := reference.OpenGzCSV(path)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	idx := []int{c.Find("npi"), c.Find("entity_type"), c.Find("name"), c.Find("address"), c.Find("city"),
		c.Find("state"), c.Find("zip5"), c.Find("primary_taxonomy")}
	out := map[string]Provider{}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		p := Provider{rec[idx[0]], rec[idx[1]], rec[idx[2]], rec[idx[3]], rec[idx[4]], rec[idx[5]], rec[idx[6]], rec[idx[7]]}
		out[p.NPI] = p
	}
}

type Link struct {
	NPI, CCN, Method, Confidence string
	Score                        float64
}

var CrosswalkHeader = []string{"npi", "ccn", "method", "confidence", "score", "npi_name", "hospital_name"}

// BuildCrosswalk links NPIs to hospital CCNs.
//   - address (high): an organization NPI in the same ZIP with the same street number and name.
//   - name_zip (medium): an organization NPI in the same ZIP whose name shares at least half its words.
//   - affiliation (low): a clinician NPI that CMS lists as affiliated with the hospital.
func BuildCrosswalk(providers map[string]Provider, hospitals map[string]Hospital, affiliations map[string][]string) []Link {
	byZip := map[string][]Hospital{}
	for _, ccn := range reference.SortedKeys(hospitals) {
		h := hospitals[ccn]
		byZip[h.Zip] = append(byZip[h.Zip], h)
	}
	var links []Link
	for _, npi := range reference.SortedKeys(providers) {
		p := providers[npi]
		if p.EntityType == "2" {
			var best *Link
			for _, h := range byZip[p.Zip] {
				j := Jaccard(p.Name, h.Name)
				l := Link{NPI: npi, CCN: h.CCN, Score: j}
				if ak := AddressKey(p.Address); ak != "" && ak == AddressKey(h.Address) && j > 0 {
					l.Method, l.Confidence = "address", "high"
				} else if j >= 0.5 {
					l.Method, l.Confidence = "name_zip", "medium"
				} else {
					continue
				}
				if best == nil || rank(l) > rank(*best) || (rank(l) == rank(*best) && l.Score > best.Score) {
					c := l
					best = &c
				}
			}
			if best != nil {
				links = append(links, *best)
				continue
			}
		}
		if ccns := affiliations[npi]; len(ccns) > 0 {
			for _, ccn := range ccns {
				if _, ok := hospitals[ccn]; ok {
					links = append(links, Link{NPI: npi, CCN: ccn, Method: "affiliation", Confidence: "low"})
				}
			}
		}
	}
	return links
}

func rank(l Link) int {
	switch l.Method {
	case "address":
		return 2
	case "name_zip":
		return 1
	}
	return 0
}

// LoadAffiliations reads the filtered facility-affiliation file: NPI -> hospital CCNs.
func LoadAffiliations(path string) (map[string][]string, error) {
	r, c, closeFn, err := reference.OpenGzCSV(path)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	npiCol := c.Find("npi")
	ccnCol := c.Find("facility_afl_ccn", "facility_affiliations_certification_number")
	typeCol := c.Find("facility_type")
	if npiCol < 0 || ccnCol < 0 {
		return nil, errors.New("facility affiliations: expected columns missing")
	}
	out := map[string][]string{}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if typeCol >= 0 && !strings.Contains(strings.ToLower(rec[typeCol]), "hospital") {
			continue
		}
		out[rec[npiCol]] = append(out[rec[npiCol]], rec[ccnCol])
	}
	for k, v := range out {
		sort.Strings(v)
		out[k] = dedupe(v)
	}
	return out, nil
}

func dedupe(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func WriteCrosswalk(path string, links []Link, providers map[string]Provider, hospitals map[string]Hospital) error {
	w, err := reference.CreateGzCSV(path, CrosswalkHeader)
	if err != nil {
		return err
	}
	for _, l := range links {
		w.Write([]string{l.NPI, l.CCN, l.Method, l.Confidence, strconv.FormatFloat(l.Score, 'f', 2, 64),
			providers[l.NPI].Name, hospitals[l.CCN].Name})
	}
	return w.Close()
}

// ---------- measures ----------

var MeasureHeader = []string{"ccn", "dataset", "measure_id", "measure_name", "score", "compared_to_national"}

// AppendMeasures copies one Care Compare measure file into long format.
func AppendMeasures(w *reference.GzCSV, path, dataset string, keep map[string]bool) (int, error) {
	r, c, closeFn, err := reference.OpenGzCSV(path)
	if err != nil {
		return 0, err
	}
	defer closeFn()
	ccn := c.Find("facility_id", "provider_id")
	id := c.Find("measure_id", "hcahps_measure_id")
	name := c.Find("measure_name", "hcahps_question")
	scoreCols := []int{c.Find("score"), c.Find("patient_survey_star_rating"), c.Find("hcahps_answer_percent"), c.Find("hcahps_linear_mean_value")}
	cmp := c.Find("compared_to_national")
	if ccn < 0 || id < 0 {
		return 0, fmt.Errorf("%s: expected facility and measure id columns", dataset)
	}
	n := 0
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		g := func(i int) string {
			if i < 0 || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}
		if keep != nil && !keep[g(ccn)] {
			continue
		}
		score := ""
		for _, sc := range scoreCols {
			if v := g(sc); v != "" && !strings.EqualFold(v, "not applicable") && !strings.EqualFold(v, "not available") {
				score = v
				break
			}
		}
		w.Write([]string{g(ccn), dataset, g(id), g(name), score, g(cmp)})
		n++
	}
}

// ---------- price join ----------

var PriceHeader = []string{"billing_code_type", "billing_code", "label", "negotiated_rate", "negotiated_type",
	"billing_class", "setting", "placeholder", "npi", "provider_name", "entity_type", "city", "state", "zip5",
	"taxonomy", "tin_value", "business_name", "ccn", "match_method", "hospital_name", "hospital_overall_rating", "source_dir"}

// CodeStats summarizes one billing code for one billing class (professional fees and facility
// fees are different prices). Min, median and max use dollar rates only; percentage-of-charges
// and per diem rows are counted but left out of the price statistics.
type CodeStats struct {
	Code               string  `json:"code"`
	Label              string  `json:"label"`
	BillingClass       string  `json:"billing_class"`
	Rows               int     `json:"rows"`
	DollarRows         int     `json:"dollar_rows"`
	OtherTypeRows      int     `json:"other_type_rows"`
	Providers          int     `json:"providers"`
	ProvidersWithCCN   int     `json:"providers_linked_to_hospital"`
	ProvidersWithStars int     `json:"providers_with_star_rating"`
	Min                float64 `json:"min_rate"`
	Median             float64 `json:"median_rate"`
	Max                float64 `json:"max_rate"`
	rates              []float64
	providers          map[string]int // npi -> 0 none, 1 ccn, 2 ccn+stars
}

type Summary struct {
	PayerID          string       `json:"payer_id"`
	Files            int          `json:"files"`
	RateRowsIn       int          `json:"rate_rows_in"`
	RowsInStates     int          `json:"rows_in_states"`
	Providers        int          `json:"providers"`
	ProvidersWithCCN int          `json:"providers_linked_to_hospital"`
	Hospitals        int          `json:"hospitals"`
	Codes            []*CodeStats `json:"codes"`
}

type group struct{ npi, tinValue, business string }

// JoinPrices joins every extracted file under ratesDir to providers (state filter), the
// crosswalk and hospital ratings, writing prices.csv.gz and summary.json into outDir.
func JoinPrices(payerID, ratesDir, outDir string, providers map[string]Provider, hospitals map[string]Hospital,
	links []Link, labels map[string]string) (*Summary, error) {
	best := map[string]Link{}
	for _, l := range links {
		if cur, ok := best[l.NPI]; !ok || rank(l) > rank(cur) {
			best[l.NPI] = l
		}
	}
	entries, err := os.ReadDir(ratesDir)
	if err != nil {
		return nil, err
	}
	w, err := reference.CreateGzCSV(filepath.Join(outDir, "prices.csv.gz"), PriceHeader)
	if err != nil {
		return nil, err
	}
	sum := &Summary{PayerID: payerID}
	stats := map[string]*CodeStats{}
	allProviders := map[string]bool{}
	allHospitals := map[string]bool{}
	for _, e := range entries {
		dir := filepath.Join(ratesDir, e.Name())
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "rates.csv.gz")); err != nil {
			continue
		}
		sum.Files++
		groups, err := loadGroups(filepath.Join(dir, "groups.csv.gz"))
		if err != nil {
			w.Close()
			return nil, err
		}
		r, c, closeFn, err := reference.OpenGzCSV(filepath.Join(dir, "rates.csv.gz"))
		if err != nil {
			w.Close()
			return nil, err
		}
		ix := func(n string) int { return c.Find(n) }
		ct, code, rate, ntype, bclass, setting, ph, pg := ix("billing_code_type"), ix("billing_code"), ix("negotiated_rate"),
			ix("negotiated_type"), ix("billing_class"), ix("setting"), ix("placeholder"), ix("provider_group")
		for {
			rec, err := r.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				closeFn()
				w.Close()
				return nil, err
			}
			sum.RateRowsIn++
			key := extract.CodeKey(rec[ct], rec[code])
			class := strings.ToLower(rec[bclass])
			dollar := isDollarType(rec[ntype])
			for _, g := range groups[rec[pg]] {
				p, ok := providers[g.npi]
				if !ok {
					continue // outside the configured states, or not an active NPI
				}
				l := best[g.npi]
				h := hospitals[l.CCN]
				w.Write([]string{rec[ct], rec[code], labels[key], rec[rate], rec[ntype], rec[bclass], rec[setting], rec[ph],
					p.NPI, p.Name, p.EntityType, p.City, p.State, p.Zip, p.Taxonomy, g.tinValue, g.business,
					l.CCN, l.Method, h.Name, h.Rating, e.Name()})
				sum.RowsInStates++
				allProviders[p.NPI] = true
				if l.CCN != "" {
					allHospitals[l.CCN] = true
				}
				st := stats[key+"|"+class]
				if st == nil {
					st = &CodeStats{Code: key, Label: labels[key], BillingClass: class, providers: map[string]int{}}
					stats[key+"|"+class] = st
				}
				st.Rows++
				if !dollar {
					st.OtherTypeRows++
				}
				level := 0
				if l.CCN != "" {
					level = 1
					if _, err := strconv.Atoi(h.Rating); err == nil {
						level = 2
					}
				}
				if level > st.providers[p.NPI] || st.providers[p.NPI] == 0 {
					st.providers[p.NPI] = level
				}
				if f, err := strconv.ParseFloat(rec[rate], 64); err == nil && rec[ph] != "1" && dollar {
					st.rates = append(st.rates, f)
					st.DollarRows++
				}
			}
		}
		closeFn()
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	sum.Providers = len(allProviders)
	sum.Hospitals = len(allHospitals)
	for npi := range allProviders {
		if best[npi].CCN != "" {
			sum.ProvidersWithCCN++
		}
	}
	for _, k := range reference.SortedKeys(stats) {
		st := stats[k]
		st.Providers = len(st.providers)
		for _, lv := range st.providers {
			if lv >= 1 {
				st.ProvidersWithCCN++
			}
			if lv == 2 {
				st.ProvidersWithStars++
			}
		}
		if len(st.rates) > 0 {
			sort.Float64s(st.rates)
			st.Min, st.Max = st.rates[0], st.rates[len(st.rates)-1]
			m := len(st.rates) / 2
			st.Median = st.rates[m]
			if len(st.rates)%2 == 0 {
				st.Median = (st.rates[m-1] + st.rates[m]) / 2
			}
		}
		sum.Codes = append(sum.Codes, st)
	}
	b, _ := json.MarshalIndent(sum, "", "  ")
	return sum, os.WriteFile(filepath.Join(outDir, "summary.json"), append(b, '\n'), 0o644)
}

// isDollarType reports whether a negotiated_type is a dollar amount. "percentage" is a percent
// of billed charges and "per diem" is a daily rate, so neither compares with a case price.
func isDollarType(t string) bool {
	switch strings.ToLower(t) {
	case "negotiated", "fee schedule", "derived":
		return true
	}
	return false
}

func loadGroups(path string) (map[string][]group, error) {
	r, c, closeFn, err := reference.OpenGzCSV(path)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	k, n, tv, bn := c.Find("provider_group"), c.Find("npi"), c.Find("tin_value"), c.Find("business_name")
	out := map[string][]group{}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out[rec[k]] = append(out[rec[k]], group{rec[n], rec[tv], rec[bn]})
	}
}

// InstitutionalNPIs returns the NPIs (sorted) in provider groups used by institutional rates,
// i.e. the facilities whose identity the crosswalk needs.
func InstitutionalNPIs(ratesDir string) ([]string, error) {
	entries, err := os.ReadDir(ratesDir)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, e := range entries {
		dir := filepath.Join(ratesDir, e.Name())
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "rates.csv.gz")); err != nil {
			continue
		}
		groups, err := loadGroups(filepath.Join(dir, "groups.csv.gz"))
		if err != nil {
			return nil, err
		}
		r, c, closeFn, err := reference.OpenGzCSV(filepath.Join(dir, "rates.csv.gz"))
		if err != nil {
			return nil, err
		}
		bc, pg := c.Find("billing_class"), c.Find("provider_group")
		used := map[string]bool{}
		for {
			rec, err := r.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				closeFn()
				return nil, err
			}
			if strings.EqualFold(rec[bc], "institutional") {
				used[rec[pg]] = true
			}
		}
		closeFn()
		for k := range used {
			for _, g := range groups[k] {
				if g.npi != "" {
					set[g.npi] = true
				}
			}
		}
	}
	return reference.SortedKeys(set), nil
}
