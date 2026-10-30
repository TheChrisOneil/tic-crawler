package main

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
			opt := extract.Options{Codes: codes}
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
			client := fetch.New(s)
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
				_, err = toc.Read(r, func(it toc.Item) error { col.Add(it); return nil })
				resp.Body.Close()
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
				sum := sha256.Sum256([]byte(e.URL))
				res.Dir = hex.EncodeToString(sum[:])[:12]
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
