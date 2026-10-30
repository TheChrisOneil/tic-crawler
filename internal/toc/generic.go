package toc

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
)

// GenericHeader holds a TOC's top-level scalars plus what the validator needs to know about
// reporting_structure, since key order is not guaranteed and items may be malformed.
type GenericHeader struct {
	Fields          map[string]any
	HasReportingArr bool // reporting_structure was an array
	ReportingOther  any  // reporting_structure when it was not an array
	TopLevelIsObj   bool
}

// ReadGeneric streams a TOC, calling onItem with each reporting_structure entry as decoded
// JSON (numbers as json.Number), so invalid shapes reach the validator instead of failing here.
func ReadGeneric(r io.Reader, onItem func(index int, item any) error) (GenericHeader, error) {
	h := GenericHeader{Fields: map[string]any{}}
	dec := json.NewDecoder(r)
	dec.UseNumber()
	t, err := dec.Token()
	if err != nil {
		return h, err
	}
	if t != json.Delim('{') {
		return h, nil // the validator reports a non-object top level
	}
	h.TopLevelIsObj = true
	index := 0
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return h, err
		}
		k, _ := t.(string)
		if k != "reporting_structure" {
			var v any
			if err := dec.Decode(&v); err != nil {
				return h, fmt.Errorf("%s: %w", k, err)
			}
			switch v.(type) {
			case map[string]any, []any:
				// nested header values are not used
			default:
				h.Fields[k] = v
			}
			continue
		}
		vt, err := dec.Token()
		if err != nil {
			return h, err
		}
		if vt != json.Delim('[') {
			if d, ok := vt.(json.Delim); ok { // an object: skip it
				depth := 1
				for depth > 0 {
					t, err := dec.Token()
					if err != nil {
						return h, err
					}
					if dd, ok := t.(json.Delim); ok {
						if dd == '{' || dd == '[' {
							depth++
						} else {
							depth--
						}
					}
				}
				_ = d
				h.ReportingOther = map[string]any{}
			} else if vt == nil {
				h.ReportingOther = struct{}{}
			} else {
				h.ReportingOther = vt
			}
			continue
		}
		h.HasReportingArr = true
		for dec.More() {
			var item any
			if err := dec.Decode(&item); err != nil {
				return h, fmt.Errorf("reporting_structure[%d]: %w", index, err)
			}
			if err := onItem(index, item); err != nil {
				return h, err
			}
			index++
		}
		if _, err := dec.Token(); err != nil {
			return h, err
		}
	}
	return h, nil
}

// MrfEntry is one unique MRF URL found in a TOC, with the plans that reference it.
type MrfEntry struct {
	URL             string
	FileType        string
	Descriptions    []string
	PlanNames       []string
	PlanIDTypes     []string
	PlanMarketTypes []string
	PlanCount       int
	PlansTruncated  bool
}

// MrfCollector collects unique MRF URLs from generic items (same rules as the Python crawler).
type MrfCollector struct {
	NameCap  int
	entries  map[string]*MrfEntry
	order    []string
	planKeys map[string]map[uint64]bool
}

func NewMrfCollector(nameCap int) *MrfCollector {
	return &MrfCollector{NameCap: nameCap, entries: map[string]*MrfEntry{}, planKeys: map[string]map[uint64]bool{}}
}

func str(v any) (string, bool) { s, ok := v.(string); return s, ok }

func addCapped(list []string, v any, cap int) []string {
	s, ok := str(v)
	if !ok {
		return list
	}
	for _, x := range list {
		if x == s {
			return list
		}
	}
	if len(list) >= cap {
		return list
	}
	return append(list, s)
}

func (c *MrfCollector) AddItem(item any) {
	m, ok := item.(map[string]any)
	if !ok {
		return
	}
	var plans []map[string]any
	if ps, ok := m["reporting_plans"].([]any); ok {
		for _, p := range ps {
			if pm, ok := p.(map[string]any); ok {
				plans = append(plans, pm)
			}
		}
	}
	type file struct {
		kind string
		loc  map[string]any
	}
	var files []file
	if fs, ok := m["in_network_files"].([]any); ok {
		for _, f := range fs {
			if fm, ok := f.(map[string]any); ok {
				files = append(files, file{"in-network", fm})
			}
		}
	}
	if aa, ok := m["allowed_amount_file"].(map[string]any); ok {
		files = append(files, file{"allowed-amount", aa})
	}
	for _, f := range files {
		url, ok := str(f.loc["location"])
		if !ok || url == "" {
			continue
		}
		e := c.entries[url]
		if e == nil {
			e = &MrfEntry{URL: url, FileType: f.kind}
			c.entries[url] = e
			c.order = append(c.order, url)
			c.planKeys[url] = map[uint64]bool{}
		}
		if d, ok := str(f.loc["description"]); ok && len(e.Descriptions) < 5 && !containsStr(e.Descriptions, d) {
			e.Descriptions = append(e.Descriptions, d)
		}
		keys := c.planKeys[url]
		for _, p := range plans {
			h := fnv.New64a()
			fmt.Fprintf(h, "%v\x00%v\x00%v", p["plan_id_type"], p["plan_id"], p["plan_name"])
			k := h.Sum64()
			if keys[k] {
				continue
			}
			keys[k] = true
			e.PlanCount++
			e.PlanIDTypes = addCapped(e.PlanIDTypes, p["plan_id_type"], 10)
			e.PlanMarketTypes = addCapped(e.PlanMarketTypes, p["plan_market_type"], 10)
			if name, ok := str(p["plan_name"]); ok && !containsStr(e.PlanNames, name) {
				if len(e.PlanNames) < c.NameCap {
					e.PlanNames = append(e.PlanNames, name)
				} else {
					e.PlansTruncated = true
				}
			}
		}
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (c *MrfCollector) Len() int { return len(c.order) }

func (c *MrfCollector) Entries() []*MrfEntry {
	out := make([]*MrfEntry, 0, len(c.order))
	for _, u := range c.order {
		out = append(out, c.entries[u])
	}
	return out
}

// ItemDigest hashes an item with sorted keys, for the uniqueItems check.
func ItemDigest(item any) [32]byte {
	b, _ := json.Marshal(item) // encoding/json sorts map keys
	return sha256.Sum256(b)
}
