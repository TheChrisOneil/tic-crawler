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
