// Package validate checks TOCs against the pinned CMS JSON Schema one reporting_structure
// item at a time. The schema is split into an item schema and a header schema so a multi-GB
// TOC never loads at once; uniqueItems on reporting_structure is checked with item hashes.
package validate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/TheChrisOneil/tic-crawler/internal/schemas"
	"github.com/TheChrisOneil/tic-crawler/internal/toc"
)

const (
	TOCSchema = "table-of-contents.json"
	MaxKinds  = 200
)

var (
	indexRx     = regexp.MustCompile(`\[\d+\]`)
	quotedURLRx = regexp.MustCompile(`'https?://[^']*'`)
	printer     = message.NewPrinter(language.English)
)

type ErrorKind struct {
	Kind     string   `json:"kind"`
	Count    int      `json:"count"`
	Examples []string `json:"examples"`
}

type Report struct {
	PinnedVersion string      `json:"pinned_version"`
	FileVersion   string      `json:"file_version"`
	Valid         bool        `json:"valid"`
	ErrorCount    int         `json:"error_count"`
	Errors        []string    `json:"errors"`
	ItemCount     int         `json:"item_count"`
	InvalidItems  int         `json:"invalid_items"`
	ErrorKinds    []ErrorKind `json:"error_kinds"`
}

type Validator struct {
	item, header *jsonschema.Schema
	maxErrors    int
	report       Report
	seen         map[[32]byte]bool
	kinds        map[string]*ErrorKind
	order        keyOrder
}

func compile(name string, doc any) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft7)
	c.AssertFormat()
	// Python jsonschema only checks "uri" when rfc3987 is installed, which the crawler never
	// had; skip it so both tools flag the same files.
	c.RegisterFormat(&jsonschema.Format{Name: "uri", Validate: func(any) error { return nil }})
	if err := c.AddResource(name, doc); err != nil {
		return nil, err
	}
	return c.Compile(name)
}

func New(version string, maxErrors int) (*Validator, error) {
	raw, err := schemas.Load(version, TOCSchema)
	if err != nil {
		return nil, err
	}
	full, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	m := full.(map[string]any)
	item, err := compile("item.json", map[string]any{"definitions": m["definitions"], "$ref": "#/definitions/reporting_structure"})
	if err != nil {
		return nil, err
	}
	hdr := map[string]any{}
	for k, v := range m {
		hdr[k] = v
	}
	props := map[string]any{}
	for k, v := range m["properties"].(map[string]any) {
		if k != "reporting_structure" {
			props[k] = v
		}
	}
	hdr["properties"] = props
	var req []any
	for _, r := range m["required"].([]any) {
		if r != "reporting_structure" {
			req = append(req, r)
		}
	}
	hdr["required"] = req
	header, err := compile("header.json", hdr)
	if err != nil {
		return nil, err
	}
	return &Validator{item: item, header: header, maxErrors: maxErrors,
		report: Report{PinnedVersion: version, Valid: true, Errors: []string{}},
		seen:   map[[32]byte]bool{}, kinds: map[string]*ErrorKind{}, order: schemaKeyOrder(raw)}, nil
}

// AddError records an error found outside the schema, such as a JSON parse error.
func (v *Validator) AddError(msg string) { v.add(msg) }

func (v *Validator) add(msg string) {
	v.report.ErrorCount++
	v.report.Valid = false
	if len(v.report.Errors) < v.maxErrors {
		if len(msg) > 300 {
			msg = msg[:300]
		}
		v.report.Errors = append(v.report.Errors, msg)
	}
	location, detail, _ := strings.Cut(msg, ": ")
	kind := indexRx.ReplaceAllString(location, "[*]") + ": " + quotedURLRx.ReplaceAllString(detail, "'<url>'")
	if len(kind) > 240 {
		kind = kind[:240]
	}
	e := v.kinds[kind]
	if e == nil {
		if len(v.kinds) >= MaxKinds {
			kind = "(other errors)"
			if e = v.kinds[kind]; e == nil {
				e = &ErrorKind{Kind: kind, Examples: []string{}}
				v.kinds[kind] = e
			}
		} else {
			e = &ErrorKind{Kind: kind, Examples: []string{}}
			v.kinds[kind] = e
		}
	}
	e.Count++
	if len(e.Examples) < 5 {
		e.Examples = append(e.Examples, location)
	}
}

func path(parts []string) string {
	var b strings.Builder
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err == nil {
			b.WriteString("[" + p + "]")
		} else {
			b.WriteString("." + p)
		}
	}
	return b.String()
}

type leaf struct {
	loc []string
	kw  []string
	msg string
}

// leaves flattens a validation error into Python-style messages, one per failing keyword.
// anyOf/oneOf report one error at their own location, as Python's iter_errors does.
func (v *Validator) leaves(err error, doc any) []leaf {
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []leaf{{nil, nil, err.Error()}}
	}
	var out []leaf
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		_, anyOf := e.ErrorKind.(*kind.AnyOf)
		_, oneOf := e.ErrorKind.(*kind.OneOf)
		if len(e.Causes) == 0 || anyOf || oneOf {
			msgs := pyMessages(e, at(doc, e.InstanceLocation))
			if msgs == nil {
				msgs = []string{e.ErrorKind.LocalizedString(printer)}
			}
			for _, m := range msgs {
				out = append(out, leaf{e.InstanceLocation, keywordPointer(e), m})
			}
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if lessPath(a.loc, b.loc) || lessPath(b.loc, a.loc) {
			return lessPath(a.loc, b.loc)
		}
		return v.order.less(a.kw, b.kw)
	})
	return out
}

// lessPath orders instance paths like Python sorts absolute_path lists: indexes numerically.
func lessPath(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] == b[i] {
			continue
		}
		x, ex := strconv.Atoi(a[i])
		y, ey := strconv.Atoi(b[i])
		if ex == nil && ey == nil {
			return x < y
		}
		return a[i] < b[i]
	}
	return len(a) < len(b)
}

// toSchemaValue converts decoded JSON (json.Number numbers) into what the validator expects.
func toSchemaValue(v any) any {
	b, _ := json.Marshal(v)
	out, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return v
	}
	return out
}

func (v *Validator) ValidateItem(index int, item any) {
	v.report.ItemCount++
	before := v.report.ErrorCount
	d := toc.ItemDigest(item)
	if v.seen[d] {
		v.add(fmt.Sprintf("reporting_structure[%d]: duplicate item (uniqueItems)", index))
	}
	v.seen[d] = true
	doc := toSchemaValue(item)
	if err := v.item.Validate(doc); err != nil {
		for _, l := range v.leaves(err, doc) {
			v.add(fmt.Sprintf("reporting_structure[%d]%s: %s", index, path(l.loc), l.msg))
		}
	}
	if v.report.ErrorCount > before {
		v.report.InvalidItems++
	}
}

func (v *Validator) ValidateHeader(h toc.GenericHeader) {
	if !h.TopLevelIsObj {
		v.add("$: top level is not a JSON object")
		return
	}
	if h.ReportingOther != nil {
		v.add("reporting_structure: is not of type 'array'")
	}
	if ver, ok := h.Fields["version"]; ok && ver != nil {
		v.report.FileVersion = fmt.Sprint(ver)
	}
	doc := toSchemaValue(h.Fields)
	if err := v.header.Validate(doc); err != nil {
		for _, l := range v.leaves(err, doc) {
			v.add("$" + path(l.loc) + ": " + l.msg)
		}
	}
	if !h.HasReportingArr {
		if h.ReportingOther == nil {
			v.add("$: 'reporting_structure' is a required property")
		}
	} else if v.report.ItemCount == 0 {
		v.add("reporting_structure: [] should be non-empty")
	}
}

func (v *Validator) Finish() Report {
	v.report.ErrorKinds = []ErrorKind{}
	for _, k := range v.kinds {
		v.report.ErrorKinds = append(v.report.ErrorKinds, *k)
	}
	sort.Slice(v.report.ErrorKinds, func(i, j int) bool {
		a, b := v.report.ErrorKinds[i], v.report.ErrorKinds[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Kind < b.Kind
	})
	return v.report
}
