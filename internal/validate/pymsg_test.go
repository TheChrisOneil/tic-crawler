package validate

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestPyRepr(t *testing.T) {
	cases := map[string]any{
		"None": nil, "True": true, "'it\\'s'": "x", `"it's"`: "it's", "'a\\nb'": "a\nb",
		"1.5": json.Number("1.50"), "100.0": json.Number("100.0"), "1e+16": json.Number("1e16"), "3": json.Number("3"),
		"[1, 'a', None]": []any{json.Number("1"), "a", nil}, "{'a': 1, 'b': [True]}": map[string]any{"b": []any{true}, "a": json.Number("1")},
	}
	delete(cases, "'it\\'s'")
	for want, v := range cases {
		if got := pyRepr(v); got != want {
			t.Errorf("pyRepr(%#v) = %s, want %s", v, got, want)
		}
	}
	if pyStr("tab\there\x01") != `'tab\there\x01'` {
		t.Error(pyStr("tab\there\x01"))
	}
}

// Messages for keywords the TOC schema does not use, checked against Python jsonschema 4.26.
func TestPyMessagesOtherKeywords(t *testing.T) {
	cases := []struct {
		schema, inst string
		want         string
	}{
		{`{"minItems": 2}`, `[1]`, "[1] is too short"},
		{`{"maxItems": 1}`, `[1, 2]`, "[1, 2] is too long"},
		{`{"maxItems": 0}`, `[1]`, "[1] is expected to be empty"},
		{`{"minLength": 2}`, `"a"`, "'a' is too short"},
		{`{"minLength": 1}`, `""`, "'' should be non-empty"},
		{`{"maxLength": 1}`, `"ab"`, "'ab' is too long"},
		{`{"maxLength": 0}`, `"a"`, "'a' is expected to be empty"},
		{`{"pattern": "^a"}`, `"b"`, "'b' does not match '^a'"},
		{`{"minimum": 1}`, `0`, "0 is less than the minimum of 1"},
		{`{"maximum": 1}`, `2`, "2 is greater than the maximum of 1"},
		{`{"exclusiveMinimum": 0}`, `0`, "0 is less than or equal to the minimum of 0"},
		{`{"exclusiveMaximum": 1}`, `1`, "1 is greater than or equal to the maximum of 1"},
		{`{"const": 1}`, `2`, "1 was expected"},
		{`{"uniqueItems": true}`, `[1, 1]`, "[1, 1] has non-unique elements"},
		{`{"anyOf": [{"type": "string"}, {"type": "number"}]}`, `[]`, "[] is not valid under any of the given schemas"},
		{`{"type": ["string", "null"]}`, `1`, "1 is not of type 'string', 'null'"},
		{`{"additionalProperties": false}`, `{"x": 1, "y": 2}`, "Additional properties are not allowed ('x', 'y' were unexpected)"},
		{`{"additionalProperties": false}`, `{"x": 1}`, "Additional properties are not allowed ('x' was unexpected)"},
		{`{"format": "date"}`, `"2026-13-01"`, "'2026-13-01' is not a 'date'"},
	}
	for _, c := range cases {
		doc, _ := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(c.schema)))
		s, err := compile("t.json", doc)
		if err != nil {
			t.Fatal(err)
		}
		inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(c.inst)))
		v := &Validator{order: schemaKeyOrder([]byte(c.schema)), schema: doc}
		err = s.Validate(inst)
		if err == nil {
			t.Errorf("%s: no error", c.schema)
			continue
		}
		got := v.leaves(err, inst)
		if len(got) != 1 || got[0].msg != c.want {
			t.Errorf("%s on %s: got %+v, want %q", c.schema, c.inst, got, c.want)
		}
	}
}
