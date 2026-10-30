// Package model holds plain data types shared across the crawler.
package model

import (
	"encoding/json"
	"strings"
	"time"
)

type Payer struct {
	PayerID        string   `json:"payer_id" yaml:"payer_id"`
	LegalName      string   `json:"legal_name" yaml:"legal_name"`
	HIOSIDs        []string `json:"hios_ids" yaml:"-"` // read as strings or numbers by the seed loader
	States         []string `json:"states" yaml:"states"`
	LandingURL     string   `json:"landing_url" yaml:"landing_url"`
	TocURL         string   `json:"toc_url" yaml:"toc_url"` // literal, or a pattern like .../{date:%Y-%m}-01_index.json
	TocLinkPattern string   `json:"toc_link_pattern" yaml:"toc_link_pattern"`
	TocFile        string   `json:"toc_file" yaml:"toc_file"` // TOC saved by hand for sites that block bots
	Kind           string   `json:"kind" yaml:"kind"`         // issuer | tpa
	Aliases        []string `json:"aliases" yaml:"aliases"`
	Sources        []string `json:"sources" yaml:"sources"`
	Notes          string   `json:"notes" yaml:"notes"`
	Cohorts        []string `json:"cohorts" yaml:"cohorts"`
	Priority       int      `json:"priority" yaml:"-"`
}

// Strftime supports the %Y, %m and %d fields used in TOC URL patterns.
func Strftime(layout string, t time.Time) string {
	return strings.NewReplacer("%Y", t.Format("2006"), "%m", t.Format("01"), "%d", t.Format("02"), "%%", "%").Replace(layout)
}

// RenderTocURLs expands a TOC URL pattern into candidates: this month first, then last month.
func RenderTocURLs(pattern string, today time.Time) []string {
	if pattern == "" {
		return nil
	}
	if !strings.Contains(pattern, "{") {
		return []string{pattern}
	}
	this := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	prev := this.AddDate(0, -1, 0)
	var out []string
	for _, d := range []time.Time{this, prev} {
		u := pattern
		for {
			i := strings.Index(u, "{date:")
			if i < 0 {
				break
			}
			j := strings.Index(u[i:], "}")
			if j < 0 {
				break
			}
			u = u[:i] + Strftime(u[i+6:i+j], d) + u[i+j+1:]
		}
		if len(out) == 0 || out[len(out)-1] != u {
			out = append(out, u)
		}
	}
	return out
}

// PayerResult is one payer's outcome in a run. JSON names match the Python crawler's state.
type PayerResult struct {
	PayerID string `json:"payer_id"`
	// ok | unchanged | failed | skipped | deferred (retried next run)
	Status string `json:"status"`
	// ok | manual | robots_disallow | robots_unreachable | waf_blocked | http_error | network_error
	// | no_toc_url | too_large | parse_error
	AccessStatus     string          `json:"access_status"`
	TocURL           string          `json:"toc_url"`
	TocSHA256        string          `json:"toc_sha256"`
	TocBytes         int64           `json:"toc_bytes"`
	TocLastUpdatedOn string          `json:"toc_last_updated_on"`
	MrfCount         int             `json:"mrf_count"`
	TotalBytes       int64           `json:"total_bytes"`
	SchemaValid      *bool           `json:"schema_valid"`
	Validation       json.RawMessage `json:"validation"`
	Error            string          `json:"error"`
	CrawledAt        string          `json:"crawled_at"`
	Outputs          []string        `json:"outputs"`
	RawBytes         int64           `json:"raw_bytes"`
}

func NewPayerResult(id string) PayerResult {
	return PayerResult{PayerID: id, Status: "pending", Outputs: []string{}, Validation: json.RawMessage("null")}
}
