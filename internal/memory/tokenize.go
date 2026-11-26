package memory

import (
	"strings"
	"unicode"
)

// TokenizeForSearch turns mixed Chinese/English text and code identifiers into
// space-separated FTS5 tokens using ASCII word tokens, intact identifier forms,
// and CJK 2-gram sliding windows (preserving single CJK characters).
func TokenizeForSearch(text string) string {
	tokens := extractTokens(text)
	return strings.Join(tokens, " ")
}

// BuildFTSQuery converts a user query into a safe FTS5 MATCH expression.
// Terms are joined with OR so partial overlap scores through BM25 rather than
// dropping results when one incidental word is absent.
func BuildFTSQuery(query string) string {
	terms := extractTokens(query)
	if len(terms) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		escaped := strings.ReplaceAll(term, `"`, `""`)
		quoted = append(quoted, `"`+escaped+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// ExtractQueryTerms returns deduplicated normalized terms for scoring/entity matching.
func ExtractQueryTerms(query string) []string {
	return extractTokens(query)
}

func extractTokens(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	seen := make(map[string]struct{})
	var out []string
	add := func(tok string) {
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok == "" {
			return
		}
		if _, exists := seen[tok]; exists {
			return
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}

	var asciiBuf []rune
	var identBuf []rune
	var cjkBuf []rune

	flushASCII := func() {
		if len(asciiBuf) > 0 {
			add(string(asciiBuf))
			asciiBuf = asciiBuf[:0]
		}
	}
	flushIdent := func() {
		if len(identBuf) > 0 {
			tok := strings.Trim(string(identBuf), "._-/:")
			if strings.ContainsAny(tok, "._-/:") && len(tok) >= 3 {
				add(tok)
			}
			identBuf = identBuf[:0]
		}
	}
	flushCJK := func() {
		n := len(cjkBuf)
		if n == 0 {
			return
		}
		if n == 1 {
			add(string(cjkBuf))
		} else {
			for i := 0; i < n-1; i++ {
				add(string(cjkBuf[i : i+2]))
			}
			if n <= 4 {
				add(string(cjkBuf))
			}
		}
		cjkBuf = cjkBuf[:0]
	}

	for _, r := range text {
		switch {
		case isCJK(r):
			flushASCII()
			flushIdent()
			cjkBuf = append(cjkBuf, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			asciiBuf = append(asciiBuf, r)
			identBuf = append(identBuf, r)
		case r == '.' || r == '_' || r == '-' || r == '/' || r == ':':
			flushCJK()
			flushASCII()
			if len(identBuf) > 0 {
				identBuf = append(identBuf, r)
			}
		default:
			flushCJK()
			flushASCII()
			flushIdent()
		}
	}
	flushCJK()
	flushASCII()
	flushIdent()

	return out
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
