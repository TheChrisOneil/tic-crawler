package medicare

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// xlsxToCSV converts the first worksheet of an .xlsx file to CSV, enough for the flat tables
// CMS publishes (shared strings, inline strings and numbers; no formulas or dates needed).
func xlsxToCSV(data []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	files := map[string]*zip.File{}
	var sheets []string
	for _, f := range zr.File {
		files[f.Name] = f
		if strings.HasPrefix(f.Name, "xl/worksheets/sheet") && strings.HasSuffix(f.Name, ".xml") {
			sheets = append(sheets, f.Name)
		}
	}
	if len(sheets) == 0 {
		return nil, fmt.Errorf("xlsx: no worksheet")
	}
	sort.Slice(sheets, func(i, j int) bool { return sheetNum(sheets[i]) < sheetNum(sheets[j]) })
	read := func(name string) ([]byte, error) {
		f := files[name]
		if f == nil {
			return nil, nil
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, 512<<20))
	}
	var shared []string
	if b, err := read("xl/sharedStrings.xml"); err != nil {
		return nil, err
	} else if b != nil {
		var sst struct {
			SI []struct {
				T string `xml:"t"`
				R []struct {
					T string `xml:"t"`
				} `xml:"r"`
			} `xml:"si"`
		}
		if err := xml.Unmarshal(b, &sst); err != nil {
			return nil, fmt.Errorf("xlsx shared strings: %w", err)
		}
		for _, si := range sst.SI {
			s := si.T
			for _, r := range si.R {
				s += r.T
			}
			shared = append(shared, s)
		}
	}
	b, err := read(sheets[0])
	if err != nil {
		return nil, err
	}
	var ws struct {
		Rows []struct {
			C []struct {
				R  string `xml:"r,attr"`
				T  string `xml:"t,attr"`
				V  string `xml:"v"`
				IS struct {
					T string `xml:"t"`
				} `xml:"is"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal(b, &ws); err != nil {
		return nil, fmt.Errorf("xlsx sheet: %w", err)
	}
	var out bytes.Buffer
	w := csv.NewWriter(&out)
	for _, row := range ws.Rows {
		var rec []string
		for i, c := range row.C {
			col := colIndex(c.R)
			if col < 0 {
				col = i
			}
			for len(rec) < col {
				rec = append(rec, "")
			}
			v := c.V
			switch c.T {
			case "s":
				if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < len(shared) {
					v = shared[n]
				}
			case "inlineStr":
				v = c.IS.T
			}
			if len(rec) == col {
				rec = append(rec, v)
			} else {
				rec[col] = v
			}
		}
		w.Write(rec)
	}
	w.Flush()
	return out.Bytes(), w.Error()
}

func sheetNum(name string) int {
	n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "xl/worksheets/sheet"), ".xml"))
	return n
}

// colIndex turns a cell reference like "AB12" into a zero-based column (27).
func colIndex(ref string) int {
	n := 0
	seen := false
	for _, r := range ref {
		if r >= 'A' && r <= 'Z' {
			n = n*26 + int(r-'A'+1)
			seen = true
		} else {
			break
		}
	}
	if !seen {
		return -1
	}
	return n - 1
}
