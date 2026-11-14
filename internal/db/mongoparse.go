package db

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// mongoStmt is one parsed mongosh-style statement:
//
//	show dbs | show collections | use <db>
//	db.<collection>.<method>(args...).<method>(args...)
//	db.getCollection("name").<method>(...)
//	db.getSiblingDB("db").<collection>.<method>(...)
//	db.<method>(args...)                  (database-level, e.g. runCommand)
type mongoStmt struct {
	text  string
	kind  string // "show", "use" or "call"
	arg   string // the word after show or use
	db    string // set by getSiblingDB; empty means the current database
	coll  string // empty for database-level calls
	calls []mongoCall
}

type mongoCall struct {
	name string
	args []any
}

type mongoParser struct {
	s []rune
	i int
}

func parseMongo(script string) ([]mongoStmt, error) {
	p := &mongoParser{s: []rune(script)}
	var out []mongoStmt
	for {
		p.skipSpaceAndSemicolons()
		if p.eof() {
			return out, nil
		}
		start := p.i
		st, err := p.statement()
		if err != nil {
			return nil, err
		}
		st.text = strings.TrimSpace(string(p.s[start:p.i]))
		out = append(out, st)
	}
}

func (p *mongoParser) statement() (mongoStmt, error) {
	word := p.ident()
	switch word {
	case "show", "use":
		p.skipInlineSpace()
		start := p.i
		for !p.eof() && !unicode.IsSpace(p.peek()) && p.peek() != ';' {
			p.i++
		}
		arg := string(p.s[start:p.i])
		if arg == "" {
			return mongoStmt{}, p.errorf("%s needs an argument", word)
		}
		return mongoStmt{kind: word, arg: arg}, nil
	case "db":
		return p.chain()
	}
	return mongoStmt{}, p.errorf("expected db.<collection>.<method>(...), show or use")
}

func (p *mongoParser) chain() (mongoStmt, error) {
	st := mongoStmt{kind: "call"}
	var path []string
	for {
		p.skipSpace()
		switch p.peek() {
		case '[':
			p.i++
			v, err := p.value()
			if err != nil {
				return st, err
			}
			name, ok := v.(string)
			if !ok {
				return st, p.errorf("collection name in [ ] must be a string")
			}
			if err := p.expect(']'); err != nil {
				return st, err
			}
			path = append(path, name)
			continue
		case '.':
			p.i++
		default:
			if len(st.calls) == 0 {
				return st, p.errorf("expected a method call such as .find()")
			}
			return st, nil
		}

		p.skipSpace()
		name := p.ident()
		if name == "" {
			return st, p.errorf("expected a name after '.'")
		}
		p.skipSpace()
		if p.peek() != '(' {
			if len(st.calls) > 0 {
				return st, p.errorf("expected a method call after %s()", st.calls[len(st.calls)-1].name)
			}
			path = append(path, name)
			continue
		}
		args, err := p.args()
		if err != nil {
			return st, err
		}

		// Navigation helpers that pick the collection or database.
		if len(st.calls) == 0 && len(path) == 0 && (name == "getCollection" || name == "getSiblingDB") {
			arg, ok := firstString(args)
			if !ok {
				return st, p.errorf("%s needs a string argument", name)
			}
			if name == "getCollection" {
				path = []string{arg}
			} else {
				st.db = arg
			}
			continue
		}
		if len(st.calls) == 0 {
			st.coll = strings.Join(path, ".")
		}
		st.calls = append(st.calls, mongoCall{name: name, args: args})
	}
}

func firstString(args []any) (string, bool) {
	if len(args) != 1 {
		return "", false
	}
	s, ok := args[0].(string)
	return s, ok
}

func (p *mongoParser) args() ([]any, error) {
	if err := p.expect('('); err != nil {
		return nil, err
	}
	args := []any{}
	for {
		p.skipSpace()
		if p.peek() == ')' {
			p.i++
			return args, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		args = append(args, v)
		p.skipSpace()
		if p.peek() == ',' {
			p.i++
		} else if p.peek() != ')' {
			return nil, p.errorf("expected ',' or ')'")
		}
	}
}

// value parses a JavaScript-style literal into a BSON value. Objects become
// bson.D so key order survives, which matters for sort and pipeline stages.
func (p *mongoParser) value() (any, error) {
	p.skipSpace()
	c := p.peek()
	switch {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"' || c == '\'' || c == '`':
		return p.str()
	case c == '/':
		return p.regex()
	case c == '-' || c == '+' || c == '.' || unicode.IsDigit(c):
		return p.number()
	case isJSIdentStart(c):
		return p.identValue()
	case c == 0:
		return nil, p.errorf("unexpected end of input")
	}
	return nil, p.errorf("unexpected %q", c)
}

func (p *mongoParser) object() (bson.D, error) {
	p.i++ // {
	doc := bson.D{}
	for {
		p.skipSpace()
		if p.peek() == '}' {
			p.i++
			return doc, nil
		}
		var key string
		switch c := p.peek(); {
		case c == '"' || c == '\'' || c == '`':
			s, err := p.str()
			if err != nil {
				return nil, err
			}
			key = s
		case isJSIdentStart(c) || unicode.IsDigit(c):
			key = p.ident()
		default:
			return nil, p.errorf("expected a key")
		}
		p.skipSpace()
		if err := p.expect(':'); err != nil {
			return nil, err
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		doc = append(doc, bson.E{Key: key, Value: v})
		p.skipSpace()
		if p.peek() == ',' {
			p.i++
		} else if p.peek() != '}' {
			return nil, p.errorf("expected ',' or '}'")
		}
	}
}

func (p *mongoParser) array() (bson.A, error) {
	p.i++ // [
	arr := bson.A{}
	for {
		p.skipSpace()
		if p.peek() == ']' {
			p.i++
			return arr, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
		p.skipSpace()
		if p.peek() == ',' {
			p.i++
		} else if p.peek() != ']' {
			return nil, p.errorf("expected ',' or ']'")
		}
	}
}

func (p *mongoParser) str() (string, error) {
	q := p.s[p.i]
	p.i++
	var sb strings.Builder
	for !p.eof() {
		c := p.s[p.i]
		p.i++
		switch {
		case c == q:
			return sb.String(), nil
		case c == '\\' && !p.eof():
			e := p.s[p.i]
			p.i++
			switch e {
			case 'n':
				sb.WriteRune('\n')
			case 't':
				sb.WriteRune('\t')
			case 'r':
				sb.WriteRune('\r')
			case 'u':
				if p.i+4 > len(p.s) {
					return "", p.errorf("bad \\u escape")
				}
				n, err := strconv.ParseUint(string(p.s[p.i:p.i+4]), 16, 32)
				if err != nil {
					return "", p.errorf("bad \\u escape")
				}
				sb.WriteRune(rune(n))
				p.i += 4
			default:
				sb.WriteRune(e)
			}
		default:
			sb.WriteRune(c)
		}
	}
	return "", p.errorf("unterminated string")
}

func (p *mongoParser) regex() (bson.Regex, error) {
	p.i++ // opening /
	var sb strings.Builder
	inClass := false
	for {
		if p.eof() {
			return bson.Regex{}, p.errorf("unterminated regular expression")
		}
		c := p.s[p.i]
		p.i++
		if c == '\\' && !p.eof() {
			sb.WriteRune(c)
			sb.WriteRune(p.s[p.i])
			p.i++
			continue
		}
		if c == '/' && !inClass {
			break
		}
		if c == '[' {
			inClass = true
		} else if c == ']' {
			inClass = false
		}
		sb.WriteRune(c)
	}
	start := p.i
	for !p.eof() && unicode.IsLetter(p.peek()) {
		p.i++
	}
	return bson.Regex{Pattern: sb.String(), Options: string(p.s[start:p.i])}, nil
}

func (p *mongoParser) number() (any, error) {
	start := p.i
	if c := p.peek(); c == '-' || c == '+' {
		p.i++
	}
	if p.ident() == "Infinity" {
		if p.s[start] == '-' {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	}
	p.i = start + 1
	for !p.eof() {
		c := p.peek()
		prev := p.s[p.i-1]
		if unicode.IsDigit(c) || c == '.' || c == 'e' || c == 'E' || ((c == '-' || c == '+') && (prev == 'e' || prev == 'E')) {
			p.i++
			continue
		}
		break
	}
	text := string(p.s[start:p.i])
	if !strings.ContainsAny(text, ".eE") {
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			if n >= math.MinInt32 && n <= math.MaxInt32 {
				return int32(n), nil
			}
			return n, nil
		}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, p.errorf("bad number %q", text)
	}
	return f, nil
}

// identValue handles keywords and the mongosh type helpers.
func (p *mongoParser) identValue() (any, error) {
	name := p.ident()
	if name == "new" {
		p.skipSpace()
		name = p.ident()
	}
	switch name {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null", "undefined":
		return nil, nil
	case "Infinity":
		return math.Inf(1), nil
	case "NaN":
		return math.NaN(), nil
	}

	p.skipSpace()
	if p.peek() != '(' {
		return nil, p.errorf("unknown identifier %q (quote strings, e.g. \"%s\")", name, name)
	}
	args, err := p.args()
	if err != nil {
		return nil, err
	}
	v, err := typeHelper(name, args)
	if err != nil {
		return nil, p.errorf("%s", err)
	}
	return v, nil
}

func typeHelper(name string, args []any) (any, error) {
	arg := func() (string, error) {
		if len(args) != 1 {
			return "", fmt.Errorf("%s takes one argument", name)
		}
		return fmt.Sprint(args[0]), nil
	}
	switch name {
	case "ObjectId", "ObjectID":
		if len(args) == 0 {
			return bson.NewObjectID(), nil
		}
		s, err := arg()
		if err != nil {
			return nil, err
		}
		return bson.ObjectIDFromHex(s)
	case "ISODate", "Date":
		if len(args) == 0 {
			return bson.NewDateTimeFromTime(time.Now()), nil
		}
		s, err := arg()
		if err != nil {
			return nil, err
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
			if t, err := time.Parse(layout, s); err == nil {
				return bson.NewDateTimeFromTime(t), nil
			}
		}
		return nil, fmt.Errorf("cannot read date %q, use e.g. 2026-01-31T10:00:00Z", s)
	case "NumberLong", "Long":
		s, err := arg()
		if err != nil {
			return nil, err
		}
		return strconv.ParseInt(s, 10, 64)
	case "NumberInt", "Int32":
		s, err := arg()
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(s, 10, 32)
		return int32(n), err
	case "Double":
		s, err := arg()
		if err != nil {
			return nil, err
		}
		return strconv.ParseFloat(s, 64)
	case "NumberDecimal", "Decimal128":
		s, err := arg()
		if err != nil {
			return nil, err
		}
		return bson.ParseDecimal128(s)
	case "UUID":
		s, err := arg()
		if err != nil {
			return nil, err
		}
		b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
		if err != nil || len(b) != 16 {
			return nil, fmt.Errorf("bad UUID %q", s)
		}
		return bson.Binary{Subtype: bson.TypeBinaryUUID, Data: b}, nil
	}
	return nil, fmt.Errorf("unknown function %s()", name)
}

// --- lexing helpers ---

func (p *mongoParser) eof() bool { return p.i >= len(p.s) }

func (p *mongoParser) peek() rune {
	if p.eof() {
		return 0
	}
	return p.s[p.i]
}

func (p *mongoParser) expect(c rune) error {
	p.skipSpace()
	if p.peek() != c {
		return p.errorf("expected %q", c)
	}
	p.i++
	return nil
}

func isJSIdentStart(c rune) bool { return c == '_' || c == '$' || unicode.IsLetter(c) }

func (p *mongoParser) ident() string {
	start := p.i
	for !p.eof() && (isJSIdentStart(p.peek()) || unicode.IsDigit(p.peek())) {
		p.i++
	}
	return string(p.s[start:p.i])
}

func (p *mongoParser) skipInlineSpace() {
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
		p.i++
	}
}

func (p *mongoParser) skipSpace() {
	for !p.eof() {
		switch {
		case unicode.IsSpace(p.peek()):
			p.i++
		case p.peek() == '/' && at(p.s, p.i+1) == '/':
			for !p.eof() && p.peek() != '\n' {
				p.i++
			}
		case p.peek() == '/' && at(p.s, p.i+1) == '*':
			p.i += 2
			for !p.eof() && !(p.peek() == '*' && at(p.s, p.i+1) == '/') {
				p.i++
			}
			p.i = min(p.i+2, len(p.s))
		default:
			return
		}
	}
}

func (p *mongoParser) skipSpaceAndSemicolons() {
	for {
		p.skipSpace()
		if p.peek() != ';' {
			return
		}
		p.i++
	}
}

func (p *mongoParser) errorf(format string, args ...any) error {
	line, col := 1, 1
	for _, c := range p.s[:min(p.i, len(p.s))] {
		if c == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return fmt.Errorf("line %d col %d: %s", line, col, fmt.Sprintf(format, args...))
}
