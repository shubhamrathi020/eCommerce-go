package httpx

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/shubhamrathi020/eCommerce-go/internal/apperr"
)

const maxBody = 100 * 1024

type rawKey int

const rawBodyKey rawKey = 0

// invalidBody is the one error every malformed request gets, like the Nest ValidationPipe: it never echoes raw input.
func invalidBody() error {
	return &apperr.Error{Code: apperr.Validation, Message: "The request was not valid."}
}

// ReadBody reads up to 100 KB and returns the exact bytes (the Razorpay webhook verifies its signature over them).
func ReadBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(b) > maxBody {
		return nil, &HTTPError{Status: 413, Message: "request entity too large"}
	}
	return b, nil
}

// Obj validates a JSON object the way class-validator's whitelist + forbidNonWhitelisted pipe does: only the
// declared properties are allowed, each must have the declared type and size, and any failure becomes one
// generic 400. Call the typed getters, then Err().
type Obj struct {
	m   map[string]json.RawMessage
	bad bool
}

// ReadObject parses the request body as a JSON object whose keys must all be in allowed.
func ReadObject(r *http.Request, allowed ...string) *Obj {
	o := &Obj{m: map[string]json.RawMessage{}}
	b, err := ReadBody(r)
	if err != nil {
		o.bad = true
		return o
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return o
	}
	if err := json.Unmarshal(b, &o.m); err != nil || o.m == nil {
		o.bad = true
		o.m = map[string]json.RawMessage{}
		return o
	}
	ok := map[string]bool{}
	for _, k := range allowed {
		ok[k] = true
	}
	for k := range o.m {
		if !ok[k] {
			o.bad = true
		}
	}
	return o
}

func (o *Obj) fail() { o.bad = true }

// Str is a required string no longer than max characters (max <= 0 means unlimited).
func (o *Obj) Str(key string, max int) string {
	raw, ok := o.m[key]
	if !ok {
		o.fail()
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || (max > 0 && utf8.RuneCountInString(s) > max) {
		o.fail()
		return ""
	}
	return s
}

// OptStr is an optional string (absent or null gives ok=false).
func (o *Obj) OptStr(key string, max int) (string, bool) {
	raw, ok := o.m[key]
	if !ok || string(raw) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || (max > 0 && utf8.RuneCountInString(s) > max) {
		o.fail()
		return "", false
	}
	return s, true
}

// Int is a required integer of at least min.
func (o *Obj) Int(key string, min int64) int64 {
	raw, ok := o.m[key]
	if !ok {
		o.fail()
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil || f != float64(int64(f)) || int64(f) < min {
		o.fail()
		return 0
	}
	return int64(f)
}

// Num is a required number of at least min.
func (o *Obj) Num(key string, min float64) float64 {
	raw, ok := o.m[key]
	if !ok {
		o.fail()
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil || f < min {
		o.fail()
		return 0
	}
	return f
}

// OptNum is an optional number of at least min.
func (o *Obj) OptNum(key string, min float64) (float64, bool) {
	raw, ok := o.m[key]
	if !ok || string(raw) == "null" {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil || f < min {
		o.fail()
		return 0, false
	}
	return f, true
}

// Enum is a required string that must be one of values.
func (o *Obj) Enum(key string, values ...string) string {
	s := o.Str(key, 0)
	for _, v := range values {
		if s == v {
			return s
		}
	}
	o.fail()
	return ""
}

// Object is a required nested object whose keys must all be in allowed.
func (o *Obj) Object(key string, allowed ...string) *Obj {
	child := &Obj{m: map[string]json.RawMessage{}}
	raw, ok := o.m[key]
	if !ok {
		o.fail()
		child.bad = true
		return child
	}
	if err := json.Unmarshal(raw, &child.m); err != nil || child.m == nil {
		o.fail()
		child.bad = true
		child.m = map[string]json.RawMessage{}
		return child
	}
	okKeys := map[string]bool{}
	for _, k := range allowed {
		okKeys[k] = true
	}
	for k := range child.m {
		if !okKeys[k] {
			o.fail()
			child.bad = true
		}
	}
	return child
}

// Failed reports whether this object or a nested one was invalid.
func (o *Obj) Failed() bool { return o.bad }

// Absorb marks this object invalid when a nested object was.
func (o *Obj) Absorb(child *Obj) {
	if child.bad {
		o.bad = true
	}
}

// StringMap is a required object of string values (no other validation, like @IsObject()).
func (o *Obj) StringMap(key string) map[string]string {
	raw, ok := o.m[key]
	if !ok {
		o.fail()
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		o.fail()
		return nil
	}
	out := map[string]string{}
	for k, v := range m {
		s, isStr := v.(string)
		if !isStr {
			// @IsObject() accepts any object; the service then treats option values as strings.
			b, _ := json.Marshal(v)
			s = string(b)
		}
		out[k] = s
	}
	return out
}

// Array is a required JSON array (element types are not checked, like @IsArray()).
func (o *Obj) Array(key string) []json.RawMessage {
	raw, ok := o.m[key]
	if !ok {
		o.fail()
		return nil
	}
	var a []json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil || a == nil {
		o.fail()
		return nil
	}
	return a
}

// Strings is a required array whose string elements are kept (non-strings are dropped, like the Nest handlers).
func (o *Obj) Strings(key string) []string {
	out := []string{}
	for _, el := range o.Array(key) {
		var s string
		if json.Unmarshal(el, &s) == nil {
			out = append(out, s)
		}
	}
	return out
}

// Has reports whether the key was sent.
func (o *Obj) Has(key string) bool { _, ok := o.m[key]; return ok }

// Err is the validation error when anything was invalid, else nil.
func (o *Obj) Err() error {
	if o.bad {
		return invalidBody()
	}
	return nil
}

// Lenient reads the body as an arbitrary JSON object without validating it (for `@Body('ids')`-style handlers).
func Lenient(r *http.Request) map[string]any {
	b, err := ReadBody(r)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m == nil {
		return map[string]any{}
	}
	return m
}

var _ = rawBodyKey
