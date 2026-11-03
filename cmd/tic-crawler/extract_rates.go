package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/TheChrisOneil/tic-crawler/internal/extract"
	"github.com/TheChrisOneil/tic-crawler/internal/fetch"
	"github.com/TheChrisOneil/tic-crawler/internal/toc"
)

type codeList struct {
	Codes []struct {
		Type  string `yaml:"type"`
		Code  string `yaml:"code"`
		Label string `yaml:"label"`
	} `yaml:"codes"`
}

func loadCodes(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cl codeList
	if err := yaml.Unmarshal(b, &cl); err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, c := range cl.Codes {
		m[extract.CodeKey(c.Type, c.Code)] = true
	}
	return m, nil
}

// loadColumn reads one column (by header name) of a CSV file into a set.
func loadColumn(path, column string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := toc.MaybeGunzip(f)
	if err != nil {
		return nil, err
	}
	cr := csv.NewReader(r)
	header, err := cr.Read()
	if err != nil {
		return nil, err
	}
	idx := -1
	for i, h := range header {
		if h == column {
			idx = i
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("%s has no %q column", path, column)
	}
	out := map[string]bool{}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if v := strings.TrimSpace(rec[idx]); v != "" {
			out[v] = true
		}
	}
}

// budgetReader fails once more than limit bytes have been read.
type budgetReader struct {
	r     io.Reader
	n     int64
	limit int64
}

var errBudget = errors.New("run byte budget exceeded (limits.max_run_bytes); ask the owner before raising it")

func (b *budgetReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += int64(n)
	if b.n > b.limit {
		return n, errBudget
	}
	return n, err
}

type fileResult struct {
	URL             string        `json:"url"`
	Dir             string        `json:"dir,omitempty"`
	PlanCount       int           `json:"plan_count"`
	PlanNames       []string      `json:"plan_names"`
	ContentLength   int64         `json:"content_length,omitempty"`
	BytesDownloaded int64         `json:"bytes_downloaded"`
	Status          string        `json:"status"` // ok | skipped_size | failed | not_tried
	Error           string        `json:"error,omitempty"`
	Meta            *extract.Meta `json:"meta,omitempty"`
}

func extractRatesCmd() *cobra.Command {
	var payer, tocURL, codesPath, npiFile, tinFile, outDir string
	var urls []string
	var maxFiles int
	var skip string
	var maxBytes int64
	cmd := &cobra.Command{
		Use:   "extract-rates",
		Short: "Stream in-network files and keep only listed billing codes and providers",
		Long: `Streams in-network rate files (from a TOC or explicit --url) and keeps rows for the billing
codes in --codes and, when given, the NPIs in --npi-file or EINs in --tin-file. Raw files are
never stored. The run stops before downloading more than limits.max_run_bytes.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			if maxBytes <= 0 || maxBytes > s.MaxRunBytes {
				maxBytes = s.MaxRunBytes
			}
			codes, err := loadCodes(codesPath)
			if err != nil {
				return err
			}
			opt := extract.Options{Codes: codes, Today: time.Now().UTC().Format("2006-01-02")}
			if npiFile != "" {
				set, err := loadColumn(npiFile, "npi")
				if err != nil {
					return err
				}
				opt.NPIs = map[int64]bool{}
				for k := range set {
					if n, err := strconv.ParseInt(k, 10, 64); err == nil {
						opt.NPIs[n] = true
					}
				}
			}
			if tinFile != "" {
				set, err := loadColumn(tinFile, "ein")
				if err != nil {
					return err
				}
				opt.TINs = map[string]bool{}
				for k := range set {
					opt.TINs[strings.ReplaceAll(k, "-", "")] = true
				}
			}
			ctx := context.Background()
			client := newClient(s)
			// Remote provider references are read while the main file's slot is held, so they
			// bypass the concurrency semaphore (Send still applies robots and the rate limit).
			opt.FetchRemote = func(u string) (io.ReadCloser, error) {
				resp, err := client.Send(ctx, "GET", u, nil)
				if err != nil {
					return nil, err
				}
				if resp.StatusCode >= 400 {
					resp.Body.Close()
					return nil, &fetch.FetchError{URL: u, Status: resp.StatusCode}
				}
				return resp.Body, nil
			}

			var candidates []*toc.Entry
			for _, u := range urls {
				candidates = append(candidates, &toc.Entry{URL: u, FileType: "in-network"})
			}
			if tocURL != "" {
				resp, err := client.Get(ctx, tocURL)
				if err != nil {
					return fmt.Errorf("TOC: %w", err)
				}
				r, err := toc.MaybeGunzip(resp.Body)
				if err != nil {
					resp.Body.Close()
					return err
				}
				col := toc.NewCollector(s.ManifestPlanNameCap)
				pf, err := toc.NewPlanFileWriter(filepath.Join(outDir, payer, "plan_files.csv.gz"))
				if err != nil {
					resp.Body.Close()
					return err
				}
				_, err = toc.Read(r, func(it toc.Item) error { col.Add(it); pf.Add(it); return nil })
				resp.Body.Close()
				if cerr := pf.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					return fmt.Errorf("TOC: %w", err)
				}
				candidates = append(candidates, col.ByPlanCount()...)
			}
			if len(candidates) == 0 {
				return errors.New("give --toc-url or --url")
			}
			if skip != "" {
				re, err := regexp.Compile(skip)
				if err != nil {
					return fmt.Errorf("--skip: %w", err)
				}
				kept := candidates[:0]
				for _, e := range candidates {
					if !re.MatchString(e.URL) {
						kept = append(kept, e)
					}
				}
				candidates = kept
			}

			base := filepath.Join(outDir, payer)
			var results []fileResult
			var spent int64
			done := 0
			for i, e := range candidates {
				if done >= maxFiles {
					// Record the rest so the index shows what a later run could pick.
					for _, rest := range candidates[i:] {
						results = append(results, fileResult{URL: rest.URL, PlanCount: rest.PlanCount, Status: "not_tried"})
					}
					break
				}
				res := fileResult{URL: e.URL, PlanCount: e.PlanCount, PlanNames: e.PlanNames}
				check := client.HeadOrRange(ctx, e.URL)
				res.ContentLength = check.ContentLength
				if check.ContentLength > 0 && spent+check.ContentLength > maxBytes {
					res.Status = "skipped_size"
					results = append(results, res)
					continue
				}
				res.Dir = toc.FileID(e.URL)
				opt.FileID = res.Dir
				start := time.Now()
				meta, n, err := extractOne(ctx, client, e.URL, opt, filepath.Join(base, res.Dir), maxBytes-spent)
				spent += n
				res.BytesDownloaded, res.Meta = n, meta
				if err != nil {
					res.Status, res.Error = "failed", err.Error()
					os.RemoveAll(filepath.Join(base, res.Dir))
					res.Dir = ""
				} else {
					res.Status = "ok"
					done++
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s %d bytes in %s %s\n", res.Status, e.URL, n, time.Since(start).Round(time.Second), res.Error)
				results = append(results, res)
				if errors.Is(err, errBudget) {
					break
				}
			}
			if err := os.MkdirAll(base, 0o755); err != nil {
				return err
			}
			idx := map[string]any{"payer_id": payer, "toc_url": tocURL, "max_run_bytes": maxBytes,
				"bytes_downloaded": spent, "files": results}
			b, _ := json.MarshalIndent(idx, "", "  ")
			if err := os.WriteFile(filepath.Join(base, "index.json"), append(b, '\n'), 0o644); err != nil {
				return err
			}
			if tocURL != "" {
				if err := writePlanCoverage(base, results); err != nil {
					return fmt.Errorf("plan coverage: %w", err)
				}
			}
			if done == 0 {
				return errors.New("no file extracted; see index.json")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&payer, "payer", "", "payer id (output folder name)")
	f.StringVar(&tocURL, "toc-url", "", "TOC to pick in-network files from (most plans first)")
	f.StringSliceVar(&urls, "url", nil, "in-network file URL (repeatable)")
	f.StringVar(&codesPath, "codes", "config/codes.yaml", "billing codes to keep")
	f.StringVar(&npiFile, "npi-file", "", "CSV with an npi column; keep only these providers")
	f.StringVar(&tinFile, "tin-file", "", "CSV with an ein column; also keep these organizations")
	f.StringVar(&skip, "skip", "", "regexp; leave out in-network files whose URL matches")
	f.IntVar(&maxFiles, "max-files", 1, "stop after this many files")
	f.Int64Var(&maxBytes, "max-bytes", 0, "download budget in bytes (default and ceiling: limits.max_run_bytes)")
	f.StringVar(&outDir, "out", "out/rates", "output folder")
	cmd.MarkFlagRequired("payer")
	return cmd
}

func extractOne(ctx context.Context, c *fetch.Client, u string, opt extract.Options, dir string, budget int64) (*extract.Meta, int64, error) {
	resp, err := c.Get(ctx, u)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	br := &budgetReader{r: resp.Body, limit: budget}
	r, err := toc.MaybeGunzip(br)
	if err != nil {
		return nil, br.n, err
	}
	meta, err := extract.Run(r, opt, dir)
	if errors.Is(br.lastErr(), errBudget) {
		err = errBudget
	}
	return meta, br.n, err
}

func (b *budgetReader) lastErr() error {
	if b.n > b.limit {
		return errBudget
	}
	return nil
}

// PlanCoverageHeader is the layout of plan_coverage.csv.gz: for each plan in the TOC, how many
// of its in-network files were extracted, and for each billing code how many providers (NPIs)
// have a usable dollar rate. providers_with_rate is the most found in any one of the plan's
// extracted files (a lower bound for the plan). A plan with no extracted file has one row with
// an empty code, so "no rate" is never mistaken for "not covered".
var PlanCoverageHeader = []string{"plan_id_type", "plan_id", "plan_name", "plan_market_type", "in_network_files",
	"files_extracted", "code", "providers_with_rate", "tins_with_rate"}

func writePlanCoverage(base string, results []fileResult) error {
	type covKey struct{ file, code string }
	cov := map[covKey]extract.CodeCoverage{}
	extracted := map[string]bool{}
	codesByFile := map[string][]string{}
	for _, r := range results {
		if r.Status != "ok" || r.Meta == nil {
			continue
		}
		extracted[r.Dir] = true
		for _, c := range r.Meta.Coverage {
			cov[covKey{r.Dir, c.Code}] = c
		}
		codesByFile[r.Dir] = append(append([]string{}, r.Meta.CodesFound...), r.Meta.CodesMissing...)
	}
	f, err := os.Open(filepath.Join(base, "plan_files.csv.gz"))
	if err != nil {
		return err
	}
	defer f.Close()
	gr, err := toc.MaybeGunzip(f)
	if err != nil {
		return err
	}
	cr := csv.NewReader(gr)
	cr.FieldsPerRecord = len(toc.PlanFilesHeader)
	if _, err := cr.Read(); err != nil {
		return err
	}
	type plan struct {
		row   []string
		files []string
	}
	plans := map[string]*plan{}
	var order []string
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if rec[6] != "in-network" {
			continue
		}
		k := rec[0] + "\x00" + rec[1] + "\x00" + rec[2]
		p := plans[k]
		if p == nil {
			p = &plan{row: rec[:4]}
			plans[k] = p
			order = append(order, k)
		}
		p.files = append(p.files, rec[7])
	}
	w, err := toc.NewGzCSV(filepath.Join(base, "plan_coverage.csv.gz"), PlanCoverageHeader)
	if err != nil {
		return err
	}
	for _, k := range order {
		p := plans[k]
		seen := map[string]bool{}
		var ext []string
		for _, fid := range p.files {
			if !seen[fid] {
				seen[fid] = true
				if extracted[fid] {
					ext = append(ext, fid)
				}
			}
		}
		head := append(append([]string{}, p.row...), strconv.Itoa(len(seen)), strconv.Itoa(len(ext)))
		if len(ext) == 0 {
			w.Write(append(head, "", "", ""))
			continue
		}
		best := map[string]extract.CodeCoverage{}
		var codes []string
		for _, fid := range ext {
			for _, code := range codesByFile[fid] {
				c, ok := best[code]
				if !ok {
					codes = append(codes, code)
				}
				if fc := cov[covKey{fid, code}]; !ok || fc.NPIs > c.NPIs {
					best[code] = fc
				}
			}
		}
		sort.Strings(codes)
		for _, code := range codes {
			c := best[code]
			w.Write(append(append([]string{}, head...), code, strconv.Itoa(c.NPIs), strconv.Itoa(c.TINs)))
		}
	}
	return w.Close()
}
