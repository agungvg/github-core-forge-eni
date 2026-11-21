// js.go — Go stand-ins for JavaScript built-ins.
//
// The forge was specified against JS semantics, and the conformance
// suite was written with those semantics in mind. Where Go and JS
// disagree — JSON escaping, Number() coercion, parseInt, URI decoding,
// URLSearchParams ordering, Array slice indexing, String() coercion —
// this file bridges the gap so the bytes the server sends match what a
// JS implementation would have produced.
//
// Everything here is a pure transformation of values already in memory:
// no network, no disk, no clock, no randomness.

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

// --------------------------------------------------------------------- JSON

// marshalJSON renders v the way JSON.stringify would: angle brackets
// and ampersands stay literal (Go's encoder escapes them by default),
// and map keys come out sorted, which encoding/json does for us. The
// encoder's trailing newline is stripped; callers that want one append
// it themselves.
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// jsonQuote quotes one string as a JSON string literal, for error
// messages that embed a quoted value.
func jsonQuote(s string) string {
	return string(marshalJSON(s))
}

// parseJSON is JSON.parse: one value, no trailing junk, with numbers
// decoded at full precision first and converted to float64 afterwards.
func parseJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected token after JSON value")
		}
		return nil, err
	}
	return convertNumbers(value), nil
}

// convertNumbers walks a decoded document replacing json.Number leaves
// with float64. A literal too large for a float64 becomes an infinity
// with the literal's sign, exactly as JSON.parse('1e400') yields
// Infinity.
func convertNumbers(v any) any {
	switch node := v.(type) {
	case json.Number:
		f, err := node.Float64()
		if err != nil {
			if strings.HasPrefix(node.String(), "-") {
				return math.Inf(-1)
			}
			return math.Inf(1)
		}
		return f
	case map[string]any:
		for key, child := range node {
			node[key] = convertNumbers(child)
		}
		return node
	case []any:
		for i, child := range node {
			node[i] = convertNumbers(child)
		}
		return node
	default:
		return v
	}
}

// ------------------------------------------------------------------- numbers

// jsNumber is Number(s): surrounding JS whitespace is ignored, an empty
// (or whitespace-only) string is 0, the Infinity spellings are honoured,
// underscores are always invalid, and 0x/0b/0o prefixes take a radix
// parse. The boolean reports whether the result is a real number; false
// means NaN.
func jsNumber(s string) (float64, bool) {
	trimmed := jsTrim(s)
	if trimmed == "" {
		return 0, true
	}
	switch trimmed {
	case "Infinity", "+Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}
	if strings.Contains(trimmed, "_") {
		return math.NaN(), false
	}
	body, negative := trimmed, false
	if strings.HasPrefix(body, "+") {
		body = body[1:]
	} else if strings.HasPrefix(body, "-") {
		negative, body = true, body[1:]
	}
	if len(body) > 2 {
		if radix := jsRadixPrefix(body); radix != 0 {
			parsed, err := strconv.ParseUint(body[2:], radix, 64)
			if err != nil {
				return math.NaN(), false
			}
			result := float64(parsed)
			if negative {
				result = -result
			}
			return result, true
		}
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return math.NaN(), false
	}
	return parsed, true
}

// jsRadixPrefix returns the radix a 0x/0b/0o-prefixed literal uses, or 0
// when the string carries no such prefix.
func jsRadixPrefix(s string) int {
	if len(s) < 2 {
		return 0
	}
	switch s[:2] {
	case "0x", "0X":
		return 16
	case "0b", "0B":
		return 2
	case "0o", "0O":
		return 8
	}
	return 0
}

// jsParseIntHex is parseInt(head, 16) on a packet-line length header:
// leading JS whitespace is skipped, then hex digits are consumed; with
// no hex digit at all the result is a NaN error.
func jsParseIntHex(head string) (int, error) {
	pos := 0
	for pos < len(head) {
		c := head[pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' {
			pos++
			continue
		}
		break
	}
	end := pos
	for end < len(head) {
		c := head[end]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			break
		}
		end++
	}
	if end == pos {
		return 0, fmt.Errorf("NaN")
	}
	value, err := strconv.ParseInt(head[pos:end], 16, 32)
	if err != nil {
		return 0, err
	}
	return int(value), nil
}

// jsNumberString is String(n) for a float64 — ECMAScript
// Number::toString. The shortest decimal digits are taken from Go's 'e'
// format, then laid out in plain notation when the decimal point lands
// in [-6, 21] and in exponential notation (unpadded exponent, always
// signed) otherwise.
func jsNumberString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	negative := f < 0
	magnitude := math.Abs(f)
	scientific := strconv.FormatFloat(magnitude, 'e', -1, 64)
	marker := strings.IndexByte(scientific, 'e')
	mantissa, expText := scientific[:marker], scientific[marker+1:]
	exponent, _ := strconv.Atoi(expText)
	digits := strings.Replace(mantissa, ".", "", 1)
	point := exponent + 1 // digits before the decimal point
	var rendered string
	switch {
	case len(digits) <= point && point <= 21:
		rendered = digits + strings.Repeat("0", point-len(digits))
	case 0 < point && point <= 21:
		rendered = digits[:point] + "." + digits[point:]
	case -6 < point && point <= 0:
		rendered = "0." + strings.Repeat("0", -point) + digits
	case len(digits) == 1:
		rendered = digits + "e" + jsExp(exponent)
	default:
		rendered = digits[:1] + "." + digits[1:] + "e" + jsExp(exponent)
	}
	if negative {
		return "-" + rendered
	}
	return rendered
}

// jsExp formats an exponent the JS way: always signed, never
// zero-padded.
func jsExp(exp int) string {
	if exp >= 0 {
		return "+" + strconv.Itoa(exp)
	}
	return strconv.Itoa(exp)
}

// jsIsInteger is Number.isInteger.
func jsIsInteger(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f)
}

// jsTrunc is Math.trunc, with NaN mapping to 0.
func jsTrunc(f float64) int64 {
	if math.IsNaN(f) {
		return 0
	}
	return int64(math.Trunc(f))
}

// ------------------------------------------------------------ strings & slices

// jsTrim is String.prototype.trim: Unicode WhiteSpace plus line
// terminators, plus the two extra code points JS counts (BOM, NBSP).
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\uFEFF' || r == '\u00A0'
	})
}

// isJSSpace reports whether r is one of the characters JS \s matches.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00A0', '\u1680', '\u2028', '\u2029', '\u202F', '\u205F', '\u3000', '\uFEFF':
		return true
	}
	return r >= '\u2000' && r <= '\u200A'
}

// jsDecodeURIComponent is decodeURIComponent: percent-decoding that
// fails on a malformed escape and on escapes that do not form valid
// UTF-8.
func jsDecodeURIComponent(s string) (string, error) {
	decoded, err := url.PathUnescape(s)
	if err != nil {
		return "", err
	}
	if !utf8.ValidString(decoded) {
		return "", fmt.Errorf("URIError: URI malformed")
	}
	return decoded, nil
}

// jsString is String(v). Note the JS quirks preserved here: null and
// undefined become the words "null" and "undefined" (nil in our decoded
// JSON is the undefined case), arrays join their elements with commas
// (nullish elements contribute nothing), and plain objects become the
// literal "[object Object]".
func jsString(v any) string {
	switch val := v.(type) {
	case nil:
		return "undefined"
	case string:
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(val)
	case json.Number:
		f, _ := val.Float64()
		return jsNumberString(f)
	case []any:
		parts := make([]string, len(val))
		for i, elem := range val {
			if elem == nil {
				parts[i] = ""
			} else {
				parts[i] = jsString(elem)
			}
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// jsSlice returns the [lo, hi) window Array.prototype.slice(start, end)
// selects from a collection of the given length.
func jsSlice(length int, start, end float64) (int, int) {
	lo := sliceIndex(length, start, 0)
	hi := sliceIndex(length, end, length)
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

// sliceIndex resolves one slice bound: NaN takes the default, negatives
// count back from the end (clamped at 0), values past the end clamp to
// the length.
func sliceIndex(length int, idx float64, def int) int {
	if math.IsNaN(idx) {
		return def
	}
	n := int64(math.Trunc(idx))
	switch {
	case n < 0:
		shifted := int64(length) + n
		if shifted < 0 {
			return 0
		}
		return int(shifted)
	case n > int64(length):
		return length
	default:
		return int(n)
	}
}

// jsSliceFrom is slice(start) — everything from start to the end.
func jsSliceFrom(length int, start float64) (int, int) {
	return jsSlice(length, start, float64(length))
}

// -------------------------------------------------------------------- base64

// stripBase64Alphabet drops every rune that is not part of the named
// base64 alphabet (plus '='), the way Node's Buffer.from tolerates stray
// characters instead of failing.
func stripBase64Alphabet(s string, urlSafe bool) string {
	var clean strings.Builder
	clean.Grow(len(s))
	for _, c := range s {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '=':
			clean.WriteRune(c)
		case urlSafe && (c == '-' || c == '_'):
			clean.WriteRune(c)
		case !urlSafe && (c == '+' || c == '/'):
			clean.WriteRune(c)
		}
	}
	return clean.String()
}

// keepBase64Alphabet drops every rune that is not part of the standard
// base64 alphabet (plus '=').
func keepBase64Alphabet(s string) string {
	return stripBase64Alphabet(s, false)
}

// padBase64 appends '=' until the length is a multiple of four.
func padBase64(s string) string {
	if rem := len(s) % 4; rem != 0 {
		return s + strings.Repeat("=", 4-rem)
	}
	return s
}

// decodePaddedBestEffort decodes a padded base64 string, falling back to
// a padding-stripped decode when the padded form fails.
func decodePaddedBestEffort(padded string, decode func(string) ([]byte, error)) []byte {
	out, err := decode(padded)
	if err != nil {
		out, _ = decode(strings.TrimRight(padded, "="))
		if out == nil {
			return []byte{}
		}
	}
	return out
}

// lenientBase64Decode is Node's Buffer.from(s, 'base64'): foreign
// characters ignored, missing padding tolerated, and a best-effort
// decode of whatever survives rather than an error.
func lenientBase64Decode(s string) []byte {
	return decodePaddedBestEffort(padBase64(keepBase64Alphabet(s)), base64.StdEncoding.DecodeString)
}

// lenientBase64urlDecode is Node's Buffer.from(s, 'base64url'): same
// leniency as lenientBase64Decode, but over the URL-safe alphabet.
func lenientBase64urlDecode(s string) []byte {
	cleaned := padBase64(stripBase64Alphabet(s, true))
	out, err := base64.URLEncoding.DecodeString(cleaned)
	if err != nil {
		out, _ = base64.RawURLEncoding.DecodeString(strings.TrimRight(cleaned, "="))
		if out == nil {
			return []byte{}
		}
	}
	return out
}

// base64urlEncode renders bytes in unpadded base64url, the encoding the
// opaque global ids use.
func base64urlEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// --------------------------------------------------------------- query strings

// queryParams is URLSearchParams: an ordered list of pairs. Get returns
// the first value for a name, Set replaces in place (dropping later
// duplicates), and String serializes as
// application/x-www-form-urlencoded.
type queryParams struct {
	pairs [][2]string
}

// parseQueryParams splits a raw query string on '&'. A segment without
// '=' is a key with an empty value; empty segments are skipped.
func parseQueryParams(raw string) queryParams {
	var qp queryParams
	if raw == "" {
		return qp
	}
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		key, value := part, ""
		if i := strings.IndexByte(part, '='); i >= 0 {
			key, value = part[:i], part[i+1:]
		}
		qp.pairs = append(qp.pairs, [2]string{queryUnescape(key), queryUnescape(value)})
	}
	return qp
}

// queryUnescape decodes one query component: '+' is a space and %XX is
// the byte it names. A '%' that is not followed by two hex digits is
// left alone — URLSearchParams never throws while parsing.
func queryUnescape(s string) string {
	if !strings.ContainsAny(s, "+%") {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '+' {
			out.WriteByte(' ')
			continue
		}
		if c == '%' && i+2 < len(s) {
			if hex, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				out.WriteByte(byte(hex))
				i += 2
				continue
			}
		}
		out.WriteByte(c)
	}
	return out.String()
}

// Get returns the first value stored under name.
func (qp *queryParams) Get(name string) (string, bool) {
	for _, pair := range qp.pairs {
		if pair[0] == name {
			return pair[1], true
		}
	}
	return "", false
}

// Set replaces the first pair for name and removes any later ones with
// the same name; a missing name is appended.
func (qp *queryParams) Set(name, value string) {
	for i, pair := range qp.pairs {
		if pair[0] != name {
			continue
		}
		qp.pairs[i][1] = value
		kept := qp.pairs[:i+1]
		for _, later := range qp.pairs[i+1:] {
			if later[0] != name {
				kept = append(kept, later)
			}
		}
		qp.pairs = kept
		return
	}
	qp.pairs = append(qp.pairs, [2]string{name, value})
}

// Clone deep-copies the pair list.
func (qp *queryParams) Clone() queryParams {
	out := queryParams{pairs: make([][2]string, len(qp.pairs))}
	copy(out.pairs, qp.pairs)
	return out
}

// formEncode renders one component the way URLSearchParams.toString
// does: alphanumerics plus *-._ pass through, a space becomes '+', and
// every other byte becomes an uppercase %XX escape.
func formEncode(s string) string {
	var out strings.Builder
	out.Grow(len(s))
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

// String serializes the pairs in insertion order, joined with '&'.
func (qp *queryParams) String() string {
	parts := make([]string, len(qp.pairs))
	for i, pair := range qp.pairs {
		parts[i] = formEncode(pair[0]) + "=" + formEncode(pair[1])
	}
	return strings.Join(parts, "&")
}

// ------------------------------------------------------------- JSON accessors

// asRecord passes a decoded JSON object through and turns anything else
// into an empty object.
func asRecord(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// asStringValue passes a string through and reports anything else as
// absent.
func asStringValue(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// intParam reads a query parameter as Number(raw): it must be finite
// and greater than zero, and is then truncated toward zero. Anything
// else yields the fallback.
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
