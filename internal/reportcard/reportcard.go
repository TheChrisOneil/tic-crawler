// Package reportcard compares a payer's negotiated hospital facility rates with Medicare.
//
// For each hospital (an NPI linked to a CMS certification number by the quality crosswalk at
// high or medium confidence) and each billing code with a Medicare outpatient or lab
// benchmark, it reports the negotiated facility rates as multiples of Medicare.
//
// It is a price benchmark only, not a quality rating. Rules, all from the rate files as
// published:
//   - Institutional (facility) rows only; professional fees have a different benchmark.
//   - Usable dollar rates only: rows flagged placeholder, not_dollar (percentage, per diem),
//     expired or non_ffs (bundles, capitation) are counted and left out.
//   - Deduplicated by tax ID: one (file, TIN, rate) counts once however many NPIs share it, so
//     big groups do not outweigh small ones.
//   - Rates under MinMultiple or over MaxMultiple times Medicare are counted as outliers and
//     left out of the statistics; they are almost always data errors.
package reportcard

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/TheChrisOneil/tic-crawler/internal/extract"
	"github.com/TheChrisOneil/tic-crawler/internal/medicare"
	"github.com/TheChrisOneil/tic-crawler/internal/reference"
)

const (
	MinMultiple = 0.3
	MaxMultiple = 20
)

// Hospital is the slice of CMS Hospital General Information the card shows.
type Hospital struct {
	CCN, Name, City, State string
}

// Link is a crosswalk row: an NPI that belongs to a hospital.
type Link struct {
	NPI, CCN, Confidence string
}

type Cell struct {
	Median   float64            `json:"median"`
	Min      float64            `json:"min"`
	Max      float64            `json:"max"`
	Multiple float64            `json:"multiple"`
	Rates    int                `json:"rates"` // distinct (file, TIN, rate) values used
	Outliers int                `json:"outliers,omitempty"`
	ByFile   map[string]float64 `json:"by_file"` // file_id -> median multiple
}

type HospitalCard struct {
	CCN   string           `json:"ccn"`
	Name  string           `json:"name"`
	City  string           `json:"city"`
	State string           `json:"state"`
	Cells map[string]*Cell `json:"cells"` // code key -> cell
	// FlatRate is set when FlatCodes (4 or more) different services share the exact same median
	// rate: usually a default or case rate, not a price for each service. Shown as a warning.
	FlatRate  float64 `json:"flat_rate,omitempty"`
	FlatCodes int     `json:"flat_codes,omitempty"`
}

// FlatRateMinCodes is how many services must share one median rate to flag a hospital.
const FlatRateMinCodes = 4

type CodeCard struct {
	Code           string  `json:"code"`
	Label          string  `json:"label"`
	MedicareCode   string  `json:"medicare_code"`
	MedicareSource string  `json:"medicare_source"` // opps | clfs
	MedicareAmount float64 `json:"medicare_amount"`
	Hospitals      int     `json:"hospitals"`
	MedianMultiple float64 `json:"median_multiple"`
	MinMultiple    float64 `json:"min_multiple"`
	MaxMultiple    float64 `json:"max_multiple"`
}

type FileCard struct {
	FileID        string   `json:"file_id"`
	PlanCount     int      `json:"plan_count"`
	PlanNames     []string `json:"plan_names"`
	Networks      []string `json:"networks"` // first 20, sorted
	NetworkCount  int      `json:"network_count"`
	LastUpdatedOn string   `json:"last_updated_on"`
}

type Card struct {
	PayerID      string          `json:"payer_id"`
	PayerName    string          `json:"payer_name"`
	GeneratedAt  string          `json:"generated_at"`
	Medicare     []string        `json:"medicare_releases"`
	Rules        map[string]any  `json:"rules"`
	Codes        []*CodeCard     `json:"codes"`
	Files        []*FileCard     `json:"files"`
	Hospitals    []*HospitalCard `json:"hospitals"`
	Excluded     map[string]int  `json:"excluded_rows"`
	NoBenchmark  []string        `json:"codes_without_benchmark"`
	HospitalsAll int             `json:"hospitals_linked"`
}

type fileIndex struct {
	Files []struct {
		Dir       string   `json:"dir"`
		PlanCount int      `json:"plan_count"`
		PlanNames []string `json:"plan_names"`
		Meta      *struct {
			ReportingEntityName string `json:"reporting_entity_name"`
			LastUpdatedOn       string `json:"last_updated_on"`
		} `json:"meta"`
	} `json:"files"`
}

type grp struct{ npi, tin string }

func loadGroups(path string) (map[string][]grp, map[string]bool, error) {
	r, c, closeFn, err := reference.OpenGzCSV(path)
	if err != nil {
		return nil, nil, err
	}
	defer closeFn()
	k, n, tt, tv, nn := c.Find("provider_group"), c.Find("npi"), c.Find("tin_type"), c.Find("tin_value"), c.Find("network_name")
	out := map[string][]grp{}
	nets := map[string]bool{}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nets, nil
		}
		if err != nil {
			return nil, nil, err
		}
		tin := strings.ToLower(rec[tt]) + ":" + digits(rec[tv])
		out[rec[k]] = append(out[rec[k]], grp{rec[n], tin})
		if nn >= 0 {
			for _, x := range strings.Split(rec[nn], "|") {
				if x = strings.TrimSpace(x); x != "" {
					nets[x] = true
				}
			}
		}
	}
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64{}, v...)
	sort.Float64s(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// Build reads extract-rates output for one payer (ratesDir/<file_id>/rates.csv.gz,
// groups.csv.gz and ratesDir/index.json) and returns its report card.
func Build(payerID, ratesDir string, links []Link, hospitals map[string]Hospital,
	bm map[string]map[string]medicare.Benchmark, labels map[string]string, now string) (*Card, error) {
	hosp := map[string]string{} // npi -> ccn
	for _, l := range links {
		if l.Confidence == "high" || l.Confidence == "medium" {
			if _, ok := hospitals[l.CCN]; ok {
				hosp[l.NPI] = l.CCN
			}
		}
	}
	var idx fileIndex
	if b, err := os.ReadFile(filepath.Join(ratesDir, "index.json")); err == nil {
		json.Unmarshal(b, &idx)
	}
	card := &Card{PayerID: payerID, GeneratedAt: now, Excluded: map[string]int{},
		Rules: map[string]any{"billing_class": "institutional", "dedupe": "file, tax id and rate",
			"crosswalk_confidence": []string{"high", "medium"}, "min_multiple": MinMultiple, "max_multiple": MaxMultiple,
			"medicare": "OPPS national unadjusted payment rate (labs: CLFS national limit); not wage-adjusted"}}
	type key struct{ ccn, code string }
	type rateKey struct {
		ccn, code, file, tin string
		rate                 float64
	}
	seen := map[rateKey]bool{}
	vals := map[key][]float64{}
	byFile := map[key]map[string][]float64{}
	outliers := map[key]int{}
	noBench := map[string]bool{}
	releases := map[string]bool{}
	bench := map[string]medicare.Benchmark{}
	benchCode := map[string]string{}
	type fileInfo struct {
		Dir       string
		PlanCount int
		PlanNames []string
		Entity    string
		Updated   string
	}
	info := map[string]fileInfo{}
	for _, f := range idx.Files {
		if f.Dir == "" {
			continue
		}
		fi := fileInfo{Dir: f.Dir, PlanCount: f.PlanCount, PlanNames: f.PlanNames}
		if f.Meta != nil {
			fi.Entity, fi.Updated = f.Meta.ReportingEntityName, f.Meta.LastUpdatedOn
		}
		info[f.Dir] = fi
	}
	// Every extracted file folder counts, including ones from earlier runs that a later
	// index.json no longer lists; meta.json fills in what the index lacks.
	entries, err := os.ReadDir(ratesDir)
	if err != nil {
		return nil, err
	}
	var files []fileInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(ratesDir, e.Name(), "rates.csv.gz")); err != nil {
			continue
		}
		fi, ok := info[e.Name()]
		if !ok || fi.Entity == "" {
			var m struct {
				ReportingEntityName string            `json:"reporting_entity_name"`
				LastUpdatedOn       string            `json:"last_updated_on"`
				Plan                map[string]string `json:"plan"`
			}
			if b, err := os.ReadFile(filepath.Join(ratesDir, e.Name(), "meta.json")); err == nil {
				json.Unmarshal(b, &m)
			}
			fi.Dir, fi.Entity, fi.Updated = e.Name(), m.ReportingEntityName, m.LastUpdatedOn
			if len(fi.PlanNames) == 0 && m.Plan["plan_name"] != "" {
				fi.PlanNames, fi.PlanCount = []string{m.Plan["plan_name"]}, 1
			}
		}
		files = append(files, fi)
	}
	for _, f := range files {
		dir := filepath.Join(ratesDir, f.Dir)
		groups, nets, err := loadGroups(filepath.Join(dir, "groups.csv.gz"))
		if err != nil {
			return nil, err
		}
		fc := &FileCard{FileID: f.Dir, PlanCount: f.PlanCount, PlanNames: append([]string{}, f.PlanNames...), Networks: []string{}}
		if len(fc.PlanNames) > 10 {
			fc.PlanNames = fc.PlanNames[:10]
		}
		for n := range nets {
			fc.Networks = append(fc.Networks, n)
		}
		sort.Strings(fc.Networks)
		fc.NetworkCount = len(fc.Networks)
		if len(fc.Networks) > 20 {
			fc.Networks = fc.Networks[:20]
		}
		fc.LastUpdatedOn = f.Updated
		if card.PayerName == "" {
			card.PayerName = f.Entity
		}
		card.Files = append(card.Files, fc)
		r, c, closeFn, err := reference.OpenGzCSV(filepath.Join(dir, "rates.csv.gz"))
		if err != nil {
			return nil, err
		}
		ct, code, rate, ntype, bclass, exp, arr, pg, flg := c.Find("billing_code_type"), c.Find("billing_code"),
			c.Find("negotiated_rate"), c.Find("negotiated_type"), c.Find("billing_class"), c.Find("expiration_date"),
			c.Find("negotiation_arrangement"), c.Find("provider_group"), c.Find("flags")
		for {
			rec, err := r.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				closeFn()
				return nil, err
			}
			if !strings.EqualFold(rec[bclass], "institutional") {
				continue
			}
			ck := extract.CodeKey(rec[ct], rec[code])
			if !strings.HasPrefix(ck, "HCPCS:") {
				noBench[ck] = true // inpatient MS-DRG: no outpatient benchmark
				continue
			}
			var flags []string
			if flg >= 0 {
				if rec[flg] != "" {
					flags = strings.Split(rec[flg], "|")
				}
			} else { // files extracted before the flags column existed
				flags = extract.RowFlags(rec[rate], rec[ntype], rec[exp], rec[arr], now[:min(10, len(now))])
			}
			if len(flags) > 0 {
				card.Excluded[flags[0]]++
				continue
			}
			b, ok := bench[ck]
			if !ok {
				var mc string
				b, mc, ok = medicare.Facility(bm, strings.TrimPrefix(ck, "HCPCS:"))
				if !ok {
					noBench[ck] = true
					continue
				}
				bench[ck], benchCode[ck] = b, mc
				releases[b.Release] = true
			}
			v, _ := strconv.ParseFloat(rec[rate], 64)
			for _, g := range groups[rec[pg]] {
				ccn, ok := hosp[g.npi]
				if !ok {
					continue
				}
				rk := rateKey{ccn, ck, f.Dir, g.tin, v}
				if seen[rk] {
					continue
				}
				seen[rk] = true
				k := key{ccn, ck}
				m := v / b.Amount
				if m < MinMultiple || m > MaxMultiple {
					outliers[k]++
					card.Excluded["outlier_vs_medicare"]++
					continue
				}
				vals[k] = append(vals[k], v)
				if byFile[k] == nil {
					byFile[k] = map[string][]float64{}
				}
				byFile[k][f.Dir] = append(byFile[k][f.Dir], v)
			}
		}
		closeFn()
	}
	cards := map[string]*HospitalCard{}
	perCode := map[string][]float64{}
	for k, v := range vals {
		h := cards[k.ccn]
		if h == nil {
			hs := hospitals[k.ccn]
			h = &HospitalCard{CCN: k.ccn, Name: hs.Name, City: hs.City, State: hs.State, Cells: map[string]*Cell{}}
			cards[k.ccn] = h
		}
		amt := bench[k.code].Amount
		sort.Float64s(v)
		med := median(v)
		cell := &Cell{Median: round(med, 2), Min: v[0], Max: v[len(v)-1], Multiple: round(med/amt, 2), Rates: len(v),
			Outliers: outliers[k], ByFile: map[string]float64{}}
		for fid, fv := range byFile[k] {
			cell.ByFile[fid] = round(median(fv)/amt, 2)
		}
		h.Cells[k.code] = cell
		perCode[k.code] = append(perCode[k.code], med/amt)
	}
	for _, h := range cards {
		same := map[float64]int{}
		for _, cl := range h.Cells {
			same[cl.Median]++
		}
		for v, n := range same {
			if n >= FlatRateMinCodes && (n > h.FlatCodes || (n == h.FlatCodes && v > h.FlatRate)) {
				h.FlatRate, h.FlatCodes = v, n
			}
		}
		card.Hospitals = append(card.Hospitals, h)
	}
	sort.Slice(card.Hospitals, func(i, j int) bool {
		a, b := card.Hospitals[i], card.Hospitals[j]
		if a.State != b.State {
			return a.State < b.State
		}
		return a.Name < b.Name
	})
	codes := make([]string, 0, len(perCode))
	for c := range perCode {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		ms := perCode[c]
		sort.Float64s(ms)
		b := bench[c]
		card.Codes = append(card.Codes, &CodeCard{Code: c, Label: labels[c], MedicareCode: benchCode[c], MedicareSource: b.Source,
			MedicareAmount: b.Amount, Hospitals: len(ms), MedianMultiple: round(median(ms), 2),
			MinMultiple: round(ms[0], 2), MaxMultiple: round(ms[len(ms)-1], 2)})
	}
	for c := range noBench {
		if _, ok := perCode[c]; !ok {
			card.NoBenchmark = append(card.NoBenchmark, c)
		}
	}
	sort.Strings(card.NoBenchmark)
	for r := range releases {
		card.Medicare = append(card.Medicare, r)
	}
	sort.Strings(card.Medicare)
	linked := map[string]bool{}
	for _, c := range hosp {
		linked[c] = true
	}
	card.HospitalsAll = len(linked)
	return card, nil
}

// LoadLinks reads the quality crosswalk (crosswalk.csv.gz).
func LoadLinks(path string) ([]Link, error) {
	r, c, closeFn, err := reference.OpenGzCSV(path)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	n, cc, conf := c.Find("npi"), c.Find("ccn"), c.Find("confidence")
	if n < 0 || cc < 0 || conf < 0 {
		return nil, errors.New("crosswalk: expected npi, ccn and confidence columns")
	}
	var out []Link
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Link{NPI: rec[n], CCN: rec[cc], Confidence: rec[conf]})
	}
}

// Write stores the card as JSON.
func Write(path string, c *Card) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
