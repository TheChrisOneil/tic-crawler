// Package pipeline is the crawl (Template Method): Run fixes the order of steps and a Steps
// implementation fills them in. TocCrawl is HEAD-only; RawDownload adds raw downloads; Probe
// parses and validates without checking MRF URLs.
package pipeline

import (
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TheChrisOneil/tic-crawler/internal/config"
	"github.com/TheChrisOneil/tic-crawler/internal/fetch"
	"github.com/TheChrisOneil/tic-crawler/internal/model"
	"github.com/TheChrisOneil/tic-crawler/internal/state"
	"github.com/TheChrisOneil/tic-crawler/internal/toc"
	"github.com/TheChrisOneil/tic-crawler/internal/validate"
)

var ManifestColumns = []string{"payer_id", "toc_url", "toc_sha256", "toc_last_updated_on", "toc_file_version",
	"pinned_schema_version", "toc_schema_valid", "toc_validation_error_count", "toc_validation_errors", "file_type",
	"url", "description", "plan_count", "plan_names", "plans_truncated", "plan_id_types", "plan_market_types",
	"http_status", "check_method", "content_length", "last_modified", "etag", "check_error"}

// SkipPayer ends the run for a payer without marking it failed.
type SkipPayer struct{ Msg, AccessStatus string }

func (e *SkipPayer) Error() string { return e.Msg }

// AccessProblem means the TOC could not be reached. Deferred problems are retried next run.
type AccessProblem struct {
	AccessStatus string
	Msg          string
	HTTPStatus   int
}

func (e *AccessProblem) Error() string { return e.Msg }

// Most specific first: when several candidate URLs fail, the first match here wins.
var problemPriority = []string{"robots_disallow", "waf_blocked", "robots_unreachable", "network_error", "http_error"}

func (e *AccessProblem) Deferred() bool {
	switch e.AccessStatus {
	case "robots_unreachable", "network_error":
		return true
	case "http_error":
		return e.HTTPStatus == 408 || e.HTTPStatus == 429 || e.HTTPStatus >= 500
	}
	return false
}

// Classify maps a fetch error to an access_status and HTTP status.
func Classify(err error) (string, int) {
	var rd *fetch.RobotsDisallowed
	if errors.As(err, &rd) {
		if rd.Unreachable {
			return "robots_unreachable", 0
		}
		return "robots_disallow", 0
	}
	var fe *fetch.FetchError
	if errors.As(err, &fe) {
		switch {
		case fe.Wall != "":
			return "waf_blocked", fe.Status
		case fe.Status == 0:
			return "network_error", 0
		}
		return "http_error", fe.Status
	}
	return "network_error", 0
}

type Entry struct {
	*toc.MrfEntry
	Check fetch.UrlCheck
}

type Context struct {
	Payer      model.Payer
	RunDate    time.Time
	WorkDir    string
	Result     *model.PayerResult
	TocPath    string
	Header     toc.GenericHeader
	Validation *validate.Report
	Entries    []*Entry
}

// Steps are the hooks a pipeline fills in.
type Steps interface {
	FetchTOC(ctx context.Context, c *Context) error
	ParseAndValidate(c *Context) error
	IsUnchanged(c *Context) bool
	CheckMRFs(ctx context.Context, c *Context) error
	AfterChecks(ctx context.Context, c *Context) error
	WriteOutputs(c *Context) error
}

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

// Run is the template method: it owns the order of steps and maps errors to statuses.
func Run(ctx context.Context, steps Steps, payer model.Payer, today time.Time) model.PayerResult {
	r := model.NewPayerResult(payer.PayerID)
	r.CrawledAt = now()
	tmp, err := os.MkdirTemp("", "tic-")
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
		return r
	}
	defer os.RemoveAll(tmp)
	c := &Context{Payer: payer, RunDate: today, WorkDir: tmp, Result: &r}
	err = func() error {
		if err := steps.FetchTOC(ctx, c); err != nil {
			return err
		}
		if err := steps.ParseAndValidate(c); err != nil {
			return err
		}
		if steps.IsUnchanged(c) {
			r.Status = "unchanged"
			return nil
		}
		if err := steps.CheckMRFs(ctx, c); err != nil {
			return err
		}
		if err := steps.AfterChecks(ctx, c); err != nil {
			return err
		}
		if err := steps.WriteOutputs(c); err != nil {
			return err
		}
		r.Status = "ok"
		return nil
	}()
	var sp *SkipPayer
	var ap *AccessProblem
	switch {
	case err == nil:
		if r.AccessStatus == "" {
			r.AccessStatus = "ok"
		}
	case errors.As(err, &sp):
		r.Status, r.Error, r.AccessStatus = "skipped", sp.Msg, sp.AccessStatus
	case errors.As(err, &ap):
		r.Status = "failed"
		if ap.Deferred() {
			r.Status = "deferred"
		}
		r.AccessStatus = ap.AccessStatus
		r.Error = truncate(ap.Msg, 500)
	default:
		r.Status, r.Error = "failed", truncate(fmt.Sprintf("%T: %v", err, err), 500)
	}
	return r
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------- TOC crawl ----------

type TocCrawl struct {
	S      config.Settings
	HTTP   *fetch.Client
	State  *state.Store
	OutDir string
	Force  bool
	Today  time.Time
}

type tooLarge struct{ msg string }

func (e *tooLarge) Error() string { return e.msg }

func (p *TocCrawl) FetchTOC(ctx context.Context, c *Context) error {
	if c.Payer.TocFile != "" {
		return p.useManualTOC(c)
	}
	candidates := model.RenderTocURLs(c.Payer.TocURL, p.Today)
	if len(candidates) == 0 && c.Payer.TocLinkPattern != "" && c.Payer.LandingURL != "" {
		var err error
		candidates, err = p.ScrapeTOCLinks(ctx, c.Payer)
		if err != nil {
			st, code := Classify(err)
			return &AccessProblem{st, "landing page: " + err.Error(), code}
		}
	}
	if len(candidates) == 0 {
		return &SkipPayer{"no_toc_url", "no_toc_url"}
	}
	var problems []*AccessProblem
	for _, u := range candidates {
		path := filepath.Join(c.WorkDir, "toc.raw")
		size, err := p.download(ctx, u, path)
		var tl *tooLarge
		if errors.As(err, &tl) {
			return &SkipPayer{"toc_too_large: " + tl.msg, "too_large"}
		}
		if err != nil {
			st, code := Classify(err)
			label := st
			var rd *fetch.RobotsDisallowed
			if errors.As(err, &rd) {
				label = "robots_disallowed"
			}
			wall := ""
			var fe *fetch.FetchError
			if errors.As(err, &fe) && fe.Wall != "" {
				wall = " [" + fe.Wall + "]"
			}
			problems = append(problems, &AccessProblem{st, label + " " + err.Error() + wall, code})
			continue
		}
		c.TocPath, c.Result.TocURL, c.Result.TocBytes = path, u, size
		return nil
	}
	rank := func(s string) int {
		for i, x := range problemPriority {
			if x == s {
				return i
			}
		}
		return len(problemPriority)
	}
	sort.SliceStable(problems, func(i, j int) bool { return rank(problems[i].AccessStatus) < rank(problems[j].AccessStatus) })
	msgs := make([]string, len(problems))
	for i, pr := range problems {
		msgs[i] = pr.Msg
	}
	top := problems[0]
	return &AccessProblem{top.AccessStatus, truncate(strings.Join(msgs, "; "), 400), top.HTTPStatus}
}

func (p *TocCrawl) download(ctx context.Context, u, path string) (int64, error) {
	resp, err := p.HTTP.Get(ctx, u)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil && n > p.S.MaxTOCBytes {
		return 0, &tooLarge{fmt.Sprintf("TOC is %d bytes, over max_toc_bytes", n)}
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, p.S.MaxTOCBytes+1))
	if err != nil {
		return n, &fetch.FetchError{URL: u, Message: err.Error()}
	}
	if n > p.S.MaxTOCBytes {
		return n, &tooLarge{"TOC exceeded max_toc_bytes while streaming"}
	}
	return n, nil
}

func (p *TocCrawl) useManualTOC(c *Context) error {
	src := c.Payer.TocFile
	st, err := os.Stat(src)
	if err != nil || st.IsDir() {
		return &SkipPayer{"manual TOC file not found: " + src, "no_toc_url"}
	}
	if st.Size() > p.S.MaxTOCBytes {
		return &SkipPayer{fmt.Sprintf("toc_too_large: manual file is %d bytes", st.Size()), "too_large"}
	}
	path := filepath.Join(c.WorkDir, "toc.raw")
	if err := copyFile(src, path); err != nil {
		return err
	}
	c.TocPath = path
	c.Result.TocURL = "manual:" + filepath.ToSlash(src)
	c.Result.TocBytes = st.Size()
	c.Result.AccessStatus = "manual"
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ScrapeTOCLinks finds TOC links on the landing page, newest first (dated names sort).
func (p *TocCrawl) ScrapeTOCLinks(ctx context.Context, payer model.Payer) ([]string, error) {
	page, err := p.HTTP.GetText(ctx, payer.LandingURL, 20<<20)
	if err != nil {
		return nil, err
	}
	rx, err := regexp.Compile(payer.TocLinkPattern)
	if err != nil {
		return nil, err
	}
	base, _ := url.Parse(payer.LandingURL)
	set := map[string]bool{}
	for _, m := range rx.FindAllString(page, -1) {
		ref, err := url.Parse(html.UnescapeString(m))
		if err != nil {
			continue
		}
		set[base.ResolveReference(ref).String()] = true
	}
	var out []string
	for u := range set {
		out = append(out, u)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	if len(out) > 3 {
		out = out[:3]
	}
	return out, nil
}

func openJSON(path string) (io.Reader, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	r, err := toc.MaybeGunzip(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return r, f.Close, nil
}

// SHA256OfJSON hashes the decompressed TOC, so a gzip re-wrap does not look like a change.
func SHA256OfJSON(path string) (string, error) {
	r, closeFn, err := openJSON(path)
	if err != nil {
		return "", err
	}
	defer closeFn()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (p *TocCrawl) ParseAndValidate(c *Context) error {
	sha, err := SHA256OfJSON(c.TocPath)
	if err != nil {
		return err
	}
	c.Result.TocSHA256 = sha
	v, err := validate.New(p.S.SchemaVersion, p.S.MaxErrorsPerTOC)
	if err != nil {
		return err
	}
	col := toc.NewMrfCollector(p.S.ManifestPlanNameCap)
	r, closeFn, err := openJSON(c.TocPath)
	if err != nil {
		return err
	}
	h, perr := toc.ReadGeneric(r, func(i int, item any) error {
		v.ValidateItem(i, item)
		col.AddItem(item)
		return nil
	})
	closeFn()
	c.Header = h
	if perr != nil { // malformed JSON is a validation result, not a crash
		v.AddError(fmt.Sprintf("$: JSON parse error: %v", perr))
	} else {
		v.ValidateHeader(h)
	}
	rep := v.Finish()
	c.Validation = &rep
	c.Result.Validation = marshal(rep)
	valid := rep.Valid
	c.Result.SchemaValid = &valid
	c.Result.MrfCount = col.Len()
	if lu, ok := h.Fields["last_updated_on"].(string); ok {
		c.Result.TocLastUpdatedOn = lu
	}
	c.Entries = nil
	for _, e := range col.Entries() {
		c.Entries = append(c.Entries, &Entry{MrfEntry: e})
	}
	return nil
}

func (p *TocCrawl) IsUnchanged(c *Context) bool {
	if p.Force {
		return false
	}
	sha, _ := p.State.LatestSHA(c.Payer.PayerID)
	return sha != "" && sha == c.Result.TocSHA256
}

func (p *TocCrawl) CheckMRFs(ctx context.Context, c *Context) error {
	var wg sync.WaitGroup
	for _, e := range c.Entries {
		if !strings.HasPrefix(strings.ToLower(e.URL), "https://") {
			// Placeholders like "Missing file" appear in real TOCs; never request them.
			e.Check = fetch.UrlCheck{Error: "invalid_url"}
			continue
		}
		wg.Add(1)
		go func(e *Entry) {
			defer wg.Done()
			e.Check = p.HTTP.HeadOrRange(ctx, e.URL)
		}(e)
	}
	wg.Wait()
	var total int64
	for _, e := range c.Entries {
		total += e.Check.ContentLength
	}
	c.Result.TotalBytes = total
	return nil
}

func (p *TocCrawl) AfterChecks(context.Context, *Context) error { return nil }

func (p *TocCrawl) WriteOutputs(c *Context) error {
	pid, day := c.Payer.PayerID, c.RunDate.Format("2006-01-02")
	tocRel := "toc/" + pid + "/" + day + ".json.gz"
	if err := WriteGzipCopy(c.TocPath, filepath.Join(p.OutDir, tocRel)); err != nil {
		return err
	}
	manRel := "manifests/" + pid + "/" + day + ".csv"
	if err := WriteManifest(c, filepath.Join(p.OutDir, manRel)); err != nil {
		return err
	}
	c.Result.Outputs = []string{tocRel, manRel}
	return nil
}

// WriteGzipCopy stores the TOC as .json.gz. Already-gzipped input is copied byte for byte.
func WriteGzipCopy(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	magic := make([]byte, 2)
	n, _ := io.ReadFull(f, magic)
	if n == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		return copyFile(src, dst)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	gz, _ := gzip.NewWriterLevel(out, gzip.BestCompression) // no name, zero mtime: deterministic
	if _, err := io.Copy(gz, f); err != nil {
		out.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// WriteManifest writes one row per unique MRF URL (the Python crawler's CSV layout).
func WriteManifest(c *Context, path string) error {
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
	w.Write(ManifestColumns)
	v := c.Validation
	lu := ""
	if s, ok := c.Header.Fields["last_updated_on"].(string); ok {
		lu = s
	}
	errs := v.Errors
	if len(errs) > 5 {
		errs = errs[:5]
	}
	base := []string{c.Payer.PayerID, c.Result.TocURL, c.Result.TocSHA256, lu, v.FileVersion, v.PinnedVersion,
		pyBool(v.Valid), strconv.Itoa(v.ErrorCount), strings.Join(errs, " | ")}
	entries := append([]*Entry(nil), c.Entries...)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].FileType != entries[j].FileType {
			return entries[i].FileType < entries[j].FileType
		}
		return entries[i].URL < entries[j].URL
	})
	if len(entries) == 0 {
		w.Write(append(base, make([]string, len(ManifestColumns)-len(base))...))
	}
	for _, e := range entries {
		ck := e.Check
		status, length := "", ""
		if ck.Status != 0 {
			status = strconv.Itoa(ck.Status)
		}
		if ck.HasLength {
			length = strconv.FormatInt(ck.ContentLength, 10)
		}
		w.Write(append(append([]string(nil), base...), e.FileType, e.URL, strings.Join(e.Descriptions, " | "),
			strconv.Itoa(e.PlanCount), strings.Join(e.PlanNames, " | "), pyBool(e.PlansTruncated),
			strings.Join(e.PlanIDTypes, "|"), strings.Join(e.PlanMarketTypes, "|"), status, ck.Method, length,
			ck.LastModified, ck.ETag, ck.Error))
	}
	w.Flush()
	return w.Error()
}

// ---------- raw download ----------

// RawStore receives raw MRFs (GCS or local; never Drive).
type RawStore interface {
	OpenWriter(rel string) (io.WriteCloser, error)
}

type RawDownload struct {
	*TocCrawl
	Raw         RawStore
	MaxRunBytes int64
	BytesUsed   int64
}

func (p *RawDownload) AfterChecks(ctx context.Context, c *Context) error {
	estimate := c.Result.TotalBytes
	if p.BytesUsed+estimate > p.MaxRunBytes {
		return &SkipPayer{Msg: fmt.Sprintf("raw_download_over_cap: needs %d bytes, %d left in this run", estimate, p.MaxRunBytes-p.BytesUsed)}
	}
	for _, e := range c.Entries {
		if e.Check.Error != "" {
			continue
		}
		sum := sha1.Sum([]byte(e.URL))
		rel := fmt.Sprintf("raw/%s/%s/%s", c.Payer.PayerID, c.RunDate.Format("2006-01-02"), hex.EncodeToString(sum[:])[:16])
		if err := p.one(ctx, c, e.URL, rel); err != nil {
			return err
		}
	}
	return nil
}

func (p *RawDownload) one(ctx context.Context, c *Context, u, rel string) error {
	resp, err := p.HTTP.Get(ctx, u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	w, err := p.Raw.OpenWriter(rel)
	if err != nil {
		return err
	}
	defer w.Close()
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			p.BytesUsed += int64(n)
			c.Result.RawBytes += int64(n)
			if p.BytesUsed > p.MaxRunBytes {
				return &SkipPayer{Msg: "raw_download_over_cap while streaming"}
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// ---------- probe ----------

// Probe fetches, parses and validates a TOC without checking MRF URLs (for payer requests).
type Probe struct{ *TocCrawl }

func (p *Probe) IsUnchanged(*Context) bool                 { return false }
func (p *Probe) CheckMRFs(context.Context, *Context) error { return nil }

func marshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
