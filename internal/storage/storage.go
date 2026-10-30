// Package storage holds the storage backends (Strategy) and the Drive, Sheets and GCS adapters.
// Drive holds the catalog only: TOCs, manifests, run logs and the registry Sheet. Raw
// in-network and allowed-amount files never go to Drive.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

const (
	FolderMIME = "application/vnd.google-apps.folder"
	SheetMIME  = "application/vnd.google-apps.spreadsheet"
)

var mimeBySuffix = map[string]string{".gz": "application/gzip", ".csv": "text/csv", ".json": "application/json"}

// Backend stores catalog files at POSIX-style relative paths like toc/aetna/2026-10-29.json.gz.
type Backend interface {
	PutFile(ctx context.Context, rel, local string) (string, error)
}

type Local struct{ Root string }

func (l Local) PutFile(_ context.Context, rel, local string) (string, error) {
	dest := filepath.Join(l.Root, filepath.FromSlash(rel))
	a, _ := filepath.Abs(dest)
	b, _ := filepath.Abs(local)
	if a == b {
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	in, err := os.Open(local)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return "", err
	}
	return dest, out.Close()
}

// ---------- Google API helpers ----------

// Sleep is overridable in tests.
var Sleep = time.Sleep

func retryable(err error) bool {
	var ge *googleapi.Error
	if !errors.As(err, &ge) {
		return false
	}
	switch ge.Code {
	case 429, 500, 502, 503, 504:
		return true
	case 403:
		text := strings.ToLower(ge.Message + " " + ge.Body)
		for _, e := range ge.Errors {
			text += " " + strings.ToLower(e.Reason)
		}
		return strings.Contains(text, "ratelimitexceeded") || strings.Contains(text, "userratelimitexceeded") || strings.Contains(text, "quota")
	}
	return false
}

// WithBackoff runs fn, backing off on 403 rate-limit and 429/5xx responses.
func WithBackoff(maxRetries int, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || attempt == maxRetries || !retryable(err) {
			return err
		}
		d := float64(int(1) << attempt)
		if d > 64 {
			d = 64
		}
		Sleep(time.Duration((d + rand.Float64()) * float64(time.Second)))
	}
}

// NewGoogle builds service-account clients for Drive v3 and Sheets v4.
func NewGoogle(ctx context.Context, credentialsPath string, extra ...option.ClientOption) (*drive.Service, *sheets.Service, error) {
	opts := append([]option.ClientOption{option.WithCredentialsFile(credentialsPath),
		option.WithScopes(drive.DriveScope, sheets.SpreadsheetsScope)}, extra...)
	d, err := drive.NewService(ctx, opts...)
	if err != nil {
		return nil, nil, err
	}
	s, err := sheets.NewService(ctx, opts...)
	return d, s, err
}

func q(v string) string { return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) }

// Drive adapts Backend to the Drive v3 API. It works in Shared Drives.
type Drive struct {
	svc     *drive.Service
	root    string
	folders map[string]string
}

func NewDrive(svc *drive.Service, rootFolderID string) (*Drive, error) {
	if rootFolderID == "" {
		return nil, errors.New("TIC_DRIVE_FOLDER_ID is not set")
	}
	return &Drive{svc: svc, root: rootFolderID, folders: map[string]string{"": rootFolderID}}, nil
}

func (d *Drive) find(ctx context.Context, parent, name, mime string) (string, error) {
	query := fmt.Sprintf("'%s' in parents and name = '%s' and trashed = false", q(parent), q(name))
	if mime != "" {
		query += fmt.Sprintf(" and mimeType = '%s'", mime)
	}
	var res *drive.FileList
	err := WithBackoff(6, func() (err error) {
		res, err = d.svc.Files.List().Q(query).Fields("files(id, name)").PageSize(10).
			SupportsAllDrives(true).IncludeItemsFromAllDrives(true).Context(ctx).Do()
		return err
	})
	if err != nil || len(res.Files) == 0 {
		return "", err
	}
	return res.Files[0].Id, nil
}

func (d *Drive) FolderID(ctx context.Context, rel string) (string, error) {
	rel = strings.Trim(rel, "/")
	if id, ok := d.folders[rel]; ok {
		return id, nil
	}
	parentRel, name := "", rel
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		parentRel, name = rel[:i], rel[i+1:]
	}
	parent, err := d.FolderID(ctx, parentRel)
	if err != nil {
		return "", err
	}
	id, err := d.find(ctx, parent, name, FolderMIME)
	if err != nil {
		return "", err
	}
	if id == "" {
		var f *drive.File
		err = WithBackoff(6, func() (err error) {
			f, err = d.svc.Files.Create(&drive.File{Name: name, MimeType: FolderMIME, Parents: []string{parent}}).
				Fields("id").SupportsAllDrives(true).Context(ctx).Do()
			return err
		})
		if err != nil {
			return "", err
		}
		id = f.Id
	}
	d.folders[rel] = id
	return id, nil
}

var ErrRawInDrive = errors.New("raw MRF files are never stored in Drive")

func (d *Drive) PutFile(ctx context.Context, rel, local string) (string, error) {
	rel = path.Clean(strings.TrimPrefix(rel, "/"))
	if rel == "raw" || strings.HasPrefix(rel, "raw/") {
		return "", ErrRawInDrive
	}
	dir, name := path.Split(rel)
	parent, err := d.FolderID(ctx, dir)
	if err != nil {
		return "", err
	}
	mime := mimeBySuffix[path.Ext(name)]
	if mime == "" {
		mime = "application/octet-stream"
	}
	existing, err := d.find(ctx, parent, name, "")
	if err != nil {
		return "", err
	}
	var f *drive.File
	err = WithBackoff(6, func() error {
		in, err := os.Open(local)
		if err != nil {
			return err
		}
		defer in.Close()
		media := googleapi.ContentType(mime)
		if existing != "" {
			f, err = d.svc.Files.Update(existing, &drive.File{}).Media(in, media).Fields("id").SupportsAllDrives(true).Context(ctx).Do()
		} else {
			f, err = d.svc.Files.Create(&drive.File{Name: name, Parents: []string{parent}}).Media(in, media).
				Fields("id").SupportsAllDrives(true).Context(ctx).Do()
		}
		return err
	})
	if err != nil {
		return "", err
	}
	return f.Id, nil
}

func (d *Drive) FindOrCreateSheet(ctx context.Context, name string) (string, error) {
	id, err := d.find(ctx, d.root, name, SheetMIME)
	if err != nil || id != "" {
		return id, err
	}
	var f *drive.File
	err = WithBackoff(6, func() (err error) {
		f, err = d.svc.Files.Create(&drive.File{Name: name, MimeType: SheetMIME, Parents: []string{d.root}}).
			Fields("id").SupportsAllDrives(true).Context(ctx).Do()
		return err
	})
	if err != nil {
		return "", err
	}
	return f.Id, nil
}

// ---------- registry Sheet ----------

var RegistryColumns = []string{"payer_id", "legal_name", "hios_ids", "landing_url", "toc_url", "last_crawled",
	"toc_sha256", "mrf_count", "total_bytes", "schema_valid", "last_error"}

// SheetRegistry is the TiC Payer Registry Sheet: one row per payer, updated in place by payer_id.
type SheetRegistry struct {
	svc *sheets.Service
	ID  string
}

func NewSheetRegistry(svc *sheets.Service, id string) *SheetRegistry { return &SheetRegistry{svc, id} }

// Upsert writes rows (values in RegistryColumns order; the first is payer_id).
func (s *SheetRegistry) Upsert(ctx context.Context, rows [][]any) (updated, appended int, err error) {
	var meta *sheets.Spreadsheet
	if err = WithBackoff(6, func() (err error) {
		meta, err = s.svc.Spreadsheets.Get(s.ID).Fields("sheets.properties.title").Context(ctx).Do()
		return err
	}); err != nil {
		return 0, 0, err
	}
	tab := "Sheet1"
	if len(meta.Sheets) > 0 {
		tab = meta.Sheets[0].Properties.Title
	}
	var cur *sheets.ValueRange
	if err = WithBackoff(6, func() (err error) {
		cur, err = s.svc.Spreadsheets.Values.Get(s.ID, fmt.Sprintf("'%s'!A:A", tab)).Context(ctx).Do()
		return err
	}); err != nil {
		return 0, 0, err
	}
	index := map[string]int{}
	for i, r := range cur.Values {
		if i > 0 && len(r) > 0 {
			if id := fmt.Sprint(r[0]); id != "" {
				index[id] = i + 1
			}
		}
	}
	next := len(cur.Values)
	if next < 1 {
		next = 1
	}
	next++
	header := make([]any, len(RegistryColumns))
	for i, c := range RegistryColumns {
		header[i] = c
	}
	data := []*sheets.ValueRange{{Range: fmt.Sprintf("'%s'!A1", tab), Values: [][]any{header}}}
	for _, row := range rows {
		id := fmt.Sprint(row[0])
		r, ok := index[id]
		if ok {
			updated++
		} else {
			r = next
			next++
			index[id] = r
			appended++
		}
		data = append(data, &sheets.ValueRange{Range: fmt.Sprintf("'%s'!A%d", tab, r), Values: [][]any{row}})
	}
	err = WithBackoff(6, func() error {
		_, err := s.svc.Spreadsheets.Values.BatchUpdate(s.ID, &sheets.BatchUpdateValuesRequest{ValueInputOption: "RAW", Data: data}).Context(ctx).Do()
		return err
	})
	return updated, appended, err
}

// ---------- raw MRF stores ----------

type LocalRaw struct{ Root string }

func (l LocalRaw) OpenWriter(rel string) (io.WriteCloser, error) {
	dest := filepath.Join(l.Root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, err
	}
	return os.Create(dest)
}

type GCSRaw struct {
	Bucket *gcs.BucketHandle
	Prefix string
}

func (g GCSRaw) OpenWriter(rel string) (io.WriteCloser, error) {
	name := rel
	if g.Prefix != "" {
		name = g.Prefix + "/" + rel
	}
	return g.Bucket.Object(name).NewWriter(context.Background()), nil
}

// RawStore receives raw MRFs.
type RawStore interface {
	OpenWriter(rel string) (io.WriteCloser, error)
}

// MakeRawStore parses TIC_RAW_STORE: gcs://bucket/prefix (or gs://) or a local path. Drive
// targets are refused.
func MakeRawStore(ctx context.Context, spec string, hc *http.Client) (RawStore, error) {
	if spec == "" {
		return nil, errors.New("TIC_RAW_STORE is not set; raw download needs gcs://bucket or a local path")
	}
	low := strings.ToLower(spec)
	if strings.Contains(low, "drive.google") || strings.HasPrefix(low, "drive:") || strings.HasPrefix(low, "gdrive:") {
		return nil, errors.New("raw MRF files must never be stored in Google Drive")
	}
	if strings.HasPrefix(low, "gcs://") || strings.HasPrefix(low, "gs://") {
		rest := spec[strings.Index(spec, "://")+3:]
		bucket, prefix, _ := strings.Cut(rest, "/")
		var opts []option.ClientOption
		if hc != nil {
			opts = append(opts, option.WithHTTPClient(hc))
		}
		c, err := gcs.NewClient(ctx, opts...)
		if err != nil {
			return nil, err
		}
		return GCSRaw{Bucket: c.Bucket(bucket), Prefix: strings.Trim(prefix, "/")}, nil
	}
	return LocalRaw{Root: spec}, nil
}
