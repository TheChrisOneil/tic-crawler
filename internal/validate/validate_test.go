package validate

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/TheChrisOneil/tic-crawler/internal/toc"
)

const plan = `{"plan_name": "A", "issuer_name": "Acme", "plan_id_type": "hios", "plan_id": "1234567890", "plan_market_type": "group"}`

func item(t *testing.T, mutate func(m map[string]any)) any {
	var m map[string]any
	json.Unmarshal([]byte(`{"reporting_plans": [`+plan+`], "in_network_files": [{"description": "in", "location": "https://x/a.json"}]}`), &m)
	if mutate != nil {
		mutate(m)
	}
	return m
}

func header(mutate func(map[string]any)) toc.GenericHeader {
	f := map[string]any{"reporting_entity_name": "Acme", "reporting_entity_type": "health insurance issuer",
		"last_updated_on": "2026-10-01", "version": "2.0.0"}
	if mutate != nil {
		mutate(f)
	}
	return toc.GenericHeader{Fields: f, HasReportingArr: true, TopLevelIsObj: true}
}

func run(t *testing.T, items []any, h toc.GenericHeader) Report {
	v, err := New("2.2.1", 50)
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range items {
		v.ValidateItem(i, it)
	}
	v.ValidateHeader(h)
	return v.Finish()
}

func TestCMSSampleIsValid(t *testing.T) {
	b, _ := os.ReadFile("../../tests/fixtures/toc/cms-sample.json")
	v, _ := New("2.2.1", 50)
	h, err := toc.ReadGeneric(bytes.NewReader(b), func(i int, it any) error { v.ValidateItem(i, it); return nil })
	if err != nil {
		t.Fatal(err)
	}
	v.ValidateHeader(h)
	r := v.Finish()
	if !r.Valid || r.FileVersion != "2.0.0" || r.PinnedVersion != "2.2.1" || r.ItemCount != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestValidMinimal(t *testing.T) {
	if r := run(t, []any{item(t, nil)}, header(nil)); !r.Valid {
		t.Fatal(r.Errors)
	}
}

func TestHeaderErrors(t *testing.T) {
	cases := []struct {
		mutate func(map[string]any)
		want   string
	}{
		{func(h map[string]any) { delete(h, "version") }, "version"},
		{func(h map[string]any) { h["last_updated_on"] = "10/01/2026" }, "last_updated_on"},
		{func(h map[string]any) { h["reporting_entity_name"] = "" }, "reporting_entity_name"},
	}
	for _, c := range cases {
		r := run(t, []any{item(t, nil)}, header(c.mutate))
		if r.Valid || !strings.Contains(strings.Join(r.Errors, "\n"), c.want) {
			t.Errorf("%s: %v", c.want, r.Errors)
		}
	}
}

func plan0(m map[string]any) map[string]any { return m["reporting_plans"].([]any)[0].(map[string]any) }

func TestItemErrors(t *testing.T) {
	cases := []struct {
		mutate func(map[string]any)
		want   string
	}{
		{func(m map[string]any) { plan0(m)["plan_id_type"] = "npi" }, "plan_id_type"},
		{func(m map[string]any) { plan0(m)["plan_market_type"] = "medicare" }, "plan_market_type"},
		{func(m map[string]any) { plan0(m)["plan_id_type"] = "ein" }, "plan_sponsor_name"},
		{func(m map[string]any) {
			m["in_network_files"].([]any)[0].(map[string]any)["location"] = "http://x/a.json"
		}, "location"},
		{func(m map[string]any) { delete(m, "in_network_files") }, "reporting_structure[0]"},
		{func(m map[string]any) { m["reporting_plans"] = []any{} }, "reporting_plans"},
		{func(m map[string]any) { delete(plan0(m), "issuer_name") }, "issuer_name"},
	}
	for _, c := range cases {
		r := run(t, []any{item(t, c.mutate)}, header(nil))
		all := strings.Join(r.Errors, "\n")
		if r.Valid || !strings.Contains(all, c.want) {
			t.Errorf("%s: %v", c.want, r.Errors)
		}
		for _, e := range r.Errors {
			if !strings.HasPrefix(e, "reporting_structure[0]") {
				t.Errorf("bad location %q", e)
			}
		}
	}
}

func TestDuplicateKindsAndStructure(t *testing.T) {
	bad := item(t, func(m map[string]any) { plan0(m)["plan_market_type"] = "x" })
	r := run(t, []any{bad, bad, item(t, nil)}, header(nil))
	if r.InvalidItems != 2 || r.ItemCount != 3 {
		t.Fatalf("%+v", r)
	}
	found := false
	for _, k := range r.ErrorKinds {
		if strings.HasPrefix(k.Kind, "reporting_structure[*]") && k.Count >= 2 {
			found = true
		}
	}
	if !found || !strings.Contains(strings.Join(r.Errors, "\n"), "uniqueItems") {
		t.Fatalf("%+v", r.ErrorKinds)
	}
	r = run(t, nil, toc.GenericHeader{Fields: header(nil).Fields, HasReportingArr: true, TopLevelIsObj: true})
	if r.Valid || !strings.Contains(strings.Join(r.Errors, "\n"), "non-empty") {
		t.Fatal(r.Errors)
	}
	r = run(t, nil, toc.GenericHeader{Fields: header(nil).Fields, TopLevelIsObj: true})
	if !strings.Contains(strings.Join(r.Errors, "\n"), "'reporting_structure' is a required property") {
		t.Fatal(r.Errors)
	}
	r = run(t, nil, toc.GenericHeader{})
	if r.Errors[0] != "$: top level is not a JSON object" {
		t.Fatal(r.Errors)
	}
}

func TestMissingSchemaVersion(t *testing.T) {
	if _, err := New("9.9.9", 50); err == nil {
		t.Fatal("want error")
	}
}
