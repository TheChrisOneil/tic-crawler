package validate

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/TheChrisOneil/tic-crawler/internal/toc"
)

// The expected report was produced by the Python TocValidator from the same file.
func TestMatchesPythonReport(t *testing.T) {
	raw, err := os.ReadFile("testdata/bad-toc.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	v, err := New("2.2.1", 50)
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range doc["reporting_structure"].([]any) {
		v.ValidateItem(i, it)
	}
	delete(doc, "reporting_structure")
	v.ValidateHeader(toc.GenericHeader{Fields: doc, HasReportingArr: true, TopLevelIsObj: true})
	got := v.Finish()
	var want Report
	b, _ := os.ReadFile("testdata/bad-toc.python.json")
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Errors, want.Errors) {
		for i := range max(len(got.Errors), len(want.Errors)) {
			var g, w string
			if i < len(got.Errors) {
				g = got.Errors[i]
			}
			if i < len(want.Errors) {
				w = want.Errors[i]
			}
			if g != w {
				t.Errorf("error %d:\n go: %s\n py: %s", i, g, w)
			}
		}
	}
	got.Errors, want.Errors = nil, nil
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", " ")
		t.Errorf("report differs:\n%s", gb)
	}
}

// A long message is cut for the error list, but its kind comes from the whole message, so a
// URL past the cut is still replaced with '<url>'.
func TestLongMessageKind(t *testing.T) {
	v, _ := New("2.2.1", 50)
	msg := "reporting_structure[3].x: " + strings.Repeat("é", 290) + " 'https://a/b' is bad"
	v.AddError(msg)
	r := v.Finish()
	if n := len([]rune(r.Errors[0])); n != 300 {
		t.Fatalf("error cut to %d characters", n)
	}
	want := "reporting_structure[*].x: " + strings.Repeat("é", 214)
	if r.ErrorKinds[0].Kind != want {
		t.Fatalf("kind %q", r.ErrorKinds[0].Kind)
	}
}
