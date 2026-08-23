package parser

// utf8Reason classifies why a byte string is not valid UTF-8 — the port
// of upstream's UnicodeInvalidReason.
type utf8Reason int

const (
	utf8Valid utf8Reason = iota
	// utf8ByteMismatch is a malformed byte sequence (bad start byte or
	// missing/invalid continuation byte).
	utf8ByteMismatch
	// utf8InvalidUnicode is a well-formed sequence encoding an invalid
	// codepoint (surrogate or beyond U+10FFFF).
	utf8InvalidUnicode
)

// analyzeUTF8 walks s the way upstream's Utf8Proc::Analyze does and
// returns the first invalid sequence's classification and start offset
// (utf8Valid and -1 when the whole string is valid). Unlike the standard
// library's utf8.ValidString it accepts overlong encodings, matching the
// pinned build's effective behavior.
func analyzeUTF8(s string) (utf8Reason, int) {
	for i := 0; i < len(s); {
		c := s[i]
		if c&0x80 == 0 {
			i++
			continue
		}
		start := i
		var cont int
		var cp int32
		switch {
		case c&0xE0 == 0xC0:
			cont, cp = 1, int32(c&0x1F)
		case c&0xF0 == 0xE0:
			cont, cp = 2, int32(c&0x0F)
		case c&0xF8 == 0xF0:
			cont, cp = 3, int32(c&0x07)
		default:
			return utf8ByteMismatch, start
		}
		i++
		for j := 0; j < cont; j++ {
			if i >= len(s) || s[i]&0xC0 != 0x80 {
				return utf8ByteMismatch, start
			}
			cp = cp<<6 | int32(s[i]&0x3F)
			i++
		}
		if cp > 0x10FFFF || (cp >= 0xD800 && cp <= 0xDFFF) {
			return utf8InvalidUnicode, start
		}
	}
	return utf8Valid, -1
}

// validateQueryUTF8 raises the pinned build's whole-query UTF-8 error
// when src is not valid UTF-8, positioned at the invalid sequence.
func validateQueryUTF8(src string) *Error {
	reason, off := analyzeUTF8(src)
	if reason == utf8Valid {
		return nil
	}
	kind := "byte sequence mismatch"
	if reason == utf8InvalidUnicode {
		kind = "invalid unicode"
	}
	return errorAt(src, "Invalid UTF-8 in query ("+kind+")", off)
}
