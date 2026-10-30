// Package extract streams a TiC in-network rates file and keeps only the rows a pilot needs:
// chosen billing codes and providers on an NPI/TIN allow-list. Raw files are never stored.
//
// Output is normalized so it stays small: rates.csv.gz has one row per negotiated price and
// provider group, and groups.csv.gz maps each kept provider group to its NPIs and TIN.
package extract

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// NormalizeCodeType folds CPT into HCPCS (CPT is HCPCS Level I) and DRG spellings into MS-DRG.
func NormalizeCodeType(t string) string {
	t = strings.ToUpper(strings.TrimSpace(t))
	switch t {
	case "CPT", "HCPCS":
		return "HCPCS"
	case "DRG", "MS-DRG", "MSDRG":
		return "MS-DRG"
	}
	return t
}

// CodeKey is the lookup key for a billing code, e.g. "HCPCS:99213" or "MS-DRG:470".
func CodeKey(codeType, code string) string {
	return NormalizeCodeType(codeType) + ":" + strings.ToUpper(strings.TrimSpace(code))
}

type Options struct {
	Codes map[string]bool // CodeKey -> keep; empty keeps every code
	NPIs  map[int64]bool  // nil keeps every provider
	TINs  map[string]bool // EINs (digits only) kept regardless of NPI
	// FetchRemote resolves provider_references given by URL (location), as older files do.
	FetchRemote func(url string) (io.ReadCloser, error)
	TempDir     string
}

type Meta struct {
	ReportingEntityName string            `json:"reporting_entity_name"`
	ReportingEntityType string            `json:"reporting_entity_type"`
	LastUpdatedOn       string            `json:"last_updated_on"`
	Version             string            `json:"version"`
	Plan                map[string]string `json:"plan,omitempty"`
	SHA256              string            `json:"sha256"`
	Bytes               int64             `json:"bytes_decompressed"`
	InNetworkItems      int               `json:"in_network_items"`
	ItemsKept           int               `json:"in_network_items_kept"`
	ProviderRefs        int               `json:"provider_references"`
	ProviderRefsKept    int               `json:"provider_references_kept"`
	RemoteRefsFetched   int               `json:"remote_references_fetched"`
	RateRows            int               `json:"rate_rows"`
	GroupRows           int               `json:"group_rows"`
	PlaceholderRows     int               `json:"placeholder_rate_rows"`
	CodesFound          []string          `json:"codes_found"`
}

type tin struct {
	Type         string `json:"type"`
	Value        string `json:"value"`
	BusinessName string `json:"business_name"`
}

type providerGroup struct {
	NPI []json.Number `json:"npi"`
	TIN tin           `json:"tin"`
}

type providerRef struct {
	ProviderGroupID json.Number     `json:"provider_group_id"`
	ProviderGroups  []providerGroup `json:"provider_groups"`
	NetworkName     []string        `json:"network_name"`
	Location        string          `json:"location"`
}

type price struct {
	NegotiatedType      string      `json:"negotiated_type"`
	NegotiatedRate      json.Number `json:"negotiated_rate"`
	ExpirationDate      string      `json:"expiration_date"`
	ServiceCode         []string    `json:"service_code"`
	BillingClass        string      `json:"billing_class"`
	Setting             string      `json:"setting"`
	BillingCodeModifier []string    `json:"billing_code_modifier"`
}

type negotiatedRate struct {
	ProviderReferences []json.Number   `json:"provider_references"`
	ProviderGroups     []providerGroup `json:"provider_groups"`
	NegotiatedPrices   []price         `json:"negotiated_prices"`
}

var RateHeader = []string{"billing_code_type", "billing_code", "billing_code_type_version", "negotiation_arrangement",
	"name", "negotiated_type", "negotiated_rate", "billing_class", "setting", "service_code", "billing_code_modifier",
	"expiration_date", "provider_group", "placeholder"}

var GroupHeader = []string{"provider_group", "npi", "tin_type", "tin_value", "business_name", "network_name"}

// IsPlaceholder flags rates that are clearly not real prices (0, a cent, or 999999-style fillers).
func IsPlaceholder(rate float64) bool {
	return rate <= 0.01 || rate >= 999999
}

type extractor struct {
	opt        Options
	dec        *json.Decoder
	meta       *Meta
	groups     *csv.Writer
	tmp        *csv.Writer
	keptRefs   map[string]bool
	remote     map[string]string // provider_group_id -> location URL
	usedRefs   map[string]bool   // refs used by candidate rows
	codesFound map[string]bool
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

// keepGroup returns the NPIs to keep from g, and whether the group is kept at all.
func (x *extractor) keepGroup(g providerGroup) ([]string, bool) {
	tinKept := strings.EqualFold(g.TIN.Type, "ein") && x.opt.TINs[digits(g.TIN.Value)]
	var npis []string
	for _, n := range g.NPI {
		v, err := n.Int64()
		if err != nil {
			if f, ferr := n.Float64(); ferr == nil {
				v = int64(f)
			}
		}
		if v == 0 {
			continue // the schema's "no NPI" placeholder
		}
		if x.opt.NPIs == nil || tinKept || x.opt.NPIs[v] {
			npis = append(npis, strconv.FormatInt(v, 10))
		}
	}
	if len(npis) == 0 && tinKept {
		npis = []string{""}
	}
	return npis, len(npis) > 0
}

func (x *extractor) writeGroups(key string, groups []providerGroup, networks []string) bool {
	kept := false
	for _, g := range groups {
		npis, ok := x.keepGroup(g)
		if !ok {
			continue
		}
		kept = true
		for _, n := range npis {
			x.groups.Write([]string{key, n, strings.ToLower(g.TIN.Type), g.TIN.Value, g.TIN.BusinessName, strings.Join(networks, "|")})
			x.meta.GroupRows++
		}
	}
	return kept
}

// Run streams r (decompressed JSON) and writes rates.csv.gz and groups.csv.gz into outDir.
func Run(r io.Reader, opt Options, outDir string) (*Meta, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	h := sha256.New()
	counter := &countingReader{r: io.TeeReader(r, h)}
	gw, gclose, err := createGz(outDir + "/groups.csv.gz")
	if err != nil {
		return nil, err
	}
	tmpFile, err := os.CreateTemp(opt.TempDir, "rates-*.csv")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpFile.Name())
	tmpBuf := bufio.NewWriterSize(tmpFile, 1<<20)
	x := &extractor{
		opt: opt, dec: json.NewDecoder(bufio.NewReaderSize(counter, 1<<20)), meta: &Meta{Plan: map[string]string{}},
		groups: gw, tmp: csv.NewWriter(tmpBuf), keptRefs: map[string]bool{}, remote: map[string]string{},
		usedRefs: map[string]bool{}, codesFound: map[string]bool{},
	}
	x.dec.UseNumber()
	gw.Write(GroupHeader)
	if err := x.top(); err != nil {
		gclose()
		tmpFile.Close()
		return nil, err
	}
	// Drain any trailing bytes so the hash covers the whole document.
	io.Copy(io.Discard, x.dec.Buffered())
	io.Copy(io.Discard, counter)
	if err := x.resolveRemote(); err != nil {
		gclose()
		tmpFile.Close()
		return nil, err
	}
	x.tmp.Flush()
	tmpBuf.Flush()
	if err := gclose(); err != nil {
		return nil, err
	}
	if err := x.finish(tmpFile, outDir+"/rates.csv.gz"); err != nil {
		return nil, err
	}
	x.meta.SHA256 = hex.EncodeToString(h.Sum(nil))
	x.meta.Bytes = counter.n
	for k := range x.codesFound {
		x.meta.CodesFound = append(x.meta.CodesFound, k)
	}
	sort.Strings(x.meta.CodesFound)
	b, _ := json.MarshalIndent(x.meta, "", "  ")
	return x.meta, os.WriteFile(outDir+"/meta.json", append(b, '\n'), 0o644)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func createGz(path string) (*csv.Writer, func() error, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestCompression) // no name or mtime: deterministic bytes
	w := csv.NewWriter(gz)
	return w, func() error {
		w.Flush()
		if err := w.Error(); err != nil {
			return err
		}
		if err := gz.Close(); err != nil {
			return err
		}
		return f.Close()
	}, nil
}

// finish keeps candidate rows whose provider group survived the filter.
func (x *extractor) finish(tmp *os.File, path string) error {
	defer tmp.Close()
	if _, err := tmp.Seek(0, 0); err != nil {
		return err
	}
	w, closeFn, err := createGz(path)
	if err != nil {
		return err
	}
	w.Write(RateHeader)
	rd := csv.NewReader(bufio.NewReaderSize(tmp, 1<<20))
	rd.FieldsPerRecord = len(RateHeader)
	for {
		rec, err := rd.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			closeFn()
			return err
		}
		if !x.keptRefs[rec[12]] {
			continue
		}
		w.Write(rec)
		x.meta.RateRows++
		if rec[13] == "1" {
			x.meta.PlaceholderRows++
		}
	}
	return closeFn()
}

func (x *extractor) expectDelim(want json.Delim) error {
	t, err := x.dec.Token()
	if err != nil {
		return err
	}
	if d, ok := t.(json.Delim); !ok || d != want {
		return fmt.Errorf("expected %q, got %v", want, t)
	}
	return nil
}

// skip discards the next value without holding it in memory.
func (x *extractor) skip() error {
	depth := 0
	for {
		t, err := x.dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

func (x *extractor) key() (string, error) {
	t, err := x.dec.Token()
	if err != nil {
		return "", err
	}
	k, ok := t.(string)
	if !ok {
		return "", fmt.Errorf("expected key, got %v", t)
	}
	return k, nil
}

func (x *extractor) top() error {
	if err := x.expectDelim('{'); err != nil {
		return err
	}
	for x.dec.More() {
		k, err := x.key()
		if err != nil {
			return err
		}
		switch k {
		case "provider_references":
			err = x.providerReferences()
		case "in_network":
			err = x.inNetwork()
		case "reporting_entity_name", "reporting_entity_type", "last_updated_on", "version",
			"plan_name", "issuer_name", "plan_sponsor_name", "plan_id_type", "plan_id", "plan_market_type":
			var v any
			if err = x.dec.Decode(&v); err == nil {
				s := fmt.Sprint(v)
				switch k {
				case "reporting_entity_name":
					x.meta.ReportingEntityName = s
				case "reporting_entity_type":
					x.meta.ReportingEntityType = s
				case "last_updated_on":
					x.meta.LastUpdatedOn = s
				case "version":
					x.meta.Version = s
				default:
					x.meta.Plan[k] = s
				}
			}
		default:
			err = x.skip()
		}
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return x.expectDelim('}')
}

func (x *extractor) providerReferences() error {
	if err := x.expectDelim('['); err != nil {
		return err
	}
	for x.dec.More() {
		var pr providerRef
		if err := x.dec.Decode(&pr); err != nil {
			return err
		}
		x.meta.ProviderRefs++
		key := pr.ProviderGroupID.String()
		if len(pr.ProviderGroups) == 0 && pr.Location != "" {
			x.remote[key] = pr.Location
			continue
		}
		if x.writeGroups(key, pr.ProviderGroups, pr.NetworkName) {
			x.keptRefs[key] = true
			x.meta.ProviderRefsKept++
		}
	}
	return x.expectDelim(']')
}

func (x *extractor) resolveRemote() error {
	var ids []string
	for id := range x.remote {
		if x.usedRefs[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if x.opt.FetchRemote == nil {
			continue
		}
		rc, err := x.opt.FetchRemote(x.remote[id])
		if err != nil {
			return fmt.Errorf("provider reference %s: %w", id, err)
		}
		var doc struct {
			ProviderGroups []providerGroup `json:"provider_groups"`
		}
		d := json.NewDecoder(rc)
		d.UseNumber()
		err = d.Decode(&doc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("provider reference %s: %w", id, err)
		}
		x.meta.RemoteRefsFetched++
		if x.writeGroups(id, doc.ProviderGroups, nil) {
			x.keptRefs[id] = true
			x.meta.ProviderRefsKept++
		}
	}
	return nil
}

func (x *extractor) inNetwork() error {
	if err := x.expectDelim('['); err != nil {
		return err
	}
	for x.dec.More() {
		x.meta.InNetworkItems++
		if err := x.item(); err != nil {
			return fmt.Errorf("item %d: %w", x.meta.InNetworkItems, err)
		}
	}
	return x.expectDelim(']')
}

type itemHead struct {
	codeType, code, version, arrangement, name string
}

func (h itemHead) known() bool { return h.codeType != "" && h.code != "" }

func (x *extractor) wanted(h itemHead) bool {
	return len(x.opt.Codes) == 0 || x.opt.Codes[CodeKey(h.codeType, h.code)]
}

func (x *extractor) item() error {
	if err := x.expectDelim('{'); err != nil {
		return err
	}
	var h itemHead
	var deferred []json.RawMessage // negotiated_rates seen before the billing code
	kept := false
	for x.dec.More() {
		k, err := x.key()
		if err != nil {
			return err
		}
		switch k {
		case "billing_code", "billing_code_type", "billing_code_type_version", "negotiation_arrangement", "name":
			var v any
			if err := x.dec.Decode(&v); err != nil {
				return err
			}
			s := fmt.Sprint(v)
			switch k {
			case "billing_code":
				h.code = s
			case "billing_code_type":
				h.codeType = s
			case "billing_code_type_version":
				h.version = s
			case "negotiation_arrangement":
				h.arrangement = s
			case "name":
				h.name = s
			}
		case "negotiated_rates":
			if h.known() {
				if !x.wanted(h) {
					err = x.skip()
					break
				}
				kept = true
				err = x.streamRates(h)
				break
			}
			var raw json.RawMessage
			err = x.dec.Decode(&raw)
			deferred = append(deferred, raw)
		default:
			err = x.skip()
		}
		if err != nil {
			return err
		}
	}
	if err := x.expectDelim('}'); err != nil {
		return err
	}
	if len(deferred) > 0 && x.wanted(h) {
		kept = true
		for _, raw := range deferred {
			var rates []negotiatedRate
			d := json.NewDecoder(strings.NewReader(string(raw)))
			d.UseNumber()
			if err := d.Decode(&rates); err != nil {
				return err
			}
			for _, nr := range rates {
				x.emit(h, nr)
			}
		}
	}
	if kept {
		x.meta.ItemsKept++
		x.codesFound[CodeKey(h.codeType, h.code)] = true
	}
	return nil
}

func (x *extractor) streamRates(h itemHead) error {
	if err := x.expectDelim('['); err != nil {
		return err
	}
	for x.dec.More() {
		var nr negotiatedRate
		if err := x.dec.Decode(&nr); err != nil {
			return err
		}
		x.emit(h, nr)
	}
	return x.expectDelim(']')
}

func (x *extractor) emit(h itemHead, nr negotiatedRate) {
	refs := make([]string, 0, len(nr.ProviderReferences)+1)
	for _, r := range nr.ProviderReferences {
		refs = append(refs, r.String())
	}
	if len(nr.ProviderGroups) > 0 { // v1-style inline groups
		hsh := sha256.New()
		b, _ := json.Marshal(nr.ProviderGroups)
		hsh.Write(b)
		key := "inline:" + hex.EncodeToString(hsh.Sum(nil))[:16]
		if !x.keptRefs[key] && x.writeGroups(key, nr.ProviderGroups, nil) {
			x.keptRefs[key] = true
		}
		refs = append(refs, key)
	}
	for _, ref := range refs {
		x.usedRefs[ref] = true
		for _, p := range nr.NegotiatedPrices {
			ph := "0"
			if f, err := p.NegotiatedRate.Float64(); err == nil && IsPlaceholder(f) {
				ph = "1"
			}
			x.tmp.Write([]string{h.codeType, h.code, h.version, h.arrangement, h.name, p.NegotiatedType,
				p.NegotiatedRate.String(), p.BillingClass, p.Setting, strings.Join(p.ServiceCode, "|"),
				strings.Join(p.BillingCodeModifier, "|"), p.ExpirationDate, ref, ph})
		}
	}
}
