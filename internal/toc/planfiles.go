package toc

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

// PlanFilesHeader is the layout of plans/<payer>/<date>.csv.gz and of extract-rates'
// plan_files.csv.gz: one row per plan and file it references, with no cap on plans.
// The manifest keeps at most manifest_plan_name_cap names per file; this index keeps them all,
// so a plan can always be traced to its own in-network files (HMO and PPO prices differ).
var PlanFilesHeader = []string{"plan_id_type", "plan_id", "plan_name", "plan_market_type", "plan_sponsor_name",
	"issuer_name", "file_type", "file_id", "file_url", "description"}

// FileID is the short id used for a file everywhere: extract-rates' output folder and the
// file_id column on rate rows. It is the first 12 hex characters of SHA-256 of the URL.
func FileID(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])[:12]
}

// PlanFileWriter streams plan-to-file rows into a gzipped CSV.
type PlanFileWriter struct {
	f    *os.File
	gz   *gzip.Writer
	w    *csv.Writer
	Rows int
}

func NewPlanFileWriter(path string) (*PlanFileWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestCompression) // no name or mtime: deterministic bytes
	w := csv.NewWriter(gz)
	w.Write(PlanFilesHeader)
	return &PlanFileWriter{f: f, gz: gz, w: w}, nil
}

func (p *PlanFileWriter) row(pl Plan, kind string, loc FileLoc) {
	if loc.Location == "" {
		return
	}
	p.w.Write([]string{pl.PlanIDType, pl.PlanID, pl.PlanName, pl.PlanMarketType, pl.PlanSponsorName, pl.IssuerName,
		kind, FileID(loc.Location), loc.Location, loc.Description})
	p.Rows++
}

// Add writes every plan x file pair in one reporting_structure item.
func (p *PlanFileWriter) Add(it Item) {
	for _, pl := range it.ReportingPlans {
		for _, f := range it.InNetworkFiles {
			p.row(pl, "in-network", f)
		}
		if it.AllowedAmountFile != nil {
			p.row(pl, "allowed-amount", *it.AllowedAmountFile)
		}
	}
}

func (p *PlanFileWriter) Close() error {
	p.w.Flush()
	if err := p.w.Error(); err != nil {
		p.f.Close()
		return err
	}
	if err := p.gz.Close(); err != nil {
		p.f.Close()
		return err
	}
	return p.f.Close()
}

// WritePlanFiles reads a TOC (plain or gzipped) and writes its plan-to-file index to dest.
// A TOC that does not decode as the CMS layout leaves no file behind and returns the error.
func WritePlanFiles(r io.Reader, dest string) (int, error) {
	gr, err := MaybeGunzip(r)
	if err != nil {
		return 0, err
	}
	w, err := NewPlanFileWriter(dest)
	if err != nil {
		return 0, err
	}
	_, err = Read(gr, func(it Item) error { w.Add(it); return nil })
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dest)
		return 0, err
	}
	return w.Rows, nil
}

// GzCSV is a small gzipped CSV writer with deterministic bytes.
type GzCSV struct{ p *PlanFileWriter }

// NewGzCSV creates a gzipped CSV at path with the given header.
func NewGzCSV(path string, header []string) (*GzCSV, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	w := csv.NewWriter(gz)
	w.Write(header)
	return &GzCSV{&PlanFileWriter{f: f, gz: gz, w: w}}, nil
}

func (g *GzCSV) Write(rec []string) { g.p.w.Write(rec); g.p.Rows++ }
func (g *GzCSV) Close() error       { return g.p.Close() }
