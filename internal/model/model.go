// Package model holds plain data types shared across the crawler.
package model

import (
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
