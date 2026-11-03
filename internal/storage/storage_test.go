package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// fakeGoogle is a tiny in-memory Drive + Sheets.
type fakeGoogle struct {
	mu        sync.Mutex
	files     map[string]map[string]string // id -> name, parent, mime
	next      int
	uploads   map[string]string // id -> content
	sheetRows [][]any
	batches   []map[string]any
	rateLimit int // respond 403 rateLimitExceeded this many times first
}

func (f *fakeGoogle) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rateLimit > 0 {
		f.rateLimit--
		w.WriteHeader(403)
		io.WriteString(w, `{"error":{"code":403,"message":"Rate Limit Exceeded","errors":[{"reason":"userRateLimitExceeded"}]}}`)
		return
	}
	p := r.URL.Path
	switch {
	case r.Method == "GET" && strings.HasSuffix(p, "/files"):
		qs := r.URL.Query().Get("q")
		var out []map[string]string
		for id, fl := range f.files {
			if strings.Contains(qs, "'"+fl["parent"]+"' in parents") && strings.Contains(qs, "name = '"+fl["name"]+"'") &&
				(!strings.Contains(qs, "mimeType") || strings.Contains(qs, fl["mime"])) {
				out = append(out, map[string]string{"id": id, "name": fl["name"]})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"files": out})
	case r.Method == "POST" && strings.HasSuffix(p, "/files"):
		f.next++
		id := fmt.Sprintf("id%d", f.next)
		meta, content := parseUpload(r)
		parent := ""
		if ps, ok := meta["parents"].([]any); ok && len(ps) > 0 {
			parent = ps[0].(string)
		}
		mime, _ := meta["mimeType"].(string)
		f.files[id] = map[string]string{"name": meta["name"].(string), "parent": parent, "mime": mime}
		if content != "" {
			f.uploads[id] = content
		}
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	case r.Method == "PATCH" && strings.Contains(p, "/files/"):
		id := p[strings.LastIndex(p, "/")+1:]
		_, content := parseUpload(r)
		f.uploads[id] = content
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	case r.Method == "GET" && strings.Contains(p, "/values/"):
		json.NewEncoder(w).Encode(map[string]any{"values": f.sheetRows})
	case r.Method == "GET" && strings.Contains(p, "/spreadsheets/"):
		io.WriteString(w, `{"sheets":[{"properties":{"title":"Registry"}}]}`)
	case r.Method == "POST" && strings.HasSuffix(p, ":batchUpdate"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.batches = append(f.batches, body)
		io.WriteString(w, `{}`)
	default:
		w.WriteHeader(404)
	}
}

func parseUpload(r *http.Request) (map[string]any, string) {
	meta := map[string]any{}
	ct := r.Header.Get("Content-Type")
	body, _ := io.ReadAll(r.Body)
	if !strings.HasPrefix(ct, "multipart/") {
		if strings.HasPrefix(ct, "application/json") {
			json.Unmarshal(body, &meta)
			return meta, ""
		}
		return meta, string(body)
	}
	boundary := ct[strings.Index(ct, "boundary=")+9:]
	parts := strings.Split(string(body), "--"+boundary)
	content := ""
	for _, part := range parts {
		head, data, ok := strings.Cut(part, "\r\n\r\n")
		if !ok {
			continue
		}
		data = strings.TrimSuffix(data, "\r\n")
		if strings.Contains(head, "application/json") {
			json.Unmarshal([]byte(data), &meta)
		} else {
			content = data
		}
	}
	return meta, content
}

func setup(t *testing.T) (*fakeGoogle, *Drive, *SheetRegistry) {
	f := &fakeGoogle{files: map[string]map[string]string{}, uploads: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	Sleep = func(time.Duration) {}
	ctx := context.Background()
	opts := []option.ClientOption{option.WithEndpoint(srv.URL), option.WithoutAuthentication(), option.WithHTTPClient(srv.Client())}
	ds, err := drive.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := sheets.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDrive(ds, "root")
	if err != nil {
		t.Fatal(err)
	}
	return f, d, NewSheetRegistry(ss, "sheet1")
}

func TestDrivePutFileCreatesFoldersAndUpdates(t *testing.T) {
	f, d, _ := setup(t)
	local := filepath.Join(t.TempDir(), "a.csv")
	os.WriteFile(local, []byte("v1"), 0o644)
	ctx := context.Background()
	id1, err := d.PutFile(ctx, "manifests/acme/2026-10-29.csv", local)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(local, []byte("v2"), 0o644)
	d2, _ := NewDrive(d.svc, "root") // fresh folder cache: folders are found, not recreated
	id2, err := d2.PutFile(ctx, "manifests/acme/2026-10-29.csv", local)
	if err != nil || id1 != id2 || f.uploads[id1] != "v2" {
		t.Fatal(id1, id2, err, f.uploads)
	}
	folders := 0
	for _, fl := range f.files {
		if fl["mime"] == FolderMIME {
			folders++
		}
	}
	if folders != 2 {
		t.Fatal(f.files)
	}
	if _, err := d.PutFile(ctx, "raw/acme/x", local); !errors.Is(err, ErrRawInDrive) {
		t.Fatal(err)
	}
	if _, err := NewDrive(nil, ""); err == nil {
		t.Fatal("want error")
	}
}

func TestSheetCreateAndUpsertWithRateLimit(t *testing.T) {
	f, d, s := setup(t)
	ctx := context.Background()
	f.rateLimit = 2
	sid, err := d.FindOrCreateSheet(ctx, "TiC Payer Registry")
	if err != nil || sid == "" {
		t.Fatal(sid, err)
	}
	if again, _ := d.FindOrCreateSheet(ctx, "TiC Payer Registry"); again != sid {
		t.Fatal(again)
	}
	f.sheetRows = [][]any{{"payer_id"}, {"acme"}}
	up, app, err := s.Upsert(ctx, [][]any{{"acme", "Acme"}, {"zeta", "Zeta"}})
	if err != nil || up != 1 || app != 1 {
		t.Fatal(up, app, err)
	}
	data := f.batches[0]["data"].([]any)
	if data[1].(map[string]any)["range"] != "'Registry'!A2" || data[2].(map[string]any)["range"] != "'Registry'!A3" {
		t.Fatal(data)
	}
}

func TestRetryable(t *testing.T) {
	if !retryable(&googleapi.Error{Code: 503}) || retryable(&googleapi.Error{Code: 404}) || retryable(errors.New("x")) {
		t.Fatal("retryable")
	}
	if !retryable(&googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded"}}}) ||
		retryable(&googleapi.Error{Code: 403, Message: "forbidden"}) {
		t.Fatal("403")
	}
}

func TestRawStores(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []string{"", "drive:folder", "https://drive.google.com/x"} {
		if _, err := MakeRawStore(ctx, bad, nil); err == nil {
			t.Fatal(bad)
		}
	}
	dir := t.TempDir()
	rs, err := MakeRawStore(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := rs.OpenWriter("raw/acme/x")
	w.Write([]byte("abc"))
	w.Close()
	if b, _ := os.ReadFile(filepath.Join(dir, "raw/acme/x")); string(b) != "abc" {
		t.Fatal(string(b))
	}
	g, err := MakeRawStore(ctx, "gs://bucket/pre/", http.DefaultClient)
	if err != nil || g.(GCSRaw).Prefix != "pre" {
		t.Fatal(g, err)
	}
	l := Local{Root: filepath.Join(dir, "out")}
	src := filepath.Join(dir, "src.txt")
	os.WriteFile(src, []byte("x"), 0o644)
	if _, err := l.PutFile(ctx, "a/b.txt", src); err != nil {
		t.Fatal(err)
	}
}
