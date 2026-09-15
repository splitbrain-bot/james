package sqlq

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// tokenKind classifies a lexical token.
type tokenKind int

const (
	// tokEOF marks the end of input.
	tokEOF tokenKind = iota
	// tokIdent is an identifier, case preserved.
	tokIdent
	// tokKeyword is a reserved word; its text is the upper-case canonical form.
	tokKeyword
	// tokString is a single-quoted string literal; its text is the decoded value.
	tokString
	// tokNumber is a numeric literal; its text is the source spelling.
	tokNumber
	// tokComma is ",".
	tokComma
	// tokLParen is "(".
	tokLParen
	// tokRParen is ")".
	tokRParen
	// tokDot is ".".
	tokDot
	// tokStar is "*".
	tokStar
	// tokOp is a comparison operator: =, !=, <>, <, <=, >, >=.
	tokOp
)

// token is a single lexical token with its source position for error reporting.
type token struct {
	// kind is the token's class.
	kind tokenKind

	// text is the token's value: the identifier or decoded string, the numeric spelling,
	// the canonical keyword, or the operator/punctuation source.
	text string

	// pos is the byte offset of the token's start in the source.
	pos int
}

// keywords is the set of reserved words, mapped from their upper-case form to itself. A
// word matching one lexes as tokKeyword and cannot be used as an identifier.
var keywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "GROUP": true, "BY": true,
	"ORDER": true, "LIMIT": true, "OFFSET": true, "JOIN": true, "INNER": true,
	"LEFT": true, "RIGHT": true, "OUTER": true, "ON": true, "AS": true,
	"AND": true, "OR": true, "NOT": true, "IN": true, "LIKE": true,
	"IS": true, "NULL": true, "BETWEEN": true, "ASC": true, "DESC": true,
	"TRUE": true, "FALSE": true,
	"COUNT": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true,
	"DISTINCT": true, "HAVING": true, "UNION": true, "INTERSECT": true, "EXCEPT": true,
	"WITH": true, "CASE": true,
}

// unsupported maps a reserved word the grammar does not have to the message that
// tells the caller what to do instead. Reserving the words keeps them from passing as
// identifiers or aliases, so the parser can name the missing feature.
var unsupported = map[string]string{
	"DISTINCT":  "the grammar has no DISTINCT; use GROUP BY on the columns instead",
	"HAVING":    "the grammar has no HAVING; use WHERE, or run the grouped query and filter its rows in the answer",
	"UNION":     "the grammar has no UNION, INTERSECT or EXCEPT; run one query per part",
	"INTERSECT": "the grammar has no UNION, INTERSECT or EXCEPT; run one query per part",
	"EXCEPT":    "the grammar has no UNION, INTERSECT or EXCEPT; run one query per part",
	"WITH":      "the grammar has no WITH and no subqueries; run one query per step",
	"CASE":      "the grammar has no CASE and no computed expressions; select the plain columns and compute in the answer",
}

// errComputed is the error for arithmetic, which the grammar does not have.
var errComputed = errors.New("the grammar has no computed expressions; select the plain columns and compute in the answer")

// lex tokenizes src into the full token stream, terminated by a tokEOF token. It rejects
// any character outside the grammar — notably ";" (statement stacking) and the characters
// that begin SQL comments — so those are lexical errors rather than parseable input.
func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case isLetter(c):
			start := i
			for i < n && isIdentChar(src[i]) {
				i++
			}
			word := src[start:i]
			if up := strings.ToUpper(word); keywords[up] {
				toks = append(toks, token{kind: tokKeyword, text: up, pos: start})
			} else {
				toks = append(toks, token{kind: tokIdent, text: word, pos: start})
			}
		case isDigit(c):
			tok, next, err := lexNumber(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, tok)
			i = next
		case c == '+' || c == '-':
			// A sign only begins a number, and only when a digit or a dot follows.
			// Standing alone it is illegal, which makes "--" (and thus SQL comments) and
			// arithmetic lexical errors rather than tokens.
			if i+1 < n && (isDigit(src[i+1]) || src[i+1] == '.') {
				tok, next, err := lexNumber(src, i)
				if err != nil {
					return nil, err
				}
				toks = append(toks, tok)
				i = next
			} else if c == '-' && i+1 < n && src[i+1] == '-' {
				return nil, fmt.Errorf("comments are not allowed (position %d)", i)
			} else {
				return nil, errComputed
			}
		case c == '/' || c == '%':
			return nil, errComputed
		case c == '\'':
			tok, next, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, tok)
			i = next
		case c == ',':
			toks = append(toks, token{kind: tokComma, text: ",", pos: i})
			i++
		case c == '(':
			toks = append(toks, token{kind: tokLParen, text: "(", pos: i})
			i++
		case c == ')':
			toks = append(toks, token{kind: tokRParen, text: ")", pos: i})
			i++
		case c == '.':
			toks = append(toks, token{kind: tokDot, text: ".", pos: i})
			i++
		case c == '*':
			toks = append(toks, token{kind: tokStar, text: "*", pos: i})
			i++
		case c == '=':
			toks = append(toks, token{kind: tokOp, text: "=", pos: i})
			i++
		case c == '!':
			if i+1 < n && src[i+1] == '=' {
				toks = append(toks, token{kind: tokOp, text: "!=", pos: i})
				i += 2
			} else {
				return nil, fmt.Errorf("unexpected character %q at position %d", "!", i)
			}
		case c == '<':
			switch {
			case i+1 < n && src[i+1] == '=':
				toks = append(toks, token{kind: tokOp, text: "<=", pos: i})
				i += 2
			case i+1 < n && src[i+1] == '>':
				toks = append(toks, token{kind: tokOp, text: "<>", pos: i})
				i += 2
			default:
				toks = append(toks, token{kind: tokOp, text: "<", pos: i})
				i++
			}
		case c == '>':
			if i+1 < n && src[i+1] == '=' {
				toks = append(toks, token{kind: tokOp, text: ">=", pos: i})
				i += 2
			} else {
				toks = append(toks, token{kind: tokOp, text: ">", pos: i})
				i++
			}
		default:
			r, _ := utf8.DecodeRuneInString(src[i:])
			return nil, fmt.Errorf("unexpected character %q at position %d", r, i)
		}
	}
	toks = append(toks, token{kind: tokEOF, pos: n})
	return toks, nil
}

// lexNumber scans a numeric literal starting at i and returns its token, the index just
// past it, and any error. It accepts an optional leading sign, decimal digits with at most
// one point, and an optional exponent, then validates the spelling with strconv.
func lexNumber(src string, i int) (token, int, error) {
	start := i
	n := len(src)
	if i < n && (src[i] == '+' || src[i] == '-') {
		i++
	}
	digits := false
	for i < n && isDigit(src[i]) {
		i++
		digits = true
	}
	if i < n && src[i] == '.' {
		i++
		for i < n && isDigit(src[i]) {
			i++
			digits = true
		}
	}
	if !digits {
		return token{}, 0, fmt.Errorf("malformed number at position %d", start)
	}
	if i < n && (src[i] == 'e' || src[i] == 'E') {
		i++
		if i < n && (src[i] == '+' || src[i] == '-') {
			i++
		}
		expDigits := false
		for i < n && isDigit(src[i]) {
			i++
			expDigits = true
		}
		if !expDigits {
			return token{}, 0, fmt.Errorf("malformed number at position %d", start)
		}
	}
	// A number must not run directly into an identifier character (e.g. "1abc").
	if i < n && isIdentChar(src[i]) {
		return token{}, 0, fmt.Errorf("malformed number at position %d", start)
	}
	return token{kind: tokNumber, text: src[start:i], pos: start}, i, nil
}

// lexString scans a single-quoted string literal starting at the opening quote at i and
// returns its token with the decoded value, the index just past the closing quote, and any
// error. A doubled quote (”) is the escape for a literal quote; every other byte, a
// backslash included, is taken verbatim, since the value is bound as a parameter and never
// interpreted as SQL text.
func lexString(src string, i int) (token, int, error) {
	start := i
	i++ // opening quote
	n := len(src)
	var b strings.Builder
	for i < n {
		c := src[i]
		if c == '\'' {
			if i+1 < n && src[i+1] == '\'' {
				b.WriteByte('\'')
				i += 2
				continue
			}
			return token{kind: tokString, text: b.String(), pos: start}, i + 1, nil
		}
		b.WriteByte(c)
		i++
	}
	return token{}, 0, fmt.Errorf("unterminated string starting at position %d", start)
}

// parseNumber converts a numeric literal's spelling into its bound Go value: an int64 when
// it is an integer, otherwise a float64.
func parseNumber(text string) (any, error) {
	if !strings.ContainsAny(text, ".eE") {
		if v, err := strconv.ParseInt(text, 10, 64); err == nil {
			return v, nil
		}
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid number %q", text)
	}
	return v, nil
}

// isLetter reports whether c may begin an identifier.
func isLetter(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isDigit reports whether c is a decimal digit.
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isIdentChar reports whether c may continue an identifier.
func isIdentChar(c byte) bool { return isLetter(c) || isDigit(c) }
