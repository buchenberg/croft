package croft

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// canonicalDigestVersion is the version prefix of the digest wire format.
//
// It is inherited verbatim from the format this library was extracted from so
// that states digested here match states digested there: it is part of the
// hashed bytes, so changing it silently changes every digest. Treat it as a
// stable format identifier, not branding.
const canonicalDigestVersion = "shepherd.kernel.canonical.v2"

var canonicalPrefix = []byte(canonicalDigestVersion + "\n")

// sha256Sum returns the SHA-256 hash of the input bytes.
func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// CanonicalJSONBytes returns byte-stable canonical JSON for digest input: keys
// sorted at every level, compact separators, raw UTF-8, and CPython's number and
// string formatting.
//
// It is byte-identical to Python's
//
//	json.dumps(value, sort_keys=True, separators=(",", ":"),
//	           ensure_ascii=False, allow_nan=False).encode("utf-8")
//
// which is what `canonical_json_bytes` does in shepherd2. That identity is the
// point: digests over these bytes are compared across implementations.
//
// Two things Go's encoding/json gets wrong here, both of which produce a
// different digest silently rather than failing:
//
//   - it escapes <, > and & as \u003c, \u003e, \u0026, which CPython emits raw,
//     so any payload with a shell `&&` or a `<placeholder>` diverges;
//   - it has no float formatting that matches CPython's repr.
//
// Hence the hand-written writer below. Do not simplify it back to json.Marshal.
//
// One contract this imposes on callers: a payload decoded from JSON must be
// decoded with json.Decoder.UseNumber() (or into json.Number fields). Go's
// default interface{} decoding turns every number into float64, which erases
// Python's int/float distinction — JSON `1` and `1.0` are the same float64 but
// canonicalise to "1" and "1.0". The previous implementation papered over that
// by rendering integral float64s without a decimal point, which happened to fix
// integers and silently corrupted real float values.
func CanonicalJSONBytes(v any) ([]byte, error) {
	return canonicalJSON(v, false)
}

// CanonicalJSONASCIIBytes encodes with CPython's json.dumps *default*
// (ensure_ascii=True), which is a different escaping flavour from the one the
// record digest uses.
//
// The store needs both, and that is not an artefact of the Go port: shepherd2's
// trace_store.py runs two encoders in the same append path. Record and witness ids
// go through kernel.canonical.canonical_json_bytes (ensure_ascii=False), while its
// `_json_dumps` helper omits the argument, so the context id, the batch digest and
// the stored body/receipt JSON all use ensure_ascii=True. With an ASCII-only
// payload the two agree, which is why this is invisible in the existing tests.
func CanonicalJSONASCIIBytes(v any) ([]byte, error) {
	return canonicalJSON(v, true)
}

// canonicalJSON encodes v canonically, choosing the string-escaping flavour.
func canonicalJSON(v any, ascii bool) ([]byte, error) {
	buf := &canonicalBuf{ascii: ascii}
	if err := writeCanonicalValue(buf, v, make(map[canonicalVisit]bool)); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// canonicalBuf is the output buffer for one encoding pass. It carries the escape
// flavour so the writer need not thread a flag through every case; the embedded
// Builder promotes WriteByte, WriteString and Write, so the writer body is
// unchanged.
type canonicalBuf struct {
	strings.Builder
	ascii bool
}

// formatCanonicalFloat renders f the way CPython's repr does, which is what
// json.dumps emits for a float.
//
// CPython's rule is shortest-round-trip digits with exponent notation when
// decpt <= -4 || decpt > 16 — that is, fixed notation for 1e-4 <= |f| < 1e16 and
// scientific outside it. In scientific form the exponent carries an explicit
// sign and at least two digits ("1e+16", "1e-05") and the mantissa never gets a
// trailing ".0"; in fixed form an integral value does ("1.0", "100.0"), which is
// what distinguishes a float from an int in canonical output.
//
// Every case below was confirmed against CPython 3.13 by generating the vectors
// in testdata/canonical_edge_vectors_v0.json.
func formatCanonicalFloat(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		// Mirrors allow_nan=False, which raises instead of emitting NaN/Infinity.
		return "", fmt.Errorf("canonical JSON: %v has no JSON representation", f)
	}
	if f == 0 {
		// Both zeroes are representable, and CPython preserves the sign.
		if math.Signbit(f) {
			return "-0.0", nil
		}
		return "0.0", nil
	}

	// The shortest scientific form gives the decimal exponent the rule turns on.
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, expPart, ok := strings.Cut(sci, "e")
	if !ok {
		return "", fmt.Errorf("canonical JSON: no exponent in %q", sci)
	}
	exp, err := strconv.Atoi(expPart)
	if err != nil {
		return "", fmt.Errorf("canonical JSON: bad exponent %q in %q", expPart, sci)
	}

	if exp >= 16 || exp <= -5 {
		return mantissa + "e" + pythonExponent(exp), nil
	}

	fixed := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(fixed, ".") {
		fixed += ".0"
	}
	return fixed, nil
}

// pythonExponent formats a decimal exponent the way CPython does: explicit sign,
// at least two digits.
func pythonExponent(exp int) string {
	sign := "+"
	if exp < 0 {
		sign = "-"
		exp = -exp
	}
	digits := strconv.Itoa(exp)
	if len(digits) < 2 {
		digits = "0" + digits
	}
	return sign + digits
}

// writeCanonicalString writes s as CPython's json.dumps(ensure_ascii=False)
// would: UTF-8 passed through verbatim, with only the mandatory escapes.
//
// Iterating bytes rather than runes is deliberate. Every byte of a multi-byte
// UTF-8 sequence is >= 0x80, so none can collide with an escape, and copying
// them one at a time passes the sequence through unchanged — including the
// U+2028 and U+2029 that Python leaves raw and that many encoders escape.
func writeCanonicalString(buf *canonicalBuf, s string) error {
	if !utf8.ValidString(s) {
		// CPython raises UnicodeEncodeError when it encodes a lone surrogate as
		// UTF-8 to produce the final bytes, so this mirrors an existing failure
		// rather than inventing one. The alternatives are both silent: pass the
		// bytes through and emit invalid JSON, or substitute U+FFFD the way
		// encoding/json does and digest a string the caller never held.
		return fmt.Errorf("canonical JSON: string is not valid UTF-8: %q", s)
	}
	if buf.ascii {
		return writeCanonicalStringASCII(buf, s)
	}

	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if c < 0x20 {
				// Lowercase hex, four digits, as CPython emits.
				fmt.Fprintf(buf, `\u%04x`, c)
				continue
			}
			buf.WriteByte(c)
		}
	}
	buf.WriteByte('"')
	return nil
}

// writeCanonicalStringASCII escapes as CPython's json.dumps does with
// ensure_ascii=True — its default, and what the store's context-id and
// batch-digest helpers use.
//
// Every non-ASCII rune becomes \uXXXX with lowercase hex, and above U+FFFF a
// surrogate pair, exactly as CPython emits. DEL is escaped too. Note it does NOT
// HTML-escape <, > or & : Python never does, at either setting, which is a
// different behaviour from Go's encoding/json and the reason this is hand-written.
func writeCanonicalStringASCII(buf *canonicalBuf, s string) error {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			switch {
			case r < 0x20 || r == 0x7f:
				fmt.Fprintf(buf, `\u%04x`, r)
			case r < utf8.RuneSelf:
				buf.WriteByte(byte(r))
			case r > 0xFFFF:
				r -= 0x10000
				fmt.Fprintf(buf, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
			default:
				fmt.Fprintf(buf, `\u%04x`, r)
			}
		}
	}
	buf.WriteByte('"')
	return nil
}

// canonicalVisit identifies an in-progress container, so a cycle can be reported
// rather than overflowing the stack.
type canonicalVisit struct {
	kind reflect.Kind
	ptr  uintptr
}

// enterContainer marks a container as being written, reporting a cycle if it is
// already active.
func enterContainer(seen map[canonicalVisit]bool, kind reflect.Kind, ptr uintptr) error {
	if ptr == 0 {
		return nil
	}
	key := canonicalVisit{kind: kind, ptr: ptr}
	if seen[key] {
		return fmt.Errorf("canonical JSON: circular reference detected")
	}
	seen[key] = true
	return nil
}

func leaveContainer(seen map[canonicalVisit]bool, kind reflect.Kind, ptr uintptr) {
	if ptr != 0 {
		delete(seen, canonicalVisit{kind: kind, ptr: ptr})
	}
}

// validJSONNumber reports whether s matches the JSON number grammar:
//
//	-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?
//
// json.Number carries caller-supplied text, and strconv.ParseFloat is more
// permissive than JSON — it accepts "Inf", "NaN" and digit separators — so
// without this a malformed token would be written into digest input as invalid
// JSON, or accepted where Python's json.loads rejects it.
func validJSONNumber(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}

	// Integer part: a lone 0, or a non-zero digit followed by any digits.
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	default:
		return false
	}

	// Optional fraction; a '.' must be followed by at least one digit.
	if i < len(s) && s[i] == '.' {
		i++
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}

	// Optional exponent; the digits are mandatory once e/E appears.
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}

	return i == len(s)
}

// isJSONIntegerToken reports whether a JSON number token denotes an integer,
// meaning Python's json.loads would produce an int rather than a float.
func isJSONIntegerToken(s string) bool {
	return !strings.ContainsAny(s, ".eE")
}

// writeCanonicalValue writes v in canonical form.
//
// Map keys are sorted at every level, and only string keys are supported: Python
// coerces int/float/bool/None keys, but a non-string key means the caller built a
// payload the kernel never produces, so it is an error rather than a guess.
//
// seen holds the containers currently being written, so a self-referential map or
// slice is reported instead of overflowing the stack — CPython's json.dumps
// raises "Circular reference detected". It tracks *ancestors* rather than
// everything visited, because a value legitimately reachable by two paths is not
// a cycle.
//
// JSON-compatible typed containers ([]int, map[string]int, ...) are accepted, not
// only map[string]any and []any: the public payload API does not require callers
// to convert, and rejecting them would break existing callers for no gain. Types
// JSON cannot represent at all — structs, funcs, channels — remain an explicit
// error, because the previous json.Marshal fallback is exactly how the escaping
// divergence went unnoticed: a silent fallback produces a plausible digest over
// different bytes.
func writeCanonicalValue(buf *canonicalBuf, v any, seen map[canonicalVisit]bool) error {
	switch val := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if val {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		return writeCanonicalString(buf, val)
	case json.Number:
		// A json.Number carries the original token text, which is the only way to
		// recover Python's int/float distinction: encoding/json's default
		// interface{} decoding collapses every number to float64, and a JSON `1`
		// and `1.0` are different values to Python ("1" vs "1.0").
		//
		// Python's json.loads yields an int for a token with no fraction or
		// exponent and a float otherwise, then json.dumps renders accordingly.
		// Anything with a '.' or exponent is parsed and formatted as a float, so
		// `1e2` canonicalises to `100.0` as Python does rather than staying `1e2`.
		//
		// Callers decoding JSON payloads for digesting MUST use
		// json.Decoder.UseNumber() (or unmarshal into json.Number) to reach this
		// case; see the CanonicalJSONBytes doc comment.
		s := val.String()
		if !validJSONNumber(s) {
			return fmt.Errorf("canonical JSON: json.Number %q is not a valid JSON number", s)
		}
		if isJSONIntegerToken(s) {
			// Integer tokens are emitted as-is, which also preserves Python's
			// arbitrary-precision ints beyond float64's range — except negative
			// zero, which Python parses as the integer 0 and re-serialises as "0".
			if s == "-0" {
				buf.WriteString("0")
				return nil
			}
			buf.WriteString(s)
			return nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("canonical JSON: json.Number %q is not a number: %w", s, err)
		}
		out, err := formatCanonicalFloat(f)
		if err != nil {
			return err
		}
		buf.WriteString(out)
	case float64:
		s, err := formatCanonicalFloat(val)
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case float32:
		s, err := formatCanonicalFloat(float64(val))
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case int:
		buf.WriteString(strconv.Itoa(val))
	case int8:
		buf.WriteString(strconv.FormatInt(int64(val), 10))
	case int16:
		buf.WriteString(strconv.FormatInt(int64(val), 10))
	case int32:
		buf.WriteString(strconv.FormatInt(int64(val), 10))
	case int64:
		buf.WriteString(strconv.FormatInt(val, 10))
	case uint:
		buf.WriteString(strconv.FormatUint(uint64(val), 10))
	case uint8:
		buf.WriteString(strconv.FormatUint(uint64(val), 10))
	case uint16:
		buf.WriteString(strconv.FormatUint(uint64(val), 10))
	case uint32:
		buf.WriteString(strconv.FormatUint(uint64(val), 10))
	case uint64:
		buf.WriteString(strconv.FormatUint(val, 10))
	case []any:
		if val == nil {
			buf.WriteString("null")
			return nil
		}
		ptr := reflect.ValueOf(val).Pointer()
		if err := enterContainer(seen, reflect.Slice, ptr); err != nil {
			return err
		}
		defer leaveContainer(seen, reflect.Slice, ptr)

		buf.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalValue(buf, item, seen); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case []string:
		if val == nil {
			buf.WriteString("null")
			return nil
		}
		ptr := reflect.ValueOf(val).Pointer()
		if err := enterContainer(seen, reflect.Slice, ptr); err != nil {
			return err
		}
		defer leaveContainer(seen, reflect.Slice, ptr)

		buf.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalString(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		if val == nil {
			buf.WriteString("null")
			return nil
		}
		ptr := reflect.ValueOf(val).Pointer()
		if err := enterContainer(seen, reflect.Map, ptr); err != nil {
			return err
		}
		defer leaveContainer(seen, reflect.Map, ptr)

		// Sorting UTF-8 bytes equals sorting code points, which is what Python's
		// sort_keys does for str keys.
		keys := sortedKeys(len(val), func(yield func(string) bool) {
			for k := range val {
				if !yield(k) {
					return
				}
			}
		})
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonicalValue(buf, val[k], seen); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case map[string]string:
		if val == nil {
			buf.WriteString("null")
			return nil
		}
		ptr := reflect.ValueOf(val).Pointer()
		if err := enterContainer(seen, reflect.Map, ptr); err != nil {
			return err
		}
		defer leaveContainer(seen, reflect.Map, ptr)

		keys := sortedKeys(len(val), func(yield func(string) bool) {
			for k := range val {
				if !yield(k) {
					return
				}
			}
		})
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonicalString(buf, val[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return writeCanonicalReflect(buf, val, seen)
	}
	return nil
}

// sortedKeys collects keys via yield and returns them sorted.
func sortedKeys(n int, yield func(func(string) bool)) []string {
	keys := make([]string, 0, n)
	yield(func(k string) bool {
		keys = append(keys, k)
		return true
	})
	sort.Strings(keys)
	return keys
}

// writeCanonicalReflect handles JSON-compatible containers of named or
// non-interface element types, which the type switch above cannot enumerate.
// Anything JSON cannot represent is rejected.
func writeCanonicalReflect(buf *canonicalBuf, v any, seen map[canonicalVisit]bool) error {
	rv := reflect.ValueOf(v)

	switch rv.Kind() {
	case reflect.Interface, reflect.Pointer:
		if rv.IsNil() {
			buf.WriteString("null")
			return nil
		}
		return writeCanonicalValue(buf, rv.Elem().Interface(), seen)

	case reflect.Slice, reflect.Array:
		// A nil slice has no JSON representation of its own; encoding/json emits
		// null for it, and that is what callers previously got.
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			buf.WriteString("null")
			return nil
		}
		var ptr uintptr
		if rv.Kind() == reflect.Slice {
			ptr = rv.Pointer()
		}
		kind := reflect.Slice
		if err := enterContainer(seen, kind, ptr); err != nil {
			return err
		}
		defer leaveContainer(seen, kind, ptr)

		buf.WriteByte('[')
		for i := 0; i < rv.Len(); i++ {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalValue(buf, rv.Index(i).Interface(), seen); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil

	case reflect.Map:
		if rv.IsNil() {
			buf.WriteString("null")
			return nil
		}
		if rv.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("canonical JSON: map keys must be strings, got %s", rv.Type().Key())
		}
		ptr := rv.Pointer()
		if err := enterContainer(seen, reflect.Map, ptr); err != nil {
			return err
		}
		defer leaveContainer(seen, reflect.Map, ptr)

		keys := rv.MapKeys()
		names := make([]string, 0, len(keys))
		for _, k := range keys {
			names = append(names, k.String())
		}
		sort.Strings(names)

		buf.WriteByte('{')
		for i, name := range names {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalString(buf, name); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonicalValue(buf, rv.MapIndex(reflect.ValueOf(name)).Interface(), seen); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil

	default:
		return fmt.Errorf("canonical JSON: unsupported type %T", v)
	}
}

// CanonicalDigest returns the kernel digest for a canonical input payload.
//
// It is byte-identical to Python's canonical_digest:
// sha256(CANONICAL_PREFIX + canonical_json_bytes(value)), rendered as
// "sha256:<hex>".
func CanonicalDigest(v any) (string, error) {
	b, err := CanonicalJSONBytes(v)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(canonicalPrefix)
	h.Write(b)
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}
