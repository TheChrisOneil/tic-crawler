package validate

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// Error messages follow Python jsonschema 4.x wording so reports, dashboard error kinds and
// manifests read the same as the Python crawler's.

// pyRepr renders a decoded JSON value the way Python's repr() shows it.
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return pyStr(x)
	case json.Number:
		return pyNum(string(x))
	case float64:
		return pyNum(strconv.FormatFloat(x, 'g', -1, 64))
	case int:
		return strconv.Itoa(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		// Python keeps document order; Go maps do not, so keys are sorted.
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyStr(k) + ": " + pyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

func pyNum(s string) string {
	if !strings.ContainsAny(s, ".eE") {
		return s
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	out := strconv.FormatFloat(f, 'g', -1, 64)
	if e := strings.IndexByte(out, 'e'); e >= 0 {
		// Python writes 1e+16 and 1e-05; Go writes 1e+16 and 1e-05 too, but 'g' may pick a
		// mantissa form Python does not for exponents below 16, so use Python's threshold.
		if exp, _ := strconv.Atoi(out[e+1:]); exp >= -4 && exp < 16 {
			out = strconv.FormatFloat(f, 'f', -1, 64)
		}
	}
	if !strings.ContainsAny(out, ".eIN") {
		out += ".0"
	}
	return out
}

func pyStr(s string) string {
	q := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(q):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case !unicode.IsPrint(r) && r != ' ':
			switch {
			case r < 0x100:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r < 0x10000:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}

func ratRepr(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	f, _ := r.Float64()
	return pyNum(strconv.FormatFloat(f, 'g', -1, 64))
}

func quoteAll(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = pyStr(x)
	}
	return strings.Join(q, ", ")
}

// at returns the value at an instance location.
func at(doc any, loc []string) any {
	for _, p := range loc {
		switch x := doc.(type) {
		case map[string]any:
			doc = x[p]
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(x) {
				return nil
			}
			doc = x[i]
		default:
			return nil
		}
	}
	return doc
}

// pyMessages returns the Python jsonschema message(s) for one error, or nil to fall back to
// the library's own wording.
func pyMessages(e *jsonschema.ValidationError, inst, schemaDoc any) []string {
	r := pyRepr(inst)
	switch k := e.ErrorKind.(type) {
	case *kind.Enum:
		want := make([]any, len(k.Want))
		copy(want, k.Want)
		return []string{fmt.Sprintf("%s is not one of %s", r, pyRepr(want))}
	case *kind.Required:
		out := make([]string, len(k.Missing))
		for i, m := range k.Missing {
			out[i] = pyStr(m) + " is a required property"
		}
		return out
	case *kind.Type:
		want := k.Want // sorted by the library; Python lists the types in schema order
		if list, ok := at(schemaDoc, keywordPointer(e)).([]any); ok && len(list) == len(want) {
			want = make([]string, len(list))
			for i, x := range list {
				want[i] = fmt.Sprint(x)
			}
		}
		return []string{fmt.Sprintf("%s is not of type %s", r, quoteAll(want))}
	case *kind.Format:
		return []string{fmt.Sprintf("%s is not a %s", r, pyStr(k.Want))}
	case *kind.MinItems:
		if k.Want == 1 {
			return []string{r + " should be non-empty"}
		}
		return []string{r + " is too short"}
	case *kind.MaxItems:
		if k.Want == 0 {
			return []string{r + " is expected to be empty"}
		}
		return []string{r + " is too long"}
	case *kind.MinLength:
		if k.Want == 1 {
			return []string{r + " should be non-empty"}
		}
		return []string{r + " is too short"}
	case *kind.MaxLength:
		if k.Want == 0 {
			return []string{r + " is expected to be empty"}
		}
		return []string{r + " is too long"}
	case *kind.Pattern:
		return []string{fmt.Sprintf("%s does not match %s", r, pyStr(k.Want))}
	case *kind.AdditionalProperties:
		verb := "were"
		if len(k.Properties) == 1 {
			verb = "was"
		}
		return []string{fmt.Sprintf("Additional properties are not allowed (%s %s unexpected)", quoteAll(k.Properties), verb)}
	case *kind.Const:
		return []string{pyRepr(k.Want) + " was expected"}
	case *kind.UniqueItems:
		return []string{r + " has non-unique elements"}
	case *kind.Minimum:
		return []string{fmt.Sprintf("%s is less than the minimum of %s", r, ratRepr(k.Want))}
	case *kind.Maximum:
		return []string{fmt.Sprintf("%s is greater than the maximum of %s", r, ratRepr(k.Want))}
	case *kind.ExclusiveMinimum:
		return []string{fmt.Sprintf("%s is less than or equal to the minimum of %s", r, ratRepr(k.Want))}
	case *kind.ExclusiveMaximum:
		return []string{fmt.Sprintf("%s is greater than or equal to the maximum of %s", r, ratRepr(k.Want))}
	case *kind.AnyOf:
		return []string{r + " is not valid under any of the given schemas"}
	case *kind.OneOf:
		if len(k.Subschemas) == 0 {
			return []string{r + " is not valid under any of the given schemas"}
		}
		return []string{r + " is valid under more than one of the given schemas"}
	}
	return nil
}

// keyOrder maps each object's JSON pointer in the schema to the position of its keys, so
// errors at the same instance location can be listed in schema order, as Python does.
type keyOrder map[string]map[string]int

func schemaKeyOrder(raw []byte) keyOrder {
	out := keyOrder{}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	var walk func(ptr string) error
	walk = func(ptr string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			m := map[string]int{}
			out[ptr] = m
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k := kt.(string)
				m[k] = len(m)
				if err := walk(ptr + "/" + k); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case json.Delim('['):
			for i := 0; dec.More(); i++ {
				if err := walk(ptr + "/" + strconv.Itoa(i)); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	walk("")
	return out
}

// keywordPointer is the schema pointer of the keyword that failed.
func keywordPointer(e *jsonschema.ValidationError) []string {
	_, frag, _ := strings.Cut(e.SchemaURL, "#")
	var parts []string
	for _, p := range strings.Split(frag, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return append(parts, e.ErrorKind.KeywordPath()...)
}

// less orders two keyword pointers by where they first part ways in the schema.
func (o keyOrder) less(a, b []string) bool {
	ptr := ""
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			m := o[ptr]
			x, okx := m[a[i]]
			y, oky := m[b[i]]
			if okx && oky {
				return x < y
			}
			return lessPath(a[i:i+1], b[i:i+1])
		}
		ptr += "/" + a[i]
	}
	return len(a) < len(b)
}
