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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/TheChrisOneil/tic-crawler/internal/audit"
	"github.com/TheChrisOneil/tic-crawler/internal/config"
	"github.com/TheChrisOneil/tic-crawler/internal/discovery"
	"github.com/TheChrisOneil/tic-crawler/internal/model"
	"github.com/TheChrisOneil/tic-crawler/internal/pipeline"
	"github.com/TheChrisOneil/tic-crawler/internal/state"
	"github.com/TheChrisOneil/tic-crawler/internal/storage"
	"github.com/TheChrisOneil/tic-crawler/internal/validate"
)

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func num(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case json.Number:
		i, _ := v.Int64()
		return i
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func openState(s config.Settings) (*state.Store, error) {
	if err := os.MkdirAll(filepath.Dir(s.StateDB), 0o755); err != nil {
		return nil, err
	}
	return state.Open(s.StateDB)
}

func writeJSONFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func discoverCmd() *cobra.Command {
	var only []string
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Find payers from all discovery sources, de-duplicate, and save them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			st, err := openState(s)
			if err != nil {
				return err
			}
			defer st.Close()
			payers, status := discovery.Run(context.Background(), s, newClient(s), today(), only)
			if err := st.SavePayers(payers); err != nil {
				return err
			}
			if err := writeJSONFile(filepath.Join(s.LocalDir, "payers.json"), payers); err != nil {
				return err
			}
			names := make([]string, 0, len(status))
			for k := range status {
				names = append(names, k)
			}
			sort.Strings(names)
			w := cmd.OutOrStdout()
			for _, k := range names {
				fmt.Fprintf(w, "%s: %s\n", k, status[k])
			}
			withTOC := 0
			for _, p := range payers {
				if p.TocURL != "" || p.TocLinkPattern != "" {
					withTOC++
				}
			}
			fmt.Fprintf(w, "payers: %d (with TOC URL: %d)\n", len(payers), withTOC)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&only, "source", nil, "only run these sources")
	return cmd
}

func crawlCmd() *cobra.Command {
	var ids []string
	var all, raw, force bool
	var tocFile string
	cmd := &cobra.Command{
		Use:   "crawl",
		Short: "Fetch, validate and catalog TOCs; raw MRFs are only HEAD-checked unless --download-raw",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(ids) == 0 && !all {
				return errors.New("give --payer <id> or --all")
			}
			if tocFile != "" && (all || len(ids) != 1) {
				return errors.New("--toc-file needs exactly one --payer")
			}
			s, err := settings()
			if err != nil {
				return err
			}
			st, err := openState(s)
			if err != nil {
				return err
			}
			defer st.Close()
			list, err := knownPayers(st, s)
			if err != nil {
				return err
			}
			known := map[string]model.Payer{}
			for _, p := range list {
				known[p.PayerID] = p
			}
			var targets []model.Payer
			if len(ids) > 0 {
				var missing []string
				for _, id := range ids {
					p, ok := known[id]
					if !ok {
						missing = append(missing, id)
						continue
					}
					targets = append(targets, p)
				}
				if len(missing) > 0 {
					return fmt.Errorf("unknown payer ids: %s", strings.Join(missing, ", "))
				}
				if tocFile != "" {
					targets[0].TocFile = tocFile
				}
			} else {
				for _, p := range known {
					if p.TocURL != "" || p.TocLinkPattern != "" || p.TocFile != "" {
						targets = append(targets, p)
					}
				}
				sort.Slice(targets, func(i, j int) bool {
					if targets[i].Priority != targets[j].Priority {
						return targets[i].Priority < targets[j].Priority
					}
					return targets[i].PayerID < targets[j].PayerID
				})
			}
			targetIDs := make([]string, len(targets))
			for i, p := range targets {
				targetIDs[i] = p.PayerID
			}
			sort.Strings(targetIDs)
			args := map[string]any{"payers": targetIDs, "raw": raw, "force": force}
			runID, resumed, err := st.OpenRun(args)
			if err != nil {
				return err
			}
			done, err := st.DonePayers(runID)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if resumed {
				fmt.Fprintf(w, "resuming run %s: %d payers already done\n", runID, len(done))
			}
			ctx := context.Background()
			client := newClient(s)
			day := today()
			base := &pipeline.TocCrawl{S: s, HTTP: client, State: st, OutDir: s.LocalDir, Force: force, Today: day}
			var steps pipeline.Steps = base
			if raw {
				store, err := storage.MakeRawStore(ctx, s.RawStore, nil)
				if err != nil {
					return err
				}
				steps = &pipeline.RawDownload{TocCrawl: base, Raw: store, MaxRunBytes: s.MaxRunBytes}
			}
			crawled, ok := 0, 0
			for _, p := range targets {
				if done[p.PayerID] {
					continue
				}
				if all && !force && recent(st, p.PayerID, s.MinRecrawlDays) {
					continue
				}
				fmt.Fprintf(w, "crawling %s ...\n", p.PayerID)
				r := pipeline.Run(ctx, steps, p, day)
				if err := st.RecordResult(runID, r); err != nil {
					return err
				}
				crawled++
				if r.Status == "ok" {
					ok++
				}
				fmt.Fprintf(w, "  %s mrfs=%d bytes=%d %s\n", r.Status, r.MrfCount, r.TotalBytes, r.Error)
			}
			if err := st.FinishRun(runID, "finished"); err != nil {
				return err
			}
			log, err := writeRunLog(s, st, runID, args)
			if err != nil {
				return err
			}
			if _, err := writeLocalRegistry(s, st, known); err != nil {
				return err
			}
			fmt.Fprintf(w, "run %s: %d crawled now, %d ok; log %s\n", runID, crawled, ok, log)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&ids, "payer", nil, "payer id (repeatable)")
	f.BoolVar(&all, "all", false, "crawl every payer with a TOC URL")
	f.BoolVar(&raw, "download-raw", false, "download raw MRFs to TIC_RAW_STORE (never Drive)")
	f.BoolVar(&force, "force", false, "ignore TOC hash and recrawl window")
	f.StringVar(&tocFile, "toc-file", "", "use a TOC saved by hand instead of fetching it (one --payer only)")
	return cmd
}

// recent reports whether the payer's latest good crawl is inside the recrawl window.
func recent(st *state.Store, id string, days int) bool {
	l, err := st.Latest(id)
	if err != nil || l == nil {
		return false
	}
	if status := str(l, "status"); status == "failed" || status == "deferred" {
		return false
	}
	t, err := time.Parse("2006-01-02T15:04:05Z", str(l, "crawled_at"))
	if err != nil {
		return false
	}
	return int(time.Since(t).Hours()/24) < days
}

func writeRunLog(s config.Settings, st *state.Store, runID string, args map[string]any) (string, error) {
	results, err := st.RunResults(runID)
	if err != nil {
		return "", err
	}
	byStatus := map[string]int{}
	var mrfURLs, mrfBytes, tocBytes int64
	for _, r := range results {
		byStatus[str(r, "status")]++
		if str(r, "status") == "ok" {
			mrfURLs += num(r, "mrf_count")
			mrfBytes += num(r, "total_bytes")
		}
		tocBytes += num(r, "toc_bytes")
	}
	log := map[string]any{"run_id": runID, "finished_at": time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"args": args, "schema_version": s.SchemaVersion, "results": results,
		"summary": map[string]any{"payers": len(results), "by_status": byStatus,
			"mrf_urls": mrfURLs, "mrf_bytes": mrfBytes, "toc_bytes": tocBytes}}
	path := filepath.Join(s.LocalDir, "runs", runID+".json")
	// encoding/json sorts map keys, matching the Python log's sort_keys.
	return path, writeJSONFile(path, log)
}

var registryColumns = []string{"payer_id", "legal_name", "hios_ids", "landing_url", "toc_url", "last_crawled",
	"toc_sha256", "mrf_count", "total_bytes", "schema_valid", "last_error"}

func registryRows(st *state.Store, payers map[string]model.Payer) ([][]any, error) {
	latest, err := st.AllLatest()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(payers))
	for id := range payers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var rows [][]any
	for _, id := range ids {
		p := payers[id]
		r := latest[id]
		if r == nil {
			r = map[string]any{}
		}
		tocURL := str(r, "toc_url")
		if tocURL == "" {
			tocURL = p.TocURL
		}
		valid := ""
		if b, ok := r["schema_valid"].(bool); ok {
			valid = strings.ToUpper(strconv.FormatBool(b))
		}
		lastErr := ""
		switch str(r, "status") {
		case "failed", "skipped", "deferred":
			lastErr = fmt.Sprintf("[%s] %s", str(r, "access_status"), str(r, "error"))
		}
		rows = append(rows, []any{id, p.LegalName, strings.Join(p.HIOSIDs, ","), p.LandingURL, tocURL,
			str(r, "crawled_at"), str(r, "toc_sha256"), num(r, "mrf_count"), num(r, "total_bytes"), valid, lastErr})
	}
	return rows, nil
}

func writeLocalRegistry(s config.Settings, st *state.Store, payers map[string]model.Payer) (string, error) {
	rows, err := registryRows(st, payers)
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.LocalDir, "registry.csv")
	if err := os.MkdirAll(s.LocalDir, 0o755); err != nil {
		return "", err
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.UseCRLF = true
	w.Write(registryColumns)
	for _, r := range rows {
		rec := make([]string, len(r))
		for i, v := range r {
			rec[i] = fmt.Sprint(v)
		}
		w.Write(rec)
	}
	w.Flush()
	return path, w.Error()
}

func verifySeedsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify-seeds",
		Short: "Check every landing and TOC URL in the seed file over HTTP and write a CSV report",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			payers, err := discovery.LoadSeedFile(s.SeedFile)
			if err != nil {
				return err
			}
			day := today()
			client := newClient(s)
			ctx := context.Background()
			var rows [][]string
			good := 0
			for _, p := range payers {
				type fu struct{ field, url string }
				urls := []fu{{"landing_url", p.LandingURL}}
				for _, u := range model.RenderTocURLs(p.TocURL, day) {
					urls = append(urls, fu{"toc_url", u})
				}
				for _, x := range urls {
					if x.url == "" {
						continue
					}
					c := client.HeadOrRange(ctx, x.url)
					status, length := "", ""
					if c.Status != 0 {
						status = strconv.Itoa(c.Status)
					}
					if c.ContentLength > 0 {
						length = strconv.FormatInt(c.ContentLength, 10)
					}
					if c.Error == "" {
						good++
					}
					rows = append(rows, []string{p.PayerID, x.field, x.url, status, c.Method, length, c.Error,
						day.Format("2006-01-02")})
				}
			}
			path := filepath.Join(s.LocalDir, "seed_verification.csv")
			if err := writeCSV(path, []string{"payer_id", "field", "url", "http_status", "method",
				"content_length", "error", "checked_on"}, rows); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "checked %d urls, %d ok; report %s\n", len(rows), good, path)
			return nil
		},
	}
}

func writeCSV(path string, header []string, rows [][]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.UseCRLF = true
	w.Write(header)
	w.WriteAll(rows)
	return w.Error()
}

// auditTargets lists each payer's landing page, first TOC URL, and up to mrfSamples MRF hosts
// from its latest manifest.
func auditTargets(s config.Settings, payers []model.Payer, mrfSamples int) []audit.Target {
	day := today()
	var out []audit.Target
	for _, p := range payers {
		if p.LandingURL != "" {
			out = append(out, audit.Target{PayerID: p.PayerID, Role: "landing", URL: p.LandingURL})
		}
		if u := model.RenderTocURLs(p.TocURL, day); len(u) > 0 {
			out = append(out, audit.Target{PayerID: p.PayerID, Role: "toc", URL: u[0]})
		}
		if mrfSamples <= 0 {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(s.LocalDir, "manifests", p.PayerID, "*.csv"))
		if len(files) == 0 {
			continue
		}
		sort.Strings(files)
		f, err := os.Open(files[len(files)-1])
		if err != nil {
			continue
		}
		r := csv.NewReader(f)
		r.FieldsPerRecord = -1
		header, err := r.Read()
		col := -1
		for i, h := range header {
			if h == "url" {
				col = i
			}
		}
		seen := map[string]bool{}
		for err == nil && col >= 0 && len(seen) < mrfSamples {
			var rec []string
			if rec, err = r.Read(); err != nil || col >= len(rec) {
				continue
			}
			u := rec[col]
			if !strings.HasPrefix(u, "https://") {
				continue
			}
			host := strings.SplitN(u, "/", 4)[2]
			if host != "" && !seen[host] {
				seen[host] = true
				out = append(out, audit.Target{PayerID: p.PayerID, Role: "mrf_sample", URL: u})
			}
		}
		f.Close()
	}
	return out
}

func auditAccessCmd() *cobra.Command {
	var samples int
	cmd := &cobra.Command{
		Use:   "audit-access",
		Short: "Record robots.txt rules and bot walls for every seed payer host",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			st, err := openState(s)
			if err != nil {
				return err
			}
			defer st.Close()
			payers, err := knownPayers(st, s)
			if err != nil {
				return err
			}
			rep := audit.Run(context.Background(), newClient(s), auditTargets(s, payers, samples))
			data := map[string]any{"audited_at": time.Now().UTC().Format("2006-01-02T15:04:05Z"),
				"user_agent": s.UserAgent(), "robots": rep.Robots, "probes": rep.Probes}
			path := filepath.Join(s.LocalDir, "access_audit.json")
			if err := writeJSONFile(path, data); err != nil {
				return err
			}
			verdicts := map[string]int{}
			for _, p := range rep.Probes {
				verdicts[p.Verdict]++
			}
			fmt.Fprintf(cmd.OutOrStdout(), "origins %d, urls %d: %v; report %s\n",
				len(rep.Robots), len(rep.Probes), verdicts, path)
			return nil
		},
	}
	cmd.Flags().IntVar(&samples, "mrf-samples", 2, "MRF hosts to sample per payer")
	return cmd
}

func probeCmd() *cobra.Command {
	var tocURL, landingURL, name, markdown string
	cmd := &cobra.Command{
		Use:   "probe",
		Short: "Check a requested payer: robots, bot walls, and (if reachable) TOC parse and validation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			pid := discovery.Slugify(name)
			var targets []audit.Target
			if landingURL != "" {
				targets = append(targets, audit.Target{PayerID: pid, Role: "landing", URL: landingURL})
			}
			if tocURL != "" {
				targets = append(targets, audit.Target{PayerID: pid, Role: "toc", URL: tocURL})
			}
			ctx := context.Background()
			client := newClient(s)
			rep := audit.Run(ctx, client, targets)
			var result *model.PayerResult
			if tocURL != "" {
				tmp, err := os.MkdirTemp("", "probe")
				if err != nil {
					return err
				}
				defer os.RemoveAll(tmp)
				st, err := state.Open(filepath.Join(tmp, "probe.sqlite"))
				if err != nil {
					return err
				}
				defer st.Close()
				ps := s
				ps.LocalDir = filepath.Join(tmp, "out")
				p := &pipeline.Probe{TocCrawl: &pipeline.TocCrawl{S: ps, HTTP: client, State: st,
					OutDir: ps.LocalDir, Today: today()}}
				r := pipeline.Run(ctx, p, model.Payer{PayerID: pid, LegalName: name, TocURL: tocURL,
					LandingURL: landingURL}, today())
				result = &r
			}
			text := probeMarkdown(name, pid, tocURL, landingURL, rep, result)
			if markdown != "" {
				if err := os.WriteFile(markdown, []byte(text), 0o644); err != nil {
					return err
				}
			}
			fmt.Fprintln(cmd.OutOrStdout(), text)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&tocURL, "toc-url", "", "TOC URL to test")
	f.StringVar(&landingURL, "landing-url", "", "landing page URL")
	f.StringVar(&name, "name", "Requested payer", "payer name")
	f.StringVar(&markdown, "markdown", "", "write a Markdown report here")
	return cmd
}

func pyBool(b *bool) string {
	if b == nil {
		return "None"
	}
	if *b {
		return "True"
	}
	return "False"
}

func probeMarkdown(name, pid, tocURL, landingURL string, rep audit.Report, r *model.PayerResult) string {
	lines := []string{"## Probe: " + name, "", "| Role | URL | robots.txt allows us | Status | Verdict |",
		"| --- | --- | --- | --- | --- |"}
	for _, p := range rep.Probes {
		status, wall := "", ""
		if p.Status != nil && *p.Status != 0 {
			status = strconv.Itoa(*p.Status)
		}
		if p.Wall != "" {
			wall = " (" + p.Wall + ")"
		}
		lines = append(lines, fmt.Sprintf("| %s | %s | %s | %s | %s%s |", p.Role, p.URL, pyBool(p.RobotsAllowsUs),
			status, p.Verdict, wall))
	}
	for _, rr := range rep.Robots {
		status := "None"
		if rr.Status != nil {
			status = strconv.Itoa(*rr.Status)
		}
		wall := ""
		if rr.Wall != "" {
			wall = ", wall " + rr.Wall
		}
		lines = append(lines, fmt.Sprintf("\nrobots.txt at %s: status %s, policy %s%s", rr.Origin, status, rr.Policy, wall))
	}
	if r != nil {
		var v *validate.Report
		if len(r.Validation) > 0 && string(r.Validation) != "null" {
			v = &validate.Report{}
			if json.Unmarshal(r.Validation, v) != nil {
				v = nil
			}
		}
		line := fmt.Sprintf("**TOC:** %s; %d bytes; %d unique MRF URLs; schema valid: %s",
			r.Status, r.TocBytes, r.MrfCount, pyBool(r.SchemaValid))
		if v != nil {
			line += fmt.Sprintf("; %d errors", v.ErrorCount)
		}
		lines = append(lines, "", line)
		if r.Error != "" {
			lines = append(lines, "**Error:** "+r.Error)
		}
		if v != nil {
			for i, k := range v.ErrorKinds {
				if i == 10 {
					break
				}
				lines = append(lines, fmt.Sprintf("- %d x `%s`", k.Count, k.Kind))
			}
		}
		if r.Status == "ok" {
			lines = append(lines, "", "Suggested seed entry:", "```yaml", "  - payer_id: "+pid,
				"    legal_name: "+name, "    landing_url: "+landingURL, "    toc_url: "+tocURL,
				"    verified_on: "+today().Format("2006-01-02"), "    verified_by: http", "```")
		}
	}
	return strings.Join(lines, "\n")
}

func shaFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Uploader is the part of the Drive adapter that sync uses.
type Uploader interface {
	PutFile(ctx context.Context, rel, local string) (string, error)
}

// syncOutputs uploads catalog files that are new or changed since the last sync.
func syncOutputs(ctx context.Context, s config.Settings, st *state.Store, up Uploader) (map[string]int, error) {
	counts := map[string]int{"uploaded": 0, "skipped": 0}
	for _, sub := range []string{"toc", "manifests", "runs"} {
		var files []string
		filepath.WalkDir(filepath.Join(s.LocalDir, sub), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				files = append(files, p)
			}
			return nil
		})
		sort.Strings(files)
		for _, p := range files {
			rel, _ := filepath.Rel(s.LocalDir, p)
			rel = filepath.ToSlash(rel)
			sha, err := shaFile(p)
			if err != nil {
				return counts, err
			}
			if prev, _ := st.UploadedSHA(rel); prev == sha {
				counts["skipped"]++
				continue
			}
			remote, err := up.PutFile(ctx, rel, p)
			if err != nil {
				return counts, err
			}
			if err := st.MarkUploaded(rel, sha, remote); err != nil {
				return counts, err
			}
			counts["uploaded"]++
		}
	}
	return counts, nil
}

func syncDriveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync-drive",
		Short: "Upload TOCs, manifests and run logs to Drive and update the registry Sheet",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			if s.CredentialsPath == "" {
				return errors.New("GOOGLE_APPLICATION_CREDENTIALS is not set")
			}
			st, err := openState(s)
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			dsvc, ssvc, err := storage.NewGoogle(ctx, s.CredentialsPath)
			if err != nil {
				return err
			}
			drive, err := storage.NewDrive(dsvc, s.DriveFolderID)
			if err != nil {
				return err
			}
			counts, err := syncOutputs(ctx, s, st, drive)
			if err != nil {
				return err
			}
			list, err := knownPayers(st, s)
			if err != nil {
				return err
			}
			payers := map[string]model.Payer{}
			for _, p := range list {
				payers[p.PayerID] = p
			}
			sheetID, err := drive.FindOrCreateSheet(ctx, s.SheetName)
			if err != nil {
				return err
			}
			rows, err := registryRows(st, payers)
			if err != nil {
				return err
			}
			upd, app, err := storage.NewSheetRegistry(ssvc, sheetID).Upsert(ctx, rows)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "drive: %v; sheet %s: updated %d, appended %d\n", counts, sheetID, upd, app)
			return nil
		},
	}
}

func reportCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Summarize the latest state of every payer, including staleness",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			st, err := openState(s)
			if err != nil {
				return err
			}
			defer st.Close()
			list, err := knownPayers(st, s)
			if err != nil {
				return err
			}
			latest, err := st.AllLatest()
			if err != nil {
				return err
			}
			day := today()
			staleDays := s.StaleDays(day)
			sort.Slice(list, func(i, j int) bool { return list[i].PayerID < list[j].PayerID })
			type row struct {
				PayerID          string `json:"payer_id"`
				Status           string `json:"status"`
				MrfCount         int64  `json:"mrf_count"`
				TotalBytes       int64  `json:"total_bytes"`
				SchemaValid      any    `json:"schema_valid"`
				LastCrawled      string `json:"last_crawled"`
				Error            string `json:"error"`
				TocLastUpdatedOn string `json:"toc_last_updated_on"`
				Stale            *bool  `json:"stale"`
			}
			rows := []row{}
			for _, p := range list {
				r := latest[p.PayerID]
				if r == nil {
					r = map[string]any{}
				}
				status := str(r, "status")
				if status == "" {
					status = "never"
				}
				x := row{PayerID: p.PayerID, Status: status, MrfCount: num(r, "mrf_count"),
					TotalBytes: num(r, "total_bytes"), SchemaValid: r["schema_valid"], LastCrawled: str(r, "crawled_at"),
					Error: str(r, "error"), TocLastUpdatedOn: str(r, "toc_last_updated_on")}
				if d, err := time.Parse("2006-01-02", x.TocLastUpdatedOn); err == nil {
					b := int(day.Sub(d).Hours()/24) > staleDays
					x.Stale = &b
				}
				rows = append(rows, x)
			}
			w := cmd.OutOrStdout()
			if asJSON {
				b, _ := json.MarshalIndent(rows, "", "  ")
				fmt.Fprintln(w, string(b))
				return nil
			}
			crawled, failed := 0, 0
			for _, r := range rows {
				if r.Status != "never" {
					crawled++
				}
				if r.Status == "failed" {
					failed++
				}
			}
			fmt.Fprintf(w, "payers %d, crawled %d, failed %d, stale threshold %dd\n", len(rows), crawled, failed, staleDays)
			for _, r := range rows {
				if r.Status == "never" {
					continue
				}
				e := r.Error
				if len(e) > 60 {
					e = e[:60]
				}
				valid := "None"
				if b, ok := r.SchemaValid.(bool); ok {
					valid = pyBool(&b)
				}
				fmt.Fprintf(w, "%-40s %-9s mrfs=%-7d bytes=%-15d valid=%s stale=%s %s\n",
					r.PayerID, r.Status, r.MrfCount, r.TotalBytes, valid, pyBool(r.Stale), e)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
