package medicare

import (
	"archive/zip"
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

const addB = `"Addendum B.-Final OPPS Payment by HCPCS Code for CY 2026"
"CPT codes and descriptions only are copyright American Medical Association."
,,,,,,
HCPCS Code,Short Descriptor,SI,APC,Relative Weight,Payment Rate,National Unadjusted Copayment
73721,Mri jnt of lwr extre w/o dye,Q3,5523,2.6,$245.10,,
45378,Diagnostic colonoscopy,T,5312,10.5,"$1,150.00",
G0463,Hospital outpt clinic visit,J2,5012,1.5,$135.00,
80053,Comprehen metabolic panel,Q4,,,,
99213,Office o/p est low 20 min,B,,,,
`

const clfs = `"CY 2026 Clinical Laboratory Fee Schedule"
YEAR,HCPCS,MOD,EFF_DATE,INDICATOR,RATE,SHORTDESC
2026,80053,,20260101,N,10.56,Comprehen metabolic panel
2026,85025,QW,20260101,N,7.77,Cbc
`

func TestParseAndFacility(t *testing.T) {
	o, err := ParseOPPS(strings.NewReader(addB), "Oct 2026")
	if err != nil {
		t.Fatal(err)
	}
	if len(o) != 3 || o[1].Amount != 1150 || o[0].StatusIndicator != "Q3" || o[0].APC != "5523" {
		t.Fatalf("opps = %+v", o)
	}
	c, err := ParseCLFS(strings.NewReader(clfs), "26CLABQ4")
	if err != nil {
		t.Fatal(err)
	}
	if len(c) != 1 || c[0].Code != "80053" || c[0].Amount != 10.56 {
		t.Fatalf("clfs = %+v", c)
	}
	path := filepath.Join(t.TempDir(), "m.csv.gz")
	if err := Write(path, append(o, c...)); err != nil {
		t.Fatal(err)
	}
	bm, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ code, src, mc string }{
		{"73721", "opps", "73721"}, {"80053", "clfs", "80053"}, {"99213", "opps", "G0463"}, {"99385", "", ""}, {"27447", "", ""},
	} {
		b, mc, ok := Facility(bm, tc.code)
		if ok != (tc.src != "") || b.Source != tc.src || mc != tc.mc {
			t.Errorf("Facility(%s) = %+v %q %v", tc.code, b, mc, ok)
		}
	}
}

func TestParseFileFromNestedZip(t *testing.T) {
	var inner bytes.Buffer
	zw := zip.NewWriter(&inner)
	w, _ := zw.Create("readme.csv")
	w.Write([]byte("nothing,here\n1,2\n"))
	w, _ = zw.Create("October 2026 Addendum B.csv")
	w.Write([]byte(addB))
	zw.Close()
	var outer bytes.Buffer
	zw = zip.NewWriter(&outer)
	w, _ = zw.Create("addb.xlsx")
	w.Write([]byte("not a csv"))
	w, _ = zw.Create("addb.zip")
	w.Write(inner.Bytes())
	zw.Close()
	bs, err := ParseFile(outer.Bytes(), "r", ParseOPPS)
	if err != nil || len(bs) != 3 {
		t.Fatalf("%v %v", bs, err)
	}
	if _, err := ParseFile([]byte("a,b\n1,2\n"), "r", ParseCLFS); err == nil {
		t.Fatal("want error for a CSV without the expected header")
	}
}

func TestParseFileFromXLSX(t *testing.T) {
	var x bytes.Buffer
	zw := zip.NewWriter(&x)
	w, _ := zw.Create("xl/sharedStrings.xml")
	w.Write([]byte(`<sst><si><t>HCPCS Code</t></si><si><t>Short Descriptor</t></si><si><t>SI</t></si><si><t>APC</t></si>` +
		`<si><t>Payment Rate</t></si><si><r><t>Mri jnt </t></r><r><t>lwr</t></r></si><si><t>Q3</t></si></sst>`))
	w, _ = zw.Create("xl/worksheets/sheet1.xml")
	w.Write([]byte(`<worksheet><sheetData>` +
		`<row r="1"><c r="A1" t="inlineStr"><is><t>Addendum B notice</t></is></c></row>` +
		`<row r="3"><c r="A3" t="s"><v>0</v></c><c r="B3" t="s"><v>1</v></c><c r="C3" t="s"><v>2</v></c><c r="D3" t="s"><v>3</v></c><c r="F3" t="s"><v>4</v></c></row>` +
		`<row r="4"><c r="A4" t="inlineStr"><is><t>73721</t></is></c><c r="B4" t="s"><v>5</v></c><c r="C4" t="s"><v>6</v></c><c r="D4"><v>5523</v></c><c r="F4"><v>245.1</v></c></row>` +
		`</sheetData></worksheet>`))
	zw.Close()
	var outer bytes.Buffer
	zw = zip.NewWriter(&outer)
	w, _ = zw.Create("October 2026 Addendum B.xlsx")
	w.Write(x.Bytes())
	w, _ = zw.Create("notes.pdf")
	w.Write([]byte("%PDF"))
	zw.Close()
	bs, err := ParseFile(outer.Bytes(), "r", ParseOPPS)
	if err != nil || len(bs) != 1 || bs[0].Amount != 245.1 || bs[0].Description != "Mri jnt lwr" || bs[0].APC != "5523" {
		t.Fatalf("%+v %v", bs, err)
	}
	var empty bytes.Buffer
	zw = zip.NewWriter(&empty)
	w, _ = zw.Create("notes.pdf")
	zw.Close()
	if _, err := ParseFile(empty.Bytes(), "r", ParseOPPS); err == nil || !strings.Contains(err.Error(), "notes.pdf") {
		t.Fatalf("want error listing contents, got %v", err)
	}
}
