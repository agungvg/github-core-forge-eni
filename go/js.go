// js.go — JavaScript semantics helpers, so the Go port answers byte-for-byte
// like the TypeScript original wherever the two languages differ:
// JSON encoding (no HTML escaping), Number()/parseInt, decodeURIComponent,
// URLSearchParams (ordered, form-encoded), Array.slice, String() coercion.

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// marshalJSON encodes v as JSON the way JSON.stringify does: no escaping of
// <, >, & (Go's default escapes them), keys of maps sorted (Go sorts map
// keys; the suite parses bodies, so order is unobservable).
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoder adds a trailing newline; strip it — callers add '\n' themselves.
	_ = enc.Encode(v)
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

// jsonQuote renders a Go string the way JSON.stringify would (for error
// messages that embed a quoted string, e.g. pkt-line bad length).
func jsonQuote(s string) string {
	return string(marshalJSON(s))
}

// parseJSON parses a JSON document the way JSON.parse does, into any
// (objects -> map[string]any, numbers -> float64). Trailing content after
// the first value is rejected, as JSON.parse rejects it.
func parseJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected token after JSON value")
		}
		return nil, err
	}
	return convertNumbers(v), nil
}

// convertNumbers turns json.Number values into float64 (matching JS number
// semantics) — but only where the value fits; huge exponents become +Inf
// the way JSON.parse would.
func convertNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			// JSON.parse('1e400') is Infinity.
			if strings.HasPrefix(t.String(), "-") {
				return math.Inf(-1)
			}
			return math.Inf(1)
		}
		return f
	case map[string]any:
		for k, x := range t {
			t[k] = convertNumbers(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = convertNumbers(x)
		}
		return t
	default:
		return v
	}
}

// jsNumber parses a string the way JavaScript Number() does.
// Returns (value, ok) where ok=false means NaN.
func jsNumber(s string) (float64, bool) {
	t := jsTrim(s)
	if t == "" {
		return 0, true
	}
	if t == "Infinity" || t == "+Infinity" {
		return math.Inf(1), true
	}
	if t == "-Infinity" {
		return math.Inf(-1), true
	}
	if strings.Contains(t, "_") {
		return math.NaN(), false
	}
	u := t
	neg := false
	if strings.HasPrefix(u, "+") {
		u = u[1:]
	} else if strings.HasPrefix(u, "-") {
		neg = true
		u = u[1:]
	}
	if len(u) > 2 {
		switch u[:2] {
		case "0x", "0X":
			v, err := strconv.ParseUint(u[2:], 16, 64)
			if err != nil {
				return math.NaN(), false
			}
			f := float64(v)
			if neg {
				f = -f
			}
			return f, true
		case "0b", "0B":
			v, err := strconv.ParseUint(u[2:], 2, 64)
			if err != nil {
				return math.NaN(), false
			}
			f := float64(v)
			if neg {
				f = -f
			}
			return f, true
		case "0o", "0O":
			v, err := strconv.ParseUint(u[2:], 8, 64)
			if err != nil {
				return math.NaN(), false
			}
			f := float64(v)
			if neg {
				f = -f
			}
			return f, true
		}
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return math.NaN(), false
	}
	return v, true
}

// jsParseIntHex parses a pkt-line length header the way parseInt(head, 16)
// does: leading spaces skipped, hex digits consumed, NaN when none.
func jsParseIntHex(head string) (int, error) {
	i := 0
	for i < len(head) && (head[i] == ' ' || head[i] == '\t' || head[i] == '\n' || head[i] == '\r' || head[i] == '\v' || head[i] == '\f') {
		i++
	}
	j := i
	for j < len(head) {
		c := head[j]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			j++
			continue
		}
		break
	}
	if j == i {
		return 0, fmt.Errorf("NaN")
	}
	v, err := strconv.ParseInt(head[i:j], 16, 32)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

// jsTrim trims the way String.prototype.trim does (WhiteSpace + LineTerminator).
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\uFEFF' || r == '\u00A0'
	})
}

// isJSSpace reports whether r is JS \s whitespace.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00A0', '\u1680', '\u2028', '\u2029', '\u202F', '\u205F', '\u3000', '\uFEFF':
		return true
	}
	return r >= '\u2000' && r <= '\u200A'
}

// jsDecodeURIComponent decodes a percent-encoded component the way
// decodeURIComponent does: throws (errors) on malformed % sequences and on
// % sequences that are not valid UTF-8.
func jsDecodeURIComponent(s string) (string, error) {
	out, err := url.PathUnescape(s)
	if err != nil {
		return "", err
	}
	if !utf8.ValidString(out) {
		return "", fmt.Errorf("URIError: URI malformed")
	}
	return out, nil
}

// jsString coerces a JSON value the way String(v) does.
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return "undefined"
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(t)
	case json.Number:
		f, _ := t.Float64()
		return jsNumberString(f)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			if e == nil {
				parts[i] = ""
			} else {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// jsNumberString renders a float64 the way String(n) does (ECMAScript
// Number::toString): shortest digits, plain notation for -6 < n <= 21,
// exponential otherwise with an unpadded exponent.
func jsNumberString(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if f == 0 {
		return "0"
	}
	neg := f < 0
	abs := math.Abs(f)
	// Shortest decimal in scientific form: d.ddddde±xx
	sci := strconv.FormatFloat(abs, 'e', -1, 64)
	// Split mantissa and exponent.
	ePos := strings.IndexByte(sci, 'e')
	mant, expStr := sci[:ePos], sci[ePos+1:]
	exp, _ := strconv.Atoi(expStr) // decimal exponent of the leading digit
	digits := strings.Replace(mant, ".", "", 1)
	k := len(digits)
	n := exp + 1 // position of the decimal point relative to the digit string
	var out string
	switch {
	case k <= n && n <= 21:
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		if k == 1 {
			out = digits + "e" + jsExp(exp)
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + jsExp(exp)
		}
	}
	if neg {
		return "-" + out
	}
	return out
}

func jsExp(exp int) string {
	if exp >= 0 {
		return "+" + strconv.Itoa(exp)
	}
	return strconv.Itoa(exp)
}

// jsIsInteger mirrors Number.isInteger for a float64.
func jsIsInteger(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f)
}

// jsTrunc mirrors Math.trunc.
func jsTrunc(f float64) int64 {
	if math.IsNaN(f) {
		return 0
	}
	return int64(math.Trunc(f))
}

// jsSlice applies Array.prototype.slice(start, end) index semantics to a
// length: returns the [lo, hi) window.
func jsSlice(length int, start, end float64) (int, int) {
	lo := sliceIndex(length, start, 0)
	hi := sliceIndex(length, end, length)
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

func sliceIndex(length int, idx float64, def int) int {
	if math.IsNaN(idx) {
		return def
	}
	n := int64(math.Trunc(idx))
	if n < 0 {
		v := int64(length) + n
		if v < 0 {
			return 0
		}
		return int(v)
	}
	if n > int64(length) {
		return length
	}
	return int(n)
}

// jsSliceFrom is slice(start) with start possibly negative.
func jsSliceFrom(length int, start float64) (int, int) {
	return jsSlice(length, start, float64(length))
}

// lenientBase64Decode decodes base64 the way Node's Buffer.from(s, 'base64')
// does: characters outside the alphabet are ignored, missing padding is
// tolerated.
func lenientBase64Decode(s string) []byte {
	var clean strings.Builder
	for _, c := range s {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			clean.WriteRune(c)
		}
	}
	t := clean.String()
	if m := len(t) % 4; m != 0 {
		t += strings.Repeat("=", 4-m)
	}
	out, err := base64.StdEncoding.DecodeString(t)
	if err != nil {
		// Last resort: decode what we can.
		out, _ = base64.StdEncoding.DecodeString(strings.TrimRight(t, "="))
		if out == nil {
			return []byte{}
		}
	}
	return out
}

// lenientBase64urlDecode decodes base64url the way Node's Buffer.from(s,
// 'base64url') does: invalid characters ignored, padding optional.
func lenientBase64urlDecode(s string) []byte {
	var clean strings.Builder
	for _, c := range s {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '=' {
			clean.WriteRune(c)
		}
	}
	t := clean.String()
	if m := len(t) % 4; m != 0 {
		t += strings.Repeat("=", 4-m)
	}
	out, err := base64.URLEncoding.DecodeString(t)
	if err != nil {
		out, _ = base64.RawURLEncoding.DecodeString(strings.TrimRight(t, "="))
		if out == nil {
			return []byte{}
		}
	}
	return out
}

func base64urlEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------------------------------------------------------------- query params

// queryParams is an ordered URLSearchParams: insertion order preserved,
// get() returns the first value, set() replaces in place, toString() uses
// the application/x-www-form-urlencoded serializer.
type queryParams struct {
	pairs [][2]string
}

func parseQueryParams(raw string) queryParams {
	var qp queryParams
	if raw == "" {
		return qp
	}
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v := part, ""
		if i := strings.IndexByte(part, '='); i >= 0 {
			k, v = part[:i], part[i+1:]
		}
		qp.pairs = append(qp.pairs, [2]string{queryUnescape(k), queryUnescape(v)})
	}
	return qp
}

// queryUnescape decodes a query component: '+' becomes space, %XX decoded.
// Invalid % sequences are left as-is (matching URLSearchParams, which never
// throws on parse).
func queryUnescape(s string) string {
	if !strings.ContainsAny(s, "+%") {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '+' {
			out.WriteByte(' ')
			continue
		}
		if c == '%' && i+2 < len(s) {
			if h, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				out.WriteByte(byte(h))
				i += 2
				continue
			}
		}
		out.WriteByte(c)
	}
	return out.String()
}

func (qp *queryParams) Get(name string) (string, bool) {
	for _, p := range qp.pairs {
		if p[0] == name {
			return p[1], true
		}
	}
	return "", false
}

func (qp *queryParams) Set(name, value string) {
	for i, p := range qp.pairs {
		if p[0] == name {
			qp.pairs[i][1] = value
			// delete any later duplicates
			out := qp.pairs[:i+1]
			for _, q := range qp.pairs[i+1:] {
				if q[0] != name {
					out = append(out, q)
				}
			}
			qp.pairs = out
			return
		}
	}
	qp.pairs = append(qp.pairs, [2]string{name, value})
}

func (qp *queryParams) Clone() queryParams {
	out := queryParams{pairs: make([][2]string, len(qp.pairs))}
	copy(out.pairs, qp.pairs)
	return out
}

// formEncode renders one component the way URLSearchParams.toString does:
// [A-Za-z0-9*-._] verbatim, space as '+', everything else %XX uppercase.
func formEncode(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			out.WriteByte('+')
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '*':
			out.WriteByte(c)
		default:
			fmt.Fprintf(&out, "%%%02X", c)
		}
	}
	return out.String()
}

func (qp *queryParams) String() string {
	parts := make([]string, len(qp.pairs))
	for i, p := range qp.pairs {
		parts[i] = formEncode(p[0]) + "=" + formEncode(p[1])
	}
	return strings.Join(parts, "&")
}

// asRecord mirrors the TS helper: objects pass through, everything else is {}.
func asRecord(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// asStringValue mirrors the TS helper: strings pass through, else undefined.
func asStringValue(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// intParam mirrors rest.ts intParam: Number(raw), finite and > 0, truncated,
// else the fallback.
func intParam(qp queryParams, name string, fallback int) int {
	raw, ok := qp.Get(name)
	if !ok {
		return fallback
	}
	n, valid := jsNumber(raw)
	if !valid || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return fallback
	}
	return int(math.Trunc(n))
}
