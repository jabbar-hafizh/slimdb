package db

import (
	"strings"
	"unicode"
)

// SplitSQL splits a script into statements on top-level semicolons. It skips
// semicolons inside quotes, quoted identifiers, dollar-quoted bodies and
// comments, and drops statements that hold only comments or whitespace.
func SplitSQL(script string) []string { return splitSQL(script, splitOpts{}) }

// splitOpts covers the quoting rules of databases other than Postgres.
type splitOpts struct {
	mysql    bool // backtick identifiers, backslash escapes in strings, # comments
	brackets bool // T-SQL [identifiers]
}

func splitSQL(script string, o splitOpts) []string {
	var out []string
	s := []rune(script)
	start := 0
	hasCode := false // the current statement has something besides comments

	flush := func(end int) {
		if hasCode {
			out = append(out, strings.TrimSpace(string(s[start:end])))
		}
		start, hasCode = end+1, false
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-' && at(s, i+1) == '-', o.mysql && c == '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && at(s, i+1) == '*':
			i = skipBlockComment(s, i)
		case c == '\'':
			escapes := o.mysql || i > 0 && (s[i-1] == 'E' || s[i-1] == 'e') && !isIdentRune(at(s, i-2))
			i = skipQuoted(s, i, '\'', escapes)
			hasCode = true
		case c == '"':
			i = skipQuoted(s, i, '"', o.mysql)
			hasCode = true
		case o.mysql && c == '`':
			i = skipQuoted(s, i, '`', false)
			hasCode = true
		case o.brackets && c == '[':
			i = skipQuoted(s, i, ']', false)
			hasCode = true
		case !o.mysql && !o.brackets && c == '$':
			if tag, ok := dollarTag(s, i); ok {
				i = skipDollarBody(s, i, tag)
			}
			hasCode = true
		case c == ';':
			flush(i)
		case !unicode.IsSpace(c):
			hasCode = true
		}
	}
	if start < len(s) {
		flush(len(s))
	}
	return out
}

func at(s []rune, i int) rune {
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

func isIdentRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// skipBlockComment returns the index of the closing '/'. Postgres block comments nest.
func skipBlockComment(s []rune, i int) int {
	depth := 0
	for ; i < len(s); i++ {
		switch {
		case s[i] == '/' && at(s, i+1) == '*':
			depth++
			i++
		case s[i] == '*' && at(s, i+1) == '/':
			depth--
			i++
			if depth == 0 {
				return i
			}
		}
	}
	return len(s)
}

// skipQuoted returns the index of the closing quote. A doubled quote is an
// escaped quote; with backslashEscapes (E'...' strings) so is \'.
func skipQuoted(s []rune, i int, q rune, backslashEscapes bool) int {
	for i++; i < len(s); i++ {
		switch {
		case backslashEscapes && s[i] == '\\':
			i++
		case s[i] == q && at(s, i+1) == q:
			i++
		case s[i] == q:
			return i
		}
	}
	return len(s)
}

// dollarTag reads a $tag$ opener at i. $1 style parameters are not openers.
func dollarTag(s []rune, i int) (string, bool) {
	j := i + 1
	for j < len(s) && isIdentRune(s[j]) {
		j++
	}
	if j >= len(s) || s[j] != '$' {
		return "", false
	}
	tag := string(s[i : j+1])
	if len(tag) > 2 && unicode.IsDigit([]rune(tag)[1]) {
		return "", false
	}
	return tag, true
}

// skipDollarBody returns the index of the last rune of the closing tag.
func skipDollarBody(s []rune, i int, tag string) int {
	n := len([]rune(tag))
	for j := i + n; j+n <= len(s); j++ {
		if string(s[j:j+n]) == tag {
			return j + n - 1
		}
	}
	return len(s)
}

// firstKeyword returns the first word of a statement in lower case, skipping
// leading comments and opening parentheses.
func firstKeyword(stmt string) string {
	s := []rune(stmt)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case unicode.IsSpace(c) || c == '(':
		case c == '-' && at(s, i+1) == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && at(s, i+1) == '*':
			i = skipBlockComment(s, i)
		default:
			j := i
			for j < len(s) && isIdentRune(s[j]) {
				j++
			}
			return strings.ToLower(string(s[i:j]))
		}
	}
	return ""
}
