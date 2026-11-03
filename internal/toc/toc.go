// Package toc streams a TiC table-of-contents file. Memory stays bounded by one
// reporting_structure item; key order is not guaranteed, so the header is final only at the end.
package toc

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

type Plan struct {
	PlanName        string `json:"plan_name"`
	PlanIDType      string `json:"plan_id_type"`
	PlanID          string `json:"plan_id"`
	PlanMarketType  string `json:"plan_market_type"`
	PlanSponsorName string `json:"plan_sponsor_name,omitempty"`
	IssuerName      string `json:"issuer_name,omitempty"`
}

type FileLoc struct {
	Description string `json:"description"`
	Location    string `json:"location"`
}

type Item struct {
	ReportingPlans    []Plan    `json:"reporting_plans"`
	InNetworkFiles    []FileLoc `json:"in_network_files"`
	AllowedAmountFile *FileLoc  `json:"allowed_amount_file"`
}

type Header struct {
	ReportingEntityName string `json:"reporting_entity_name"`
	ReportingEntityType string `json:"reporting_entity_type"`
	LastUpdatedOn       string `json:"last_updated_on"`
	Version             string `json:"version"`
}

// Entry is one unique MRF URL with the plans that reference it.
type Entry struct {
	URL          string   `json:"url"`
	FileType     string   `json:"file_type"` // in-network | allowed-amount
	Descriptions []string `json:"descriptions"`
	PlanNames    []string `json:"plan_names"`
	PlanIDs      []string `json:"plan_ids"`
	MarketTypes  []string `json:"plan_market_types"`
	PlanCount    int      `json:"plan_count"`
}

// Collector groups MRF URLs across reporting_structure items.
type Collector struct {
	NameCap int
	entries map[string]*Entry
	order   []string
}

func NewCollector(nameCap int) *Collector {
	return &Collector{NameCap: nameCap, entries: map[string]*Entry{}}
}

func addUnique(list []string, v string, limit int) []string {
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	if limit > 0 && len(list) >= limit {
		return list
	}
	return append(list, v)
}

func (c *Collector) add(loc FileLoc, kind string, plans []Plan) {
	if loc.Location == "" {
		return
	}
	e, ok := c.entries[loc.Location]
	if !ok {
		e = &Entry{URL: loc.Location, FileType: kind}
		c.entries[loc.Location] = e
		c.order = append(c.order, loc.Location)
	}
	e.Descriptions = addUnique(e.Descriptions, loc.Description, 10)
	for _, p := range plans {
		e.PlanCount++
		e.PlanNames = addUnique(e.PlanNames, p.PlanName, c.NameCap)
		e.PlanIDs = addUnique(e.PlanIDs, p.PlanID, 0)
		e.MarketTypes = addUnique(e.MarketTypes, p.PlanMarketType, 0)
	}
}

func (c *Collector) Add(it Item) {
	for _, f := range it.InNetworkFiles {
		c.add(f, "in-network", it.ReportingPlans)
	}
	if it.AllowedAmountFile != nil {
		c.add(*it.AllowedAmountFile, "allowed-amount", it.ReportingPlans)
	}
}

// Entries returns entries in first-seen order.
func (c *Collector) Entries() []*Entry {
	out := make([]*Entry, 0, len(c.order))
	for _, u := range c.order {
		out = append(out, c.entries[u])
	}
	return out
}

// ByPlanCount returns in-network entries, most plans first (ties by URL).
func (c *Collector) ByPlanCount() []*Entry {
	var out []*Entry
	for _, e := range c.Entries() {
		if e.FileType == "in-network" {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].PlanCount != out[j].PlanCount {
			return out[i].PlanCount > out[j].PlanCount
		}
		return out[i].URL < out[j].URL
	})
	return out
}

// MaybeGunzip returns a reader that decompresses r when it starts with the gzip magic bytes.
func MaybeGunzip(r io.Reader) (io.Reader, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	magic, err := br.Peek(2)
	if err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		return gzip.NewReader(br)
	}
	return br, nil
}

// Read streams a TOC, calling onItem for each reporting_structure item, and returns the header.
func Read(r io.Reader, onItem func(Item) error) (Header, error) {
	var h Header
	dec := json.NewDecoder(r)
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return h, fmt.Errorf("TOC is not a JSON object: %v %v", t, err)
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return h, err
		}
		k, _ := t.(string)
		switch k {
		case "reporting_structure":
			if t, err := dec.Token(); err != nil || t != json.Delim('[') {
				return h, fmt.Errorf("reporting_structure is not an array")
			}
			for dec.More() {
				var it Item
				if err := dec.Decode(&it); err != nil {
					return h, err
				}
				if err := onItem(it); err != nil {
					return h, err
				}
			}
			if _, err := dec.Token(); err != nil {
				return h, err
			}
		case "reporting_entity_name":
			err = dec.Decode(&h.ReportingEntityName)
		case "reporting_entity_type":
			err = dec.Decode(&h.ReportingEntityType)
		case "last_updated_on":
			err = dec.Decode(&h.LastUpdatedOn)
		case "version":
			err = dec.Decode(&h.Version)
		default:
			var skip json.RawMessage
			err = dec.Decode(&skip)
		}
		if err != nil {
			return h, fmt.Errorf("%s: %w", k, err)
		}
	}
	return h, nil
}
