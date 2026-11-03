// Package discovery finds payers. Each source is a Strategy registered by name in init().
package discovery

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/TheChrisOneil/tic-crawler/internal/config"
	"github.com/TheChrisOneil/tic-crawler/internal/fetch"
	"github.com/TheChrisOneil/tic-crawler/internal/model"
)

var suffixes = map[string]bool{"inc": true, "incorporated": true, "co": true, "company": true, "corp": true,
	"corporation": true, "llc": true, "lp": true, "llp": true, "ltd": true, "the": true, "pc": true, "pa": true}

var nonAlnumSpace = regexp.MustCompile(`[^a-z0-9 ]+`)

// NormalizeName is the normalized legal name used for de-duplication.
func NormalizeName(name string) string {
	s := strings.ReplaceAll(strings.ToLower(name), "&", " and ")
	words := strings.Fields(nonAlnumSpace.ReplaceAllString(s, " "))
	for len(words) > 0 && suffixes[words[len(words)-1]] {
		words = words[:len(words)-1]
	}
	for len(words) > 0 && words[0] == "the" {
		words = words[1:]
	}
	return strings.Join(words, " ")
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func Slugify(name string) string {
	s := strings.Trim(nonAlnum.ReplaceAllString(NormalizeName(name), "-"), "-")
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		return "payer"
	}
	return s
}

// Source is a way to find payers.
type Source interface {
	Name() string
	Order() int
	Active(today time.Time) bool
	Discover(ctx context.Context, c *fetch.Client, known []model.Payer) ([]model.Payer, error)
}

// Factory builds a source from settings. Register adds one to the registry.
type Factory func(config.Settings) Source

var registry = map[string]Factory{}

func Register(name string, f Factory) { registry[name] = f }
func Unregister(name string)          { delete(registry, name) }

func init() {
	Register("seed", func(s config.Settings) Source { return &SeedFile{Path: s.SeedFile} })
	Register("exchange_puf", func(s config.Settings) Source {
		return &ExchangePUF{URL: s.ExchangePUFURL, MaxBytes: s.ExchangePUFMaxBytes}
	})
	Register("root_txt", func(s config.Settings) Source {
		return &RootTxt{From: s.RootTxtFrom, Filename: s.RootTxtFilename}
	})
}

// ---------- seed file ----------

type SeedFile struct{ Path string }

func (*SeedFile) Name() string          { return "seed" }
func (*SeedFile) Order() int            { return 10 }
func (*SeedFile) Active(time.Time) bool { return true }
func (s *SeedFile) Discover(context.Context, *fetch.Client, []model.Payer) ([]model.Payer, error) {
	return LoadSeedFile(s.Path)
}

type seedEntry struct {
	model.Payer `yaml:",inline"`
	HIOS        []any `yaml:"hios_ids"`
	Priority    *int  `yaml:"priority"`
}

func LoadSeedFile(path string) ([]model.Payer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Payers []seedEntry `yaml:"payers"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	var out []model.Payer
	for _, e := range doc.Payers {
		p := e.Payer
		if p.LegalName == "" {
			return nil, fmt.Errorf("seed %q has no legal_name", p.PayerID)
		}
		if p.PayerID == "" {
			p.PayerID = Slugify(p.LegalName)
		}
		p.HIOSIDs = nil
		for _, h := range e.HIOS {
			p.HIOSIDs = append(p.HIOSIDs, fmt.Sprint(h))
		}
		if p.Kind == "" {
			p.Kind = "issuer"
		}
		p.Priority = 100
		if e.Priority != nil {
			p.Priority = *e.Priority
		}
		p.Sources = []string{"seed"}
		out = append(out, p)
	}
	return out, nil
}

// ---------- CMS Exchange PUF ----------

// ExchangePUF reads CMS Exchange Public Use Files: issuer names, HIOS issuer IDs and states.
// It covers HealthCare.gov issuers only; state exchanges and group carriers come from seeds.
type ExchangePUF struct {
	URL      string
	MaxBytes int64
}

func (*ExchangePUF) Name() string          { return "exchange_puf" }
func (*ExchangePUF) Order() int            { return 20 }
func (*ExchangePUF) Active(time.Time) bool { return true }

var (
	pufID    = []string{"IssuerId", "Issuer ID", "Issuer_ID", "HIOS Issuer ID", "HIOS_ISSUER_ID"}
	pufName  = []string{"IssuerMarketPlaceMarketingName", "Issuer Name", "Issuer_Name", "IssuerName", "Issuer Marketing Name"}
	pufState = []string{"StateCode", "State", "State Code", "STATE"}
)

func (e *ExchangePUF) Discover(ctx context.Context, c *fetch.Client, _ []model.Payer) ([]model.Payer, error) {
	if e.URL == "" {
		return nil, nil
	}
	resp, err := c.Get(ctx, e.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	tmp, err := os.CreateTemp("", "puf-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, e.MaxBytes+1))
	tmp.Close()
	if err != nil {
		return nil, err
	}
	if n > e.MaxBytes {
		return nil, &fetch.FetchError{URL: e.URL, Status: resp.StatusCode, Message: "PUF exceeds size cap"}
	}
	return ParsePUF(tmp.Name())
}

func pick(header []string, options []string) int {
	for _, o := range options {
		for i, h := range header {
			if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")), o) {
				return i
			}
		}
	}
	return -1
}

// ParsePUF streams PUF CSV rows (plain or zipped) into one payer per HIOS issuer ID.
func ParsePUF(path string) ([]model.Payer, error) {
	var readers []io.ReadCloser
	if zr, err := zip.OpenReader(path); err == nil {
		defer zr.Close()
		for _, f := range zr.File {
			if strings.HasSuffix(strings.ToLower(f.Name), ".csv") {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				readers = append(readers, rc)
			}
		}
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		readers = append(readers, f)
	}
	byID := map[string]*model.Payer{}
	var order []string
	for _, rc := range readers {
		err := func() error {
			defer rc.Close()
			cr := csv.NewReader(rc)
			cr.FieldsPerRecord = -1
			header, err := cr.Read()
			if err != nil {
				return nil
			}
			idc, namec, statec := pick(header, pufID), pick(header, pufName), pick(header, pufState)
			if idc < 0 || namec < 0 {
				return nil
			}
			for {
				rec, err := cr.Read()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				get := func(i int) string {
					if i < 0 || i >= len(rec) {
						return ""
					}
					return strings.TrimSpace(rec[i])
				}
				hios, name := get(idc), get(namec)
				if hios == "" || name == "" {
					continue
				}
				p, ok := byID[hios]
				if !ok {
					p = &model.Payer{PayerID: "hios-" + hios, LegalName: name, HIOSIDs: []string{hios},
						Sources: []string{"exchange_puf"}, Kind: "issuer", Priority: 100}
					byID[hios] = p
					order = append(order, hios)
				}
				if st := strings.ToUpper(get(statec)); st != "" && !contains(p.States, st) {
					p.States = append(p.States, st)
				}
			}
		}()
		if err != nil {
			return nil, err
		}
	}
	out := make([]model.Payer, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// ---------- root .txt (from 2027-09-06) ----------

// RootTxt reads https://<payer host>/<filename> once the rule applies, taking https lines as
// TOC locations and an email as the contact. The file name and format are placeholders until
// checked against the final rule text.
type RootTxt struct {
	From     time.Time
	Filename string
}

func (*RootTxt) Name() string                  { return "root_txt" }
func (*RootTxt) Order() int                    { return 90 }
func (r *RootTxt) Active(today time.Time) bool { return !today.Before(r.From) }

var (
	httpsRx = regexp.MustCompile(`https://\S+`)
	emailRx = regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.-]+`)
)

func ParseRootTxt(text string) ([]string, string) {
	return httpsRx.FindAllString(text, -1), emailRx.FindString(text)
}

func (r *RootTxt) Discover(ctx context.Context, c *fetch.Client, known []model.Payer) ([]model.Payer, error) {
	var found []model.Payer
	for _, p := range known {
		if p.LandingURL == "" {
			continue
		}
		u, err := url.Parse(p.LandingURL)
		if err != nil {
			continue
		}
		text, err := c.GetText(ctx, u.Scheme+"://"+u.Host+"/"+r.Filename, 1<<20)
		if err != nil {
			continue
		}
		tocs, contact := ParseRootTxt(text)
		if len(tocs) > 0 {
			np := model.Payer{PayerID: p.PayerID, LegalName: p.LegalName, TocURL: tocs[0], Sources: []string{"root_txt"},
				Kind: "issuer", Priority: 100}
			if contact != "" {
				np.Notes = "contact: " + contact
			}
			found = append(found, np)
		}
	}
	return found, nil
}

// ---------- de-duplication ----------

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func extend(dst []string, src []string) []string {
	for _, v := range src {
		if !contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}

// Dedupe merges payers sharing a normalized legal name (or alias) or any HIOS ID.
// Earlier entries win for scalar fields, so seed data overrides the PUF.
func Dedupe(payers []model.Payer) []model.Payer {
	parent := make([]int, len(payers))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			if ra < rb {
				parent[rb] = ra
			} else {
				parent[ra] = rb
			}
		}
	}
	first := map[string]int{}
	for i, p := range payers {
		var keys []string
		for _, n := range append([]string{p.LegalName}, p.Aliases...) {
			if n != "" {
				keys = append(keys, "n:"+NormalizeName(n))
			}
		}
		for _, h := range p.HIOSIDs {
			keys = append(keys, "h:"+h)
		}
		for _, k := range keys {
			if j, ok := first[k]; ok {
				union(j, i)
			} else {
				first[k] = i
			}
		}
	}
	groups := map[int][]model.Payer{}
	var roots []int
	for i, p := range payers {
		r := find(i)
		if _, ok := groups[r]; !ok {
			roots = append(roots, r)
		}
		groups[r] = append(groups[r], p)
	}
	sort.Ints(roots)
	var out []model.Payer
	for _, r := range roots {
		g := groups[r]
		b := g[0]
		b.HIOSIDs = append([]string(nil), b.HIOSIDs...)
		b.States = append([]string(nil), b.States...)
		b.Aliases = append([]string(nil), b.Aliases...)
		b.Sources = append([]string(nil), b.Sources...)
		b.Cohorts = append([]string(nil), b.Cohorts...)
		for _, o := range g[1:] {
			for _, f := range []struct {
				dst *string
				src string
			}{{&b.LandingURL, o.LandingURL}, {&b.TocURL, o.TocURL},
				{&b.TocLinkPattern, o.TocLinkPattern}, {&b.TocFile, o.TocFile}, {&b.Notes, o.Notes}} {
				if *f.dst == "" && f.src != "" {
					*f.dst = f.src
				}
			}
			if o.Priority < b.Priority {
				b.Priority = o.Priority
			}
			b.HIOSIDs = extend(b.HIOSIDs, o.HIOSIDs)
			b.States = extend(b.States, o.States)
			b.Aliases = extend(b.Aliases, o.Aliases)
			b.Sources = extend(b.Sources, o.Sources)
			b.Cohorts = extend(b.Cohorts, o.Cohorts)
			if NormalizeName(o.LegalName) != NormalizeName(b.LegalName) && !contains(b.Aliases, o.LegalName) {
				b.Aliases = append(b.Aliases, o.LegalName)
			}
		}
		sort.Strings(b.HIOSIDs)
		sort.Strings(b.States)
		out = append(out, b)
	}
	return out
}

// Run runs the active sources in order. One source failing does not stop the others.
func Run(ctx context.Context, s config.Settings, c *fetch.Client, today time.Time, only []string) ([]model.Payer, map[string]string) {
	var sources []Source
	for _, f := range registry {
		sources = append(sources, f(s))
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Order() != sources[j].Order() {
			return sources[i].Order() < sources[j].Order()
		}
		return sources[i].Name() < sources[j].Name()
	})
	var payers []model.Payer
	status := map[string]string{}
	for _, src := range sources {
		if len(only) > 0 && !contains(only, src.Name()) {
			continue
		}
		if !src.Active(today) {
			status[src.Name()] = "inactive"
			continue
		}
		found, err := src.Discover(ctx, c, Dedupe(payers))
		if err != nil {
			msg := fmt.Sprintf("error:%T: %v", err, err)
			if len(msg) > 300 {
				msg = msg[:300]
			}
			status[src.Name()] = msg
			continue
		}
		status[src.Name()] = fmt.Sprintf("ok:%d", len(found))
		payers = append(payers, found...)
	}
	return Dedupe(payers), status
}
