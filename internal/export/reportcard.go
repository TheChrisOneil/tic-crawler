package export

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// ReportCards copies each payer's price report card (out/quality/<payer>/report_card.json,
// written by `report-card`) to out/dashboard/report_card/<payer>.json and writes an index
// there. These are new files only; data.json and payers/*.json are untouched. The dashboard's
// report-card page reads them. Returns the number of cards.
func ReportCards(outDir string) (int, error) {
	paths, err := filepath.Glob(filepath.Join(outDir, "quality", "*", "report_card.json"))
	if err != nil {
		return 0, err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return 0, nil
	}
	dest := filepath.Join(outDir, "dashboard", "report_card")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return 0, err
	}
	type entry struct {
		PayerID     string `json:"payer_id"`
		PayerName   string `json:"payer_name"`
		GeneratedAt string `json:"generated_at"`
		Hospitals   int    `json:"hospitals"`
		Codes       int    `json:"codes"`
	}
	index := []entry{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return 0, err
		}
		var c struct {
			PayerID     string            `json:"payer_id"`
			PayerName   string            `json:"payer_name"`
			GeneratedAt string            `json:"generated_at"`
			Hospitals   []json.RawMessage `json:"hospitals"`
			Codes       []json.RawMessage `json:"codes"`
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return 0, err
		}
		if c.PayerID == "" {
			c.PayerID = filepath.Base(filepath.Dir(p))
		}
		if err := os.WriteFile(filepath.Join(dest, c.PayerID+".json"), b, 0o644); err != nil {
			return 0, err
		}
		index = append(index, entry{c.PayerID, c.PayerName, c.GeneratedAt, len(c.Hospitals), len(c.Codes)})
	}
	return len(index), writeJSON(filepath.Join(dest, "index.json"), map[string]any{"cards": index})
}
