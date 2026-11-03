// Package state keeps resumable crawl state in SQLite. The schema and JSON columns are the
// same as the Python crawler's, so either can read the state the other wrote.
package state

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/TheChrisOneil/tic-crawler/internal/model"
)

const schema = `
CREATE TABLE IF NOT EXISTS payers (payer_id TEXT PRIMARY KEY, data TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS runs (run_id TEXT PRIMARY KEY, started_at TEXT NOT NULL, finished_at TEXT,
    status TEXT NOT NULL, args TEXT NOT NULL DEFAULT '{}');
CREATE TABLE IF NOT EXISTS run_payers (run_id TEXT NOT NULL, payer_id TEXT NOT NULL, status TEXT NOT NULL,
    result TEXT NOT NULL, PRIMARY KEY (run_id, payer_id));
CREATE TABLE IF NOT EXISTS payer_latest (payer_id TEXT PRIMARY KEY, toc_sha256 TEXT, toc_url TEXT,
    crawled_at TEXT, result TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS uploads (path TEXT PRIMARY KEY, sha256 TEXT NOT NULL, remote_id TEXT,
    uploaded_at TEXT NOT NULL);
`

// Now is overridable in tests.
var Now = func() time.Time { return time.Now().UTC() }

func utcnow() string { return Now().Format("2006-01-02T15:04:05Z") }

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// sortedJSON marshals v with sorted keys (as Python's json.dumps(sort_keys=True)).
func sortedJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return "", err
	}
	b, err = json.Marshal(generic) // maps marshal with sorted keys
	return string(b), err
}

func (s *Store) SavePayers(payers []model.Payer) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	now := utcnow()
	for _, p := range payers {
		data, err := sortedJSON(p)
		if err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO payers(payer_id, data, updated_at) VALUES (?,?,?)
			ON CONFLICT(payer_id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`, p.PayerID, data, now); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LoadPayers() ([]model.Payer, error) {
	rows, err := s.db.Query(`SELECT data FROM payers ORDER BY payer_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Payer
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var p model.Payer
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// OpenRun returns (runID, resumed). An unfinished run with the same args is resumed.
func (s *Store) OpenRun(args map[string]any) (string, bool, error) {
	key, err := sortedJSON(args)
	if err != nil {
		return "", false, err
	}
	var id string
	err = s.db.QueryRow(`SELECT run_id FROM runs WHERE status='running' AND args=? ORDER BY started_at DESC`, key).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}
	base := Now().Format("20060102T150405Z")
	id = base
	for n := 2; ; n++ {
		var one int
		if err := s.db.QueryRow(`SELECT 1 FROM runs WHERE run_id=?`, id).Scan(&one); err == sql.ErrNoRows {
			break
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}
	_, err = s.db.Exec(`INSERT INTO runs(run_id, started_at, status, args) VALUES (?,?,?,?)`, id, utcnow(), "running", key)
	return id, false, err
}

func (s *Store) FinishRun(id, status string) error {
	_, err := s.db.Exec(`UPDATE runs SET status=?, finished_at=? WHERE run_id=?`, status, utcnow(), id)
	return err
}

func (s *Store) DonePayers(runID string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT payer_id FROM run_payers WHERE run_id=?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Store) RunResults(runID string) ([]map[string]any, error) {
	return s.jsonRows(`SELECT result FROM run_payers WHERE run_id=? ORDER BY payer_id`, runID)
}

func (s *Store) jsonRows(q string, args ...any) ([]map[string]any, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var data string
		rows.Scan(&data)
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

var keepOnFailure = []string{"mrf_count", "total_bytes", "schema_valid", "validation", "toc_bytes", "toc_last_updated_on"}

func (s *Store) RecordResult(runID string, r model.PayerResult) error {
	data, err := sortedJSON(r)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO run_payers(run_id, payer_id, status, result) VALUES (?,?,?,?)`,
		runID, r.PayerID, r.Status, data); err != nil {
		return err
	}
	switch r.Status {
	case "ok", "unchanged", "failed", "skipped", "deferred":
	default:
		return tx.Commit()
	}
	prev, err := latestTx(tx, r.PayerID)
	if err != nil {
		return err
	}
	var merged map[string]any
	json.Unmarshal([]byte(data), &merged)
	sha := r.TocSHA256
	if sha == "" && prev != nil {
		sha, _ = prev["toc_sha256"].(string)
	}
	if r.Status != "ok" && prev != nil {
		// Keep the counts from the run that produced the current TOC.
		for _, k := range keepOnFailure {
			merged[k] = prev[k]
		}
		if r.TocURL == "" {
			merged["toc_url"] = prev["toc_url"]
		}
		merged["toc_sha256"] = sha
	}
	mj, _ := json.Marshal(merged)
	var shaArg any = sha
	if sha == "" {
		shaArg = nil
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO payer_latest(payer_id, toc_sha256, toc_url, crawled_at, result) VALUES (?,?,?,?,?)`,
		r.PayerID, shaArg, merged["toc_url"], r.CrawledAt, string(mj)); err != nil {
		return err
	}
	return tx.Commit()
}

func latestTx(tx *sql.Tx, id string) (map[string]any, error) {
	var data string
	err := tx.QueryRow(`SELECT result FROM payer_latest WHERE payer_id=?`, id).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal([]byte(data), &m)
}

func (s *Store) Latest(id string) (map[string]any, error) {
	rows, err := s.jsonRows(`SELECT result FROM payer_latest WHERE payer_id=?`, id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func (s *Store) LatestSHA(id string) (string, error) {
	var sha sql.NullString
	err := s.db.QueryRow(`SELECT toc_sha256 FROM payer_latest WHERE payer_id=?`, id).Scan(&sha)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sha.String, err
}

func (s *Store) AllLatest() (map[string]map[string]any, error) {
	rows, err := s.jsonRows(`SELECT result FROM payer_latest`)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, r := range rows {
		id, _ := r["payer_id"].(string)
		out[id] = r
	}
	return out, nil
}

func (s *Store) UploadedSHA(path string) (string, error) {
	var sha string
	err := s.db.QueryRow(`SELECT sha256 FROM uploads WHERE path=?`, path).Scan(&sha)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sha, err
}

func (s *Store) MarkUploaded(path, sha, remoteID string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO uploads(path, sha256, remote_id, uploaded_at) VALUES (?,?,?,?)`, path, sha, remoteID, utcnow())
	return err
}
