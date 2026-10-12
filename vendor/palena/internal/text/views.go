package text

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bitkaio/palena-litellm-pseudonymizer/internal/presidio"
)

// delimMask marks the bytes of structural punctuation (JSON/CSV/Python-repr
// delimiters, brackets, `#`, `:` and the literal two-character escapes
// `\n \r \t \" \\ \'`). Apostrophes inside words (Don't, O'Brien) are kept.
func delimMask(text string) []bool {
	m := make([]bool, len(text))
	any := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '\\' && i+1 < len(text) && strings.IndexByte("nrt\"\\'", text[i+1]) >= 0:
			m[i], m[i+1] = true, true
			i++
			any = true
		case c == '\'' && i > 0 && i+1 < len(text) && isLetterByte(text[i-1]) && isLetterByte(text[i+1]):
			// apostrophe inside a word stays
		case strings.IndexByte(",;|\t\"'()<>[]{}:=#\\", c) >= 0:
			m[i] = true
			any = true
		}
	}
	if !any {
		return nil
	}
	return m
}

// explodeViews returns same-length copies of text with the structural
// punctuation neutralised. NER models are context-sensitive and routinely miss
// a name embedded in JSON-escaped tool output, CSV rows or code comments
// ("1:# Maintainer: X\n" inside {"output":"..."}); in these views the same
// name is found. Two renderings, because each NER model trips on different
// ones: delimiters as line breaks, and delimiter runs as ". " sentence breaks.
// Offsets are identical to text, so detections map back 1:1.
func explodeViews(text string) []string {
	m := delimMask(text)
	if m == nil {
		return nil
	}
	nl := []byte(text)
	for i := range nl {
		if m[i] {
			nl[i] = '\n'
		}
	}
	// spaces hugging a line break become line breaks too: a leading space
	// ("\n Sviatoslav Ivanov") makes the model find nothing
	for i := 0; i < len(nl); i++ {
		if nl[i] == '\n' {
			for j := i - 1; j >= 0 && nl[j] == ' '; j-- {
				nl[j] = '\n'
			}
			for j := i + 1; j < len(nl) && nl[j] == ' '; j++ {
				nl[j] = '\n'
			}
		}
	}
	dot := []byte(text)
	for i := 0; i < len(dot); {
		if !m[i] {
			i++
			continue
		}
		j := i
		for j < len(dot) && m[j] {
			j++
		}
		for k := i; k < j; k++ {
			dot[k] = ' '
		}
		if j-i > 1 {
			dot[i] = '.'
		}
		i = j
	}
	return []string{string(dot), string(nl)}
}

// tokenChar: characters that can be part of a path / filename / identifier.
func tokenChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_./~-@+", r)
}

// surroundingToken returns the maximal identifier-like token around [start,end).
func surroundingToken(text string, start, end int) string {
	s, e := start, end
	for s > 0 {
		r, w := utf8.DecodeLastRuneInString(text[:s])
		if !tokenChar(r) {
			break
		}
		s -= w
	}
	for e < len(text) {
		r, w := utf8.DecodeRuneInString(text[e:])
		if !tokenChar(r) {
			break
		}
		e += w
	}
	return strings.Trim(text[s:e], ".-")
}

var nameLikePrefixes = []string{"Mc", "Mac", "De", "Di", "Van", "Von", "Le", "La", "O"}

func isAllCaps(w string) bool {
	letters := 0
	for _, r := range w {
		if unicode.IsLetter(r) {
			if !unicode.IsUpper(r) {
				return false
			}
			letters++
		}
	}
	return letters >= 2
}

func hasInnerCamel(w string) bool {
	prevLower := false
	for _, r := range w {
		if unicode.IsUpper(r) && prevLower {
			return true
		}
		prevLower = unicode.IsLower(r)
	}
	return false
}

func looksLikeFileOrDomain(tok string) bool {
	i := strings.LastIndexByte(tok, '.')
	if i <= 0 || i == len(tok)-1 {
		return false
	}
	ext := tok[i+1:]
	if len(ext) > 6 {
		return false
	}
	for _, r := range ext {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// isNonPIIShape reports whether a PERSON/ORGANIZATION span is really part of a
// path, filename, URL, identifier, ALLCAPS acronym or CamelCase product name.
func isNonPIIShape(text string, d presidio.Detection) bool {
	if d.Start < 0 || d.End > len(text) || d.Start >= d.End {
		return false
	}
	span := text[d.Start:d.End]
	tok := surroundingToken(text, d.Start, d.End)
	if strings.Contains(tok, "://") || strings.HasPrefix(tok, "//") {
		return true // URL
	}
	if strings.ContainsAny(tok, "/~") {
		// path component: "src", "lib", "app.py" are not people, but "/home/Sviatoslav Ivanov/"
		// or "/Users/Ivanov/" leak a user name - keep Capitalised words
		if !hasCapitalisedWord(span) {
			return true
		}
	}
	if strings.Contains(tok, "_") {
		return true // snake_case / SCREAMING_CASE identifier
	}
	if strings.Contains(tok, "@") {
		return false // e-mail parts are handled by the e-mail recognizer
	}
	if !strings.Contains(tok, "/") && looksLikeFileOrDomain(tok) {
		return true // CLAUDE.md, AGENTS.md, app.py, example.com
	}
	words := strings.Fields(span)
	if len(words) == 0 {
		return false
	}
	if _, ok := stopWords[strings.ToLower(span)]; ok {
		return true
	}
	allCaps, allCamel, anyDigitMix := true, true, false
	for _, w := range words {
		if !isAllCaps(w) {
			allCaps = false
		}
		camel := hasInnerCamel(w)
		for _, p := range nameLikePrefixes {
			if camel && strings.HasPrefix(w, p) && len(w) > len(p) && unicode.IsUpper(rune(w[len(p)])) && !hasInnerCamel(w[len(p):]) {
				camel = false // McDonald, DeShawn, OBrien
			}
		}
		if !camel {
			allCamel = false
		}
		if strings.IndexFunc(w, unicode.IsDigit) >= 0 && strings.IndexFunc(w, unicode.IsLetter) >= 0 {
			anyDigitMix = true
		}
	}
	return allCaps || allCamel || anyDigitMix
}

// filterNonPII drops PERSON/ORGANIZATION detections that are structurally not
// a person/company name (see isNonPIIShape).
func filterNonPII(text string, dets []presidio.Detection) []presidio.Detection {
	out := dets[:0:0]
	for _, d := range dets {
		if d.EntityType == "PERSON" || d.EntityType == "ORGANIZATION" {
			d = trimEdgePunct(text, d)
		}
		if (d.EntityType == "PERSON" || d.EntityType == "ORGANIZATION") && (isNonPIIShape(text, d) || !hasNameLetters(text, d)) {
			continue
		}
		if d.EntityType == "PHONE_NUMBER" && !looksLikeFormattedPhone(text, d) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// allowListed: the whole span, or every word of it, is on the allow-list.
func allowListed(allow map[string]struct{}, span string) bool {
	if len(allow) == 0 {
		return false
	}
	span = strings.TrimSpace(span)
	if _, ok := allow[strings.ToLower(span)]; ok {
		return true
	}
	words := strings.FieldsFunc(span, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",;:()[]{}\"'`", r)
	})
	if len(words) == 0 {
		return false
	}
	for _, w := range words {
		w = strings.ToLower(strings.Trim(w, ".-"))
		if _, ok := allow[w]; !ok {
			return false
		}
	}
	return true
}

// isLetterByte: ASCII letter or any UTF-8 byte of a multi-byte character.
func isLetterByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// splitAtMask cuts detections found in an exploded view at the neutralised
// delimiter bytes (the NER model glued "Maintainer  Sviatoslav Ivanov" because
// ':' had been blanked) and takes their text from the original.
func splitAtMask(text string, mask []bool, dets []presidio.Detection) []presidio.Detection {
	var out []presidio.Detection
	for _, d := range dets {
		if mask == nil || d.Start < 0 || d.End > len(text) || d.Start >= d.End {
			out = append(out, d)
			continue
		}
		i := d.Start
		for i < d.End {
			for i < d.End && mask[i] {
				i++
			}
			j := i
			for j < d.End && !mask[j] {
				j++
			}
			if j > i {
				p := d
				p.Start, p.End = i, j
				// trim blanks at the edges
				for p.Start < p.End && (text[p.Start] == ' ' || text[p.Start] == '\n') {
					p.Start++
				}
				for p.End > p.Start && (text[p.End-1] == ' ' || text[p.End-1] == '\n') {
					p.End--
				}
				if p.End > p.Start {
					p.Text = text[p.Start:p.End]
					out = append(out, p)
				}
			}
			i = j
		}
	}
	return out
}

func hasCapitalisedWord(span string) bool {
	for _, w := range strings.Fields(span) {
		r, n := utf8.DecodeRuneInString(w)
		if unicode.IsUpper(r) && len(w) > n && strings.IndexFunc(w[n:], unicode.IsUpper) < 0 {
			return true
		}
	}
	return false
}

// stopWords are plain function words the NER models sometimes tag.
var stopWords = map[string]struct{}{
	"and": {}, "also": {}, "or": {}, "the": {}, "a": {}, "an": {}, "is": {}, "are": {}, "to": {}, "of": {},
	"in": {}, "on": {}, "for": {}, "with": {}, "by": {}, "as": {}, "at": {}, "if": {}, "not": {}, "no": {},
	"и": {}, "также": {}, "или": {}, "в": {}, "на": {}, "не": {}, "что": {}, "это": {},
}

// hasNameLetters: a PERSON/ORGANIZATION span must contain a Capitalised word
// with at least two letters. Lower-case NER hits from tool output and code
// ("i", "ae", "https", "tmpdir", "contact") are not names.
func hasNameLetters(text string, d presidio.Detection) bool {
	span := d.Text
	if span == "" {
		if d.Start < 0 || d.End > len(text) || d.Start >= d.End {
			return true
		}
		span = text[d.Start:d.End]
	}
	letters := 0
	for _, r := range span {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters < 2 {
		return false
	}
	for _, w := range strings.Fields(span) {
		if r, _ := utf8.DecodeRuneInString(w); unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// looksLikeFormattedPhone: bare digit runs ("036071417" - ids, timings, line
// numbers in tool output) are not phone numbers; require a leading "+" or a
// separator between digit groups.
func looksLikeFormattedPhone(text string, d presidio.Detection) bool {
	span := d.Text
	if span == "" {
		if d.Start < 0 || d.End > len(text) || d.Start >= d.End {
			return true
		}
		span = text[d.Start:d.End]
	}
	span = strings.TrimSpace(span)
	if strings.HasPrefix(span, "+") {
		return true
	}
	return strings.ContainsAny(span, " -().")
}

const edgePunct = "-`'\"()[]{}<>,;:*#|~"

// trimEdgePunct strips stray punctuation NER glues onto a name ("-Margaret
// Holloway" from a diff line, "Ivanov`" from markdown) so the same person keeps
// one pseudonym. Offsets are byte offsets into text.
func trimEdgePunct(text string, d presidio.Detection) presidio.Detection {
	if d.Start < 0 || d.End > len(text) || d.Start >= d.End || (d.Text != "" && text[d.Start:d.End] != d.Text) {
		return d
	}
	for d.Start < d.End && strings.IndexByte(edgePunct, text[d.Start]) >= 0 {
		d.Start++
	}
	for d.End > d.Start && strings.IndexByte(edgePunct, text[d.End-1]) >= 0 {
		d.End--
	}
	if d.Text != "" {
		d.Text = text[d.Start:d.End]
	}
	return d
}

// filterCommonWords drops NER-derived single-word PERSON/ORGANIZATION detections whose
// lower-cased form is a common dictionary word ("Client", "Contact", "Billing": spaCy tags
// capitalised generic nouns). Pattern-recognizer hits, multi-word spans and anything not on
// the list ("Petrov", "Коля") are kept. Trade-off: a lone surname that is also a dictionary
// word ("Baker") is not masked unless the words file excludes it (the generator excludes
// common first names and the most frequent surnames).
func filterCommonWords(common map[string]struct{}, dets []presidio.Detection) []presidio.Detection {
	if len(common) == 0 {
		return dets
	}
	out := dets[:0:0]
	for _, d := range dets {
		if (d.EntityType == "PERSON" || d.EntityType == "ORGANIZATION") && d.IsNER() {
			w := strings.ToLower(strings.Trim(strings.TrimSpace(d.Text), edgePunct+"."))
			if w != "" && !strings.ContainsAny(w, " \t\n") {
				if _, ok := common[w]; ok {
					continue
				}
			}
		}
		out = append(out, d)
	}
	return out
}
