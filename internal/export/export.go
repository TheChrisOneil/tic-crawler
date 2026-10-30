// Package export writes compact JSON for the dashboard (dashboard/ reads out/dashboard/).
// The JSON shape is shared with dashboard/app.js; change both together.
package export

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/TheChrisOneil/tic-crawler/internal/config"
	"github.com/TheChrisOneil/tic-crawler/internal/model"
	"github.com/TheChrisOneil/tic-crawler/internal/state"
	"github.com/TheChrisOneil/tic-crawler/internal/toc"
)

const PlanCap = 5000

var rowFields = []string{"file_type", "url", "description", "plan_count", "plan_names", "plans_truncated",
	"plan_id_types", "plan_market_types", "http_status", "check_method", "content_length", "last_modified", "etag", "check_error"}

// Row is one manifest row; field order matches the Python export.
type Row struct {
	FileType        string `json:"file_type"`
	URL             string `json:"url"`
	Description     string `json:"description"`
	PlanCount       int    `json:"plan_count"`
	PlanNames       string `json:"plan_names"`
	PlansTruncated  bool   `json:"plans_truncated"`
	PlanIDTypes     string `json:"plan_id_types"`
	PlanMarketTypes string `json:"plan_market_types"`
	HTTPStatus      *int   `json:"http_status"`
	CheckMethod     string `json:"check_method"`
	ContentLength   *int64 `json:"content_length"`
	LastModified    string `json:"last_modified"`
	ETag            string `json:"etag"`
	CheckError      string `json:"check_error"`
}

func latest(dir, pattern string) string {
	m, _ := filepath.Glob(filepath.Join(dir, pattern))
	sort.Strings(m)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}

func ManifestRows(path string) ([]Row, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.FieldsPerRecord = -1
	h, err := cr.Read()
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i, c := range h {
		idx[c] = i
	}
	rows := []Row{}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		g := func(k string) string {
			if i, ok := idx[k]; ok && i < len(rec) {
				return rec[i]
			}
			return ""
		}
		if g("url") == "" && g("check_error") == "" {
			continue
		}
		r := Row{FileType: g("file_type"), URL: g("url"), Description: g("description"), PlanNames: g("plan_names"),
			PlansTruncated: g("plans_truncated") == "True", PlanIDTypes: g("plan_id_types"), PlanMarketTypes: g("plan_market_types"),
			CheckMethod: g("check_method"), LastModified: g("last_modified"), ETag: g("etag"), CheckError: g("check_error")}
		r.PlanCount, _ = strconv.Atoi(g("plan_count"))
		if n, err := strconv.Atoi(g("http_status")); err == nil {
			r.HTTPStatus = &n
		}
		if n, err := strconv.ParseInt(g("content_length"), 10, 64); err == nil {
			r.ContentLength = &n
		}
		rows = append(rows, r)
	}
}

type Plan struct {
	PlanName           any `json:"plan_name"`
	PlanIDType         any `json:"plan_id_type"`
	PlanID             any `json:"plan_id"`
	PlanMarketType     any `json:"plan_market_type"`
	IssuerName         any `json:"issuer_name"`
	PlanSponsorName    any `json:"plan_sponsor_name"`
	InNetworkFiles     int `json:"in_network_files"`
	AllowedAmountFiles int `json:"allowed_amount_files"`
}

type TocPlans struct {
	Plans     []*Plan `json:"plans"`
	PlanRefs  int     `json:"plan_refs"`
	Truncated bool    `json:"truncated"`
	Cap       int     `json:"cap"`
}

// TOCPlans lists distinct plans in a stored TOC with the number of files that reference each.
func TOCPlans(path string, cap int) (*TocPlans, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := toc.MaybeGunzip(f)
	if err != nil {
		return nil, err
	}
	out := &TocPlans{Plans: []*Plan{}, Cap: cap}
	index := map[string]*Plan{}
	_, err = toc.ReadGeneric(r, func(_ int, item any) error {
		m, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		nIn := 0
		if l, ok := m["in_network_files"].([]any); ok {
			nIn = len(l)
		}
		_, hasAA := m["allowed_amount_file"].(map[string]any)
		plans, _ := m["reporting_plans"].([]any)
		for _, pv := range plans {
			p, ok := pv.(map[string]any)
			if !ok {
				continue
			}
			out.PlanRefs++
			key := fmt.Sprintf("%v\x00%v\x00%v", p["plan_id_type"], p["plan_id"], p["plan_name"])
			e := index[key]
			if e == nil {
				if len(index) >= cap {
					continue
				}
				e = &Plan{PlanName: p["plan_name"], PlanIDType: p["plan_id_type"], PlanID: p["plan_id"],
					PlanMarketType: p["plan_market_type"], IssuerName: p["issuer_name"], PlanSponsorName: p["plan_sponsor_name"]}
				index[key] = e
				out.Plans = append(out.Plans, e)
			}
			e.InNetworkFiles += nIn
			if hasAA {
				e.AllowedAmountFiles++
			}
		}
		return nil
	})
	out.Truncated = len(index) >= cap
	return out, err
}

type PayerRow struct {
	PayerID          string         `json:"payer_id"`
	LegalName        string         `json:"legal_name"`
	Kind             string         `json:"kind"`
	States           []string       `json:"states"`
	Cohorts          []string       `json:"cohorts"`
	Priority         int            `json:"priority"`
	LandingURL       string         `json:"landing_url"`
	TocURL           string         `json:"toc_url"`
	HasTocSource     bool           `json:"has_toc_source"`
	Status           string         `json:"status"`
	AccessStatus     string         `json:"access_status"`
	LastCrawled      string         `json:"last_crawled"`
	TocLastUpdatedOn string         `json:"toc_last_updated_on"`
	AgeDays          *int           `json:"age_days"`
	Stale            *bool          `json:"stale"`
	MrfCount         int64          `json:"mrf_count"`
	TotalBytes       int64          `json:"total_bytes"`
	TocBytes         int64          `json:"toc_bytes"`
	SchemaValid      any            `json:"schema_valid"`
	ValidationErrors int64          `json:"validation_errors"`
	InvalidItems     int64          `json:"invalid_items"`
	Items            int64          `json:"items"`
	URLStatus        map[string]int `json:"url_status"`
	Error            string         `json:"error"`
	FileVersion      string         `json:"file_version"`
}

func s(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func n(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case json.Number:
		i, _ := v.Int64()
		return i
	}
	return 0
}

func orEmpty(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func normPayer(p model.Payer) model.Payer {
	p.HIOSIDs, p.States, p.Aliases = orEmpty(p.HIOSIDs), orEmpty(p.States), orEmpty(p.Aliases)
	p.Sources, p.Cohorts = orEmpty(p.Sources), orEmpty(p.Cohorts)
	return p
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// Build writes out/dashboard/data.json and out/dashboard/payers/<id>.json.
func Build(cfg config.Settings, st *state.Store, payers map[string]model.Payer, today time.Time) (string, error) {
	out := cfg.LocalDir
	dest := filepath.Join(out, "dashboard")
	if err := os.MkdirAll(filepath.Join(dest, "payers"), 0o755); err != nil {
		return "", err
	}
	all, err := st.AllLatest()
	if err != nil {
		return "", err
	}
	staleDays := cfg.StaleDays(today)
	ids := make([]string, 0, len(payers))
	for id := range payers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := []PayerRow{}
	access, records := []map[string]any{}, []map[string]any{}
	for _, pid := range ids {
		p := normPayer(payers[pid])
		r := all[pid]
		if r == nil {
			r = map[string]any{}
		}
		v, _ := r["validation"].(map[string]any)
		if v == nil {
			v = map[string]any{}
		}
		lu := s(r, "toc_last_updated_on")
		var age *int
		if lu != "" {
			if d, err := time.Parse("2006-01-02", lu); err == nil {
				a := int(today.Sub(d).Hours() / 24)
				age = &a
			}
		}
		man := latest(filepath.Join(out, "manifests", pid), "*.csv")
		mrows := []Row{}
		if man != "" {
			if mrows, err = ManifestRows(man); err != nil {
				return "", err
			}
		}
		statusCounts := map[string]int{}
		for _, x := range mrows {
			k := "none"
			if x.HTTPStatus != nil && *x.HTTPStatus != 0 {
				k = strconv.Itoa(*x.HTTPStatus)
			} else if x.CheckError != "" {
				k = x.CheckError
			}
			statusCounts[k]++
		}
		manRel := ""
		if man != "" {
			manRel, _ = filepath.Rel(out, man)
			manRel = filepath.ToSlash(manRel)
		}
		detail := map[string]any{"payer": p, "result": r, "manifest_file": manRel, "rows": mrows}
		if t := latest(filepath.Join(out, "toc", pid), "*.json.gz"); t != "" {
			if tp, err := TOCPlans(t, PlanCap); err != nil {
				detail["toc_plans"] = map[string]string{"error": fmt.Sprintf("%T: %v", err, err)}
			} else {
				detail["toc_plans"] = tp
			}
		}
		if err := writeJSON(filepath.Join(dest, "payers", pid+".json"), orderedDetail(detail)); err != nil {
			return "", err
		}
		status := s(r, "status")
		if status == "" {
			status = "never"
		}
		tocURL := s(r, "toc_url")
		if tocURL == "" {
			tocURL = p.TocURL
		}
		var stale *bool
		if age != nil {
			b := *age > staleDays
			stale = &b
		}
		row := PayerRow{PayerID: pid, LegalName: p.LegalName, Kind: p.Kind, States: p.States, Cohorts: p.Cohorts,
			Priority: p.Priority, LandingURL: p.LandingURL, TocURL: tocURL, HasTocSource: p.TocURL != "" || p.TocLinkPattern != "",
			Status: status, AccessStatus: s(r, "access_status"), LastCrawled: s(r, "crawled_at"), TocLastUpdatedOn: lu,
			AgeDays: age, Stale: stale, MrfCount: n(r, "mrf_count"), TotalBytes: n(r, "total_bytes"), TocBytes: n(r, "toc_bytes"),
			SchemaValid: r["schema_valid"], ValidationErrors: n(v, "error_count"), InvalidItems: n(v, "invalid_items"),
			Items: n(v, "item_count"), URLStatus: statusCounts, Error: s(r, "error"), FileVersion: s(v, "file_version")}
		rows = append(rows, row)
		if status == "failed" || status == "skipped" || status == "deferred" {
			u := tocURL
			if u == "" {
				u = p.LandingURL
			}
			access = append(access, map[string]any{"payer_id": pid, "level": "payer", "status": status,
				"access_status": s(r, "access_status"), "url": u, "error": s(r, "error")})
		}
		for _, x := range mrows {
			if x.CheckError != "" {
				access = append(access, map[string]any{"payer_id": pid, "level": "mrf", "status": x.HTTPStatus,
					"url": x.URL, "error": x.CheckError, "file_type": x.FileType})
			}
		}
		if kinds, ok := v["error_kinds"].([]any); ok {
			for _, k := range kinds {
				if km, ok := k.(map[string]any); ok {
					rec := map[string]any{"payer_id": pid}
					for kk, vv := range km {
						rec[kk] = vv
					}
					records = append(records, rec)
				}
			}
		}
	}
	var audit any
	if b, err := os.ReadFile(filepath.Join(out, "access_audit.json")); err == nil {
		json.Unmarshal(b, &audit)
	}
	totals := map[string]int64{"payers": int64(len(rows))}
	cohortSet := map[string]bool{}
	for _, r := range rows {
		if r.HasTocSource {
			totals["with_toc_source"]++
		}
		if r.Status != "never" {
			totals["crawled"]++
		}
		switch r.Status {
		case "ok", "unchanged":
			totals["ok"]++
		case "failed", "skipped":
			totals["failed"]++
		case "deferred":
			totals["deferred"]++
		}
		if r.Stale != nil && *r.Stale {
			totals["stale"]++
		}
		totals["mrf_urls"] += r.MrfCount
		totals["mrf_bytes"] += r.TotalBytes
		if b, ok := r.SchemaValid.(bool); ok && !b {
			totals["schema_invalid"]++
		}
		for _, c := range r.Cohorts {
			cohortSet[c] = true
		}
	}
	cohorts := []string{}
	for c := range cohortSet {
		cohorts = append(cohorts, c)
	}
	sort.Strings(cohorts)
	runs := []string{}
	if m, _ := filepath.Glob(filepath.Join(out, "runs", "*.json")); m != nil {
		for _, f := range m {
			runs = append(runs, filepath.Base(f))
		}
		sort.Strings(runs)
	}
	data := struct {
		GeneratedAt        string                      `json:"generated_at"`
		SchemaVersion      string                      `json:"schema_version"`
		StaleThresholdDays int                         `json:"stale_threshold_days"`
		Totals             orderedTotals               `json:"totals"`
		Cohorts            []string                    `json:"cohorts"`
		Payers             []PayerRow                  `json:"payers"`
		Errors             map[string][]map[string]any `json:"errors"`
		AccessAudit        any                         `json:"access_audit"`
		Runs               []string                    `json:"runs"`
	}{time.Now().UTC().Format("2006-01-02T15:04:05Z"), cfg.SchemaVersion, staleDays, orderedTotals(totals), cohorts, rows,
		map[string][]map[string]any{"access": access, "records": records}, audit, runs}
	return dest, writeJSON(filepath.Join(dest, "data.json"), data)
}

type orderedTotals map[string]int64

var totalsOrder = []string{"payers", "with_toc_source", "crawled", "ok", "failed", "deferred", "stale", "mrf_urls", "mrf_bytes", "schema_invalid"}

func (t orderedTotals) MarshalJSON() ([]byte, error) {
	b := []byte("{")
	for i, k := range totalsOrder {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, fmt.Sprintf("%q:%d", k, t[k])...)
	}
	return append(b, '}'), nil
}

// orderedDetail keeps the payer file's keys in the Python order: payer, result, manifest_file, rows, toc_plans.
func orderedDetail(d map[string]any) json.Marshaler { return detailJSON(d) }

type detailJSON map[string]any

func (d detailJSON) MarshalJSON() ([]byte, error) {
	b := []byte("{")
	first := true
	for _, k := range []string{"payer", "result", "manifest_file", "rows", "toc_plans"} {
		v, ok := d[k]
		if !ok {
			continue
		}
		vb, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		if !first {
			b = append(b, ',')
		}
		first = false
		b = append(b, fmt.Sprintf("%q:", k)...)
		b = append(b, vb...)
	}
	return append(b, '}'), nil
}
