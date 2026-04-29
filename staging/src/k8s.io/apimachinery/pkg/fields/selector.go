/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fields

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/selection"
)

// Selector represents a field selector.
type Selector interface {
	// Matches returns true if this selector matches the given set of fields.
	Matches(Fields) bool

	// Empty returns true if this selector does not restrict the selection space.
	Empty() bool

	// RequiresExactMatch allows a caller to introspect whether a given selector
	// requires a single specific field to be set, and if so returns the value it
	// requires.
	RequiresExactMatch(field string) (value string, found bool)

	// RequiresExactMatchOrIn returns the value(s) required for a field.
	// For exact match (=, ==) returns a single-element slice.
	// For in() returns all values in the set.
	// Returns false if the field is not constrained to specific values.
	RequiresExactMatchOrIn(field string) (values []string, found bool)

	// Transform returns a new copy of the selector after TransformFunc has been
	// applied to the entire selector, or an error if fn returns an error.
	// If for a given requirement both field and value are transformed to empty
	// string, the requirement is skipped.
	Transform(fn TransformFunc) (Selector, error)

	// Requirements converts this interface to Requirements to expose
	// more detailed selection information.
	Requirements() Requirements

	// String returns a human readable string that represents this selector.
	String() string

	// Make a deep copy of the selector.
	DeepCopySelector() Selector
}

type nothingSelector struct{}

func (n nothingSelector) Matches(_ Fields) bool      { return false }
func (n nothingSelector) Empty() bool                { return false }
func (n nothingSelector) String() string             { return "" }
func (n nothingSelector) Requirements() Requirements { return nil }
func (n nothingSelector) DeepCopySelector() Selector { return n }
func (n nothingSelector) RequiresExactMatch(field string) (value string, found bool) {
	return "", false
}
func (n nothingSelector) RequiresExactMatchOrIn(field string) (values []string, found bool) {
	return nil, false
}
func (n nothingSelector) Transform(fn TransformFunc) (Selector, error) { return n, nil }

// Nothing returns a selector that matches no fields
func Nothing() Selector {
	return nothingSelector{}
}

// Everything returns a selector that matches all fields.
func Everything() Selector {
	return andTerm{}
}

type hasTerm struct {
	field, value string
}

func (t *hasTerm) Matches(ls Fields) bool {
	return ls.Get(t.field) == t.value
}

func (t *hasTerm) Empty() bool {
	return false
}

func (t *hasTerm) RequiresExactMatch(field string) (value string, found bool) {
	if t.field == field {
		return t.value, true
	}
	return "", false
}

func (t *hasTerm) RequiresExactMatchOrIn(field string) (values []string, found bool) {
	if t.field == field {
		return []string{t.value}, true
	}
	return nil, false
}

func (t *hasTerm) Transform(fn TransformFunc) (Selector, error) {
	field, value, err := fn(t.field, t.value)
	if err != nil {
		return nil, err
	}
	if len(field) == 0 && len(value) == 0 {
		return Everything(), nil
	}
	return &hasTerm{field, value}, nil
}

func (t *hasTerm) Requirements() Requirements {
	return []Requirement{{
		Field:    t.field,
		Operator: selection.Equals,
		Value:    t.value,
	}}
}

func (t *hasTerm) String() string {
	return fmt.Sprintf("%v=%v", t.field, EscapeValue(t.value))
}

func (t *hasTerm) DeepCopySelector() Selector {
	if t == nil {
		return nil
	}
	out := new(hasTerm)
	*out = *t
	return out
}

type notHasTerm struct {
	field, value string
}

func (t *notHasTerm) Matches(ls Fields) bool {
	return ls.Get(t.field) != t.value
}

func (t *notHasTerm) Empty() bool {
	return false
}

func (t *notHasTerm) RequiresExactMatch(field string) (value string, found bool) {
	return "", false
}

func (t *notHasTerm) RequiresExactMatchOrIn(field string) (values []string, found bool) {
	return nil, false
}

func (t *notHasTerm) Transform(fn TransformFunc) (Selector, error) {
	field, value, err := fn(t.field, t.value)
	if err != nil {
		return nil, err
	}
	if len(field) == 0 && len(value) == 0 {
		return Everything(), nil
	}
	return &notHasTerm{field, value}, nil
}

func (t *notHasTerm) Requirements() Requirements {
	return []Requirement{{
		Field:    t.field,
		Operator: selection.NotEquals,
		Value:    t.value,
	}}
}

func (t *notHasTerm) String() string {
	return fmt.Sprintf("%v!=%v", t.field, EscapeValue(t.value))
}

func (t *notHasTerm) DeepCopySelector() Selector {
	if t == nil {
		return nil
	}
	out := new(notHasTerm)
	*out = *t
	return out
}

type andTerm []Selector

func (t andTerm) Matches(ls Fields) bool {
	for _, q := range t {
		if !q.Matches(ls) {
			return false
		}
	}
	return true
}

func (t andTerm) Empty() bool {
	if t == nil {
		return true
	}
	if len([]Selector(t)) == 0 {
		return true
	}
	for i := range t {
		if !t[i].Empty() {
			return false
		}
	}
	return true
}

func (t andTerm) RequiresExactMatch(field string) (string, bool) {
	if t == nil || len([]Selector(t)) == 0 {
		return "", false
	}
	for i := range t {
		if value, found := t[i].RequiresExactMatch(field); found {
			return value, found
		}
	}
	return "", false
}

func (t andTerm) RequiresExactMatchOrIn(field string) ([]string, bool) {
	if t == nil || len([]Selector(t)) == 0 {
		return nil, false
	}
	for i := range t {
		if values, found := t[i].RequiresExactMatchOrIn(field); found {
			return values, found
		}
	}
	return nil, false
}

func (t andTerm) Transform(fn TransformFunc) (Selector, error) {
	next := make([]Selector, 0, len([]Selector(t)))
	for _, s := range []Selector(t) {
		n, err := s.Transform(fn)
		if err != nil {
			return nil, err
		}
		if !n.Empty() {
			next = append(next, n)
		}
	}
	return andTerm(next), nil
}

func (t andTerm) Requirements() Requirements {
	reqs := make([]Requirement, 0, len(t))
	for _, s := range []Selector(t) {
		rs := s.Requirements()
		reqs = append(reqs, rs...)
	}
	return reqs
}

func (t andTerm) String() string {
	var terms []string
	for _, q := range t {
		terms = append(terms, q.String())
	}
	return strings.Join(terms, ",")
}

func (t andTerm) DeepCopySelector() Selector {
	if t == nil {
		return nil
	}
	out := make([]Selector, len(t))
	for i := range t {
		out[i] = t[i].DeepCopySelector()
	}
	return andTerm(out)
}

// inTerm matches a field against a set of values (field in (v1,v2,...)).
type inTerm struct {
	field  string
	values []string
}

func (t *inTerm) Matches(ls Fields) bool {
	v := ls.Get(t.field)
	for _, val := range t.values {
		if v == val {
			return true
		}
	}
	return false
}

func (t *inTerm) Empty() bool { return false }

func (t *inTerm) RequiresExactMatch(field string) (value string, found bool) {
	if t.field == field && len(t.values) == 1 {
		return t.values[0], true
	}
	return "", false
}

func (t *inTerm) RequiresExactMatchOrIn(field string) (values []string, found bool) {
	if t.field == field {
		return t.values, true
	}
	return nil, false
}

func (t *inTerm) Transform(fn TransformFunc) (Selector, error) {
	newValues := make([]string, 0, len(t.values))
	var newField string
	for _, v := range t.values {
		f, nv, err := fn(t.field, v)
		if err != nil {
			return nil, err
		}
		newField = f
		if len(f) > 0 || len(nv) > 0 {
			newValues = append(newValues, nv)
		}
	}
	if len(newValues) == 0 {
		return Everything(), nil
	}
	return &inTerm{field: newField, values: newValues}, nil
}

func (t *inTerm) Requirements() Requirements {
	reqs := make([]Requirement, len(t.values))
	for i, v := range t.values {
		reqs[i] = Requirement{Field: t.field, Operator: selection.In, Value: v}
	}
	return reqs
}

func (t *inTerm) String() string {
	return fmt.Sprintf("%v in (%v)", t.field, strings.Join(t.values, ","))
}

func (t *inTerm) DeepCopySelector() Selector {
	if t == nil {
		return nil
	}
	vals := make([]string, len(t.values))
	copy(vals, t.values)
	return &inTerm{field: t.field, values: vals}
}

// SelectorFromSet returns a Selector which will match exactly the given Set. A
// nil Set is considered equivalent to Everything().
func SelectorFromSet(ls Set) Selector {
	if ls == nil {
		return Everything()
	}
	items := make([]Selector, 0, len(ls))
	for field, value := range ls {
		items = append(items, &hasTerm{field: field, value: value})
	}
	if len(items) == 1 {
		return items[0]
	}
	return andTerm(items)
}

// valueEscaper prefixes \,= characters with a backslash
var valueEscaper = strings.NewReplacer(
	// escape \ characters
	`\`, `\\`,
	// then escape , and = characters to allow unambiguous parsing of the value in a fieldSelector
	`,`, `\,`,
	`=`, `\=`,
)

// EscapeValue escapes an arbitrary literal string for use as a fieldSelector value
func EscapeValue(s string) string {
	return valueEscaper.Replace(s)
}

// InvalidEscapeSequence indicates an error occurred unescaping a field selector
type InvalidEscapeSequence struct {
	sequence string
}

func (i InvalidEscapeSequence) Error() string {
	return fmt.Sprintf("invalid field selector: invalid escape sequence: %s", i.sequence)
}

// UnescapedRune indicates an error occurred unescaping a field selector
type UnescapedRune struct {
	r rune
}

func (i UnescapedRune) Error() string {
	return fmt.Sprintf("invalid field selector: unescaped character in value: %v", i.r)
}

// UnescapeValue unescapes a fieldSelector value and returns the original literal value.
// May return the original string if it contains no escaped or special characters.
func UnescapeValue(s string) (string, error) {
	// if there's no escaping or special characters, just return to avoid allocation
	if !strings.ContainsAny(s, `\,=`) {
		return s, nil
	}

	v := bytes.NewBuffer(make([]byte, 0, len(s)))
	inSlash := false
	for _, c := range s {
		if inSlash {
			switch c {
			case '\\', ',', '=':
				// omit the \ for recognized escape sequences
				v.WriteRune(c)
			default:
				// error on unrecognized escape sequences
				return "", InvalidEscapeSequence{sequence: string([]rune{'\\', c})}
			}
			inSlash = false
			continue
		}

		switch c {
		case '\\':
			inSlash = true
		case ',', '=':
			// unescaped , and = characters are not allowed in field selector values
			return "", UnescapedRune{r: c}
		default:
			v.WriteRune(c)
		}
	}

	// Ending with a single backslash is an invalid sequence
	if inSlash {
		return "", InvalidEscapeSequence{sequence: "\\"}
	}

	return v.String(), nil
}

// ParseSelectorOrDie takes a string representing a selector and returns an
// object suitable for matching, or panic when an error occur.
func ParseSelectorOrDie(s string) Selector {
	selector, err := ParseSelector(s)
	if err != nil {
		panic(err)
	}
	return selector
}

// ParseSelector takes a string representing a selector and returns an
// object suitable for matching, or an error.
func ParseSelector(selector string) (Selector, error) {
	return parseSelector(selector,
		func(lhs, rhs string) (newLhs, newRhs string, err error) {
			return lhs, rhs, nil
		})
}

// ParseAndTransformSelector parses the selector and runs them through the given TransformFunc.
func ParseAndTransformSelector(selector string, fn TransformFunc) (Selector, error) {
	return parseSelector(selector, fn)
}

// TransformFunc transforms selectors.
type TransformFunc func(field, value string) (newField, newValue string, err error)

// splitTerms returns the comma-separated terms contained in the given fieldSelector.
// Backslash-escaped commas are treated as data instead of delimiters, and are included in the returned terms, with the leading backslash preserved.
func splitTerms(fieldSelector string) []string {
	if len(fieldSelector) == 0 {
		return nil
	}

	terms := make([]string, 0, 1)
	startIndex := 0
	inSlash := false
	parenDepth := 0
	for i, c := range fieldSelector {
		switch {
		case inSlash:
			inSlash = false
		case c == '\\':
			inSlash = true
		case c == '(':
			parenDepth++
		case c == ')':
			if parenDepth > 0 {
				parenDepth--
			}
		case c == ',' && parenDepth == 0:
			terms = append(terms, fieldSelector[startIndex:i])
			startIndex = i + 1
		}
	}

	terms = append(terms, fieldSelector[startIndex:])

	return terms
}

const (
	notEqualOperator    = "!="
	doubleEqualOperator = "=="
	equalOperator       = "="
)

// termOperators holds the recognized operators supported in fieldSelectors.
// doubleEqualOperator and equal are equivalent, but doubleEqualOperator is checked first
// to avoid leaving a leading = character on the rhs value.
var termOperators = []string{notEqualOperator, doubleEqualOperator, equalOperator}

// splitTerm returns the lhs, operator, and rhs parsed from the given term, along with an indicator of whether the parse was successful.
// no escaping of special characters is supported in the lhs value, so the first occurrence of a recognized operator is used as the split point.
// the literal rhs is returned, and the caller is responsible for applying any desired unescaping.
func splitTerm(term string) (lhs, op, rhs string, ok bool) {
	for i := range term {
		remaining := term[i:]
		for _, op := range termOperators {
			if strings.HasPrefix(remaining, op) {
				return term[0:i], op, term[i+len(op):], true
			}
		}
	}
	return "", "", "", false
}

func parseSelector(selector string, fn TransformFunc) (Selector, error) {
	parts := splitTerms(selector)
	sort.StringSlice(parts).Sort()
	var items []Selector
	for _, part := range parts {
		if part == "" {
			continue
		}
		// Check for "field in (v1,v2,...)" syntax
		if parsed, ok := parseInTerm(part); ok {
			items = append(items, parsed)
			continue
		}
		lhs, op, rhs, ok := splitTerm(part)
		if !ok {
			return nil, fmt.Errorf("invalid selector: '%s'; can't understand '%s'", selector, part)
		}
		unescapedRHS, err := UnescapeValue(rhs)
		if err != nil {
			return nil, err
		}
		switch op {
		case notEqualOperator:
			items = append(items, &notHasTerm{field: lhs, value: unescapedRHS})
		case doubleEqualOperator:
			items = append(items, &hasTerm{field: lhs, value: unescapedRHS})
		case equalOperator:
			items = append(items, &hasTerm{field: lhs, value: unescapedRHS})
		default:
			return nil, fmt.Errorf("invalid selector: '%s'; can't understand '%s'", selector, part)
		}
	}
	if len(items) == 1 {
		return items[0].Transform(fn)
	}
	return andTerm(items).Transform(fn)
}

// parseInTerm parses "field in (v1,v2,...)" and returns an inTerm.
func parseInTerm(term string) (Selector, bool) {
	idx := strings.Index(term, " in (")
	if idx < 0 {
		return nil, false
	}
	field := strings.TrimSpace(term[:idx])
	rest := term[idx+5:] // skip " in ("
	if !strings.HasSuffix(rest, ")") {
		return nil, false
	}
	rest = rest[:len(rest)-1] // strip trailing )
	values := strings.Split(rest, ",")
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	return &inTerm{field: field, values: values}, true
}

// OneTermEqualSelector returns an object that matches objects where one field/field equals one value.
// Cannot return an error.
func OneTermEqualSelector(k, v string) Selector {
	return &hasTerm{field: k, value: v}
}

// OneTermNotEqualSelector returns an object that matches objects where one field/field does not equal one value.
// Cannot return an error.
func OneTermNotEqualSelector(k, v string) Selector {
	return &notHasTerm{field: k, value: v}
}

// AndSelectors creates a selector that is the logical AND of all the given selectors
func AndSelectors(selectors ...Selector) Selector {
	return andTerm(selectors)
}
