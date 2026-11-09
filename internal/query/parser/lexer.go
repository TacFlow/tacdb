// Package parser reads tacdb-cypher/1 text into an ast.Query (SPEC §15.1).
// It does not know the database: names, functions and types are checked
// later, by the semantic analysis.
package parser

import (
	"strings"
	"unicode/utf8"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
)

type kind uint8

const (
	tEOF kind = iota
	tIdent
	tQuoted // `name`
	tInt
	tFloat
	tString
	tParam
	tLParen
	tRParen
	tLBracket
	tRBracket
	tLBrace
	tRBrace
	tColon
	tComma
	tPipe
	tDot
	tDotDot
	tPlus
	tPlusEq
	tMinus
	tArrowRight // ->
	tArrowLeft  // <-
	tStar
	tSlash
	tPercent
	tCaret
	tEq
	tNeq
	tLt
	tLe
	tGt
	tGe
	tVecDist // <=>
	tFTS     // @@
	tRegex   // =~, outside the dialect
	tSemi
)

var kindText = map[kind]string{
	tEOF: "end of input", tIdent: "name", tQuoted: "name", tInt: "integer", tFloat: "float", tString: "string",
	tParam: "parameter", tLParen: "'('", tRParen: "')'", tLBracket: "'['", tRBracket: "']'", tLBrace: "'{'",
	tRBrace: "'}'", tColon: "':'", tComma: "','", tPipe: "'|'", tDot: "'.'", tDotDot: "'..'", tPlus: "'+'",
	tPlusEq: "'+='", tMinus: "'-'", tArrowRight: "'->'", tArrowLeft: "'<-'", tStar: "'*'", tSlash: "'/'",
	tPercent: "'%'", tCaret: "'^'", tEq: "'='", tNeq: "'<>'", tLt: "'<'", tLe: "'<='", tGt: "'>'", tGe: "'>='",
	tVecDist: "'<=>'", tFTS: "'@@'", tRegex: "'=~'", tSemi: "';'",
}

type token struct {
	kind kind
	text string // the decoded text of names, strings and parameters; the digits of numbers
	span ast.Span
}

// lexer turns the source into tokens. Errors are QUERY_LEX with the
// location of the offending text.
type lexer struct {
	src string
	pos int
}

func lexError(src string, span ast.Span, format string, args ...any) error {
	e := errs.Newf(errs.CodeQueryLex, "query.Parse", format, args...)
	e.Stage = errs.StageLex
	e.Location = locate(src, span)
	return e
}

// locate converts a span into line and column, both 1-based; the column
// counts code points.
func locate(src string, span ast.Span) *errs.Location {
	start := min(span.Start, len(src))
	line, col := 1, 1
	for _, r := range src[:start] {
		if r == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return &errs.Location{Start: span.Start, End: span.End, Line: line, Column: col}
}

// checkUTF8 rejects a source that is not valid UTF-8.
func checkUTF8(src string) error {
	if utf8.ValidString(src) {
		return nil
	}
	i := 0
	for i < len(src) {
		r, n := utf8.DecodeRuneInString(src[i:])
		if r == utf8.RuneError && n <= 1 {
			break
		}
		i += n
	}
	return lexError(src, ast.Span{Start: i, End: i + 1}, "the query is not valid UTF-8")
}

func isIdentStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentPart(c byte) bool  { return isIdentStart(c) || isDigit(c) }

func (l *lexer) peekByte(off int) byte {
	if l.pos+off < len(l.src) {
		return l.src[l.pos+off]
	}
	return 0
}

func (l *lexer) next() (token, error) {
	// Whitespace and // comments.
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			l.pos++
			continue
		}
		if c == '/' && l.peekByte(1) == '/' {
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
			continue
		}
		break
	}
	start := l.pos
	tok := func(k kind, n int) (token, error) {
		l.pos += n
		return token{kind: k, span: ast.Span{Start: start, End: l.pos}}, nil
	}
	if l.pos >= len(l.src) {
		return tok(tEOF, 0)
	}
	c := l.src[l.pos]
	switch {
	case isIdentStart(c):
		for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
			l.pos++
		}
		return token{kind: tIdent, text: l.src[start:l.pos], span: ast.Span{Start: start, End: l.pos}}, nil
	case isDigit(c):
		return l.number()
	}
	switch c {
	case '`':
		return l.quoted()
	case '\'', '"':
		return l.str(c)
	case '$':
		l.pos++
		for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
			l.pos++
		}
		if l.pos == start+1 {
			return token{}, lexError(l.src, ast.Span{Start: start, End: l.pos}, "a parameter needs a name after '$'")
		}
		return token{kind: tParam, text: l.src[start+1 : l.pos], span: ast.Span{Start: start, End: l.pos}}, nil
	case '(':
		return tok(tLParen, 1)
	case ')':
		return tok(tRParen, 1)
	case '[':
		return tok(tLBracket, 1)
	case ']':
		return tok(tRBracket, 1)
	case '{':
		return tok(tLBrace, 1)
	case '}':
		return tok(tRBrace, 1)
	case ':':
		return tok(tColon, 1)
	case ',':
		return tok(tComma, 1)
	case '|':
		return tok(tPipe, 1)
	case ';':
		return tok(tSemi, 1)
	case '.':
		if l.peekByte(1) == '.' {
			return tok(tDotDot, 2)
		}
		return tok(tDot, 1)
	case '+':
		if l.peekByte(1) == '=' {
			return tok(tPlusEq, 2)
		}
		return tok(tPlus, 1)
	case '-':
		if l.peekByte(1) == '>' {
			return tok(tArrowRight, 2)
		}
		return tok(tMinus, 1)
	case '*':
		return tok(tStar, 1)
	case '/':
		return tok(tSlash, 1)
	case '%':
		return tok(tPercent, 1)
	case '^':
		return tok(tCaret, 1)
	case '=':
		if l.peekByte(1) == '~' {
			return tok(tRegex, 2)
		}
		return tok(tEq, 1)
	case '!':
		if l.peekByte(1) == '=' {
			return tok(tNeq, 2)
		}
	case '<':
		switch {
		case l.peekByte(1) == '=' && l.peekByte(2) == '>':
			return tok(tVecDist, 3)
		case l.peekByte(1) == '=':
			return tok(tLe, 2)
		case l.peekByte(1) == '>':
			return tok(tNeq, 2)
		case l.peekByte(1) == '-':
			return tok(tArrowLeft, 2)
		}
		return tok(tLt, 1)
	case '>':
		if l.peekByte(1) == '=' {
			return tok(tGe, 2)
		}
		return tok(tGt, 1)
	case '@':
		if l.peekByte(1) == '@' {
			return tok(tFTS, 2)
		}
	}
	_, n := utf8.DecodeRuneInString(l.src[l.pos:])
	return token{}, lexError(l.src, ast.Span{Start: start, End: start + n}, "unexpected character %q", l.src[start:start+n])
}

// number reads an integer or a float. The digits are kept as text: the
// parser converts them, folding a unary minus into the literal.
func (l *lexer) number() (token, error) {
	start := l.pos
	for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
		l.pos++
	}
	k := tInt
	if l.peekByte(0) == '.' && isDigit(l.peekByte(1)) {
		k = tFloat
		l.pos++
		for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.pos++
		}
	}
	if c := l.peekByte(0); c == 'e' || c == 'E' {
		k = tFloat
		l.pos++
		if c := l.peekByte(0); c == '+' || c == '-' {
			l.pos++
		}
		digits := l.pos
		for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.pos++
		}
		if l.pos == digits {
			return token{}, lexError(l.src, ast.Span{Start: start, End: l.pos}, "the exponent of a float needs digits")
		}
	}
	if l.pos < len(l.src) && isIdentStart(l.src[l.pos]) {
		end := l.pos
		for end < len(l.src) && isIdentPart(l.src[end]) {
			end++
		}
		return token{}, lexError(l.src, ast.Span{Start: start, End: end}, "invalid number %q", l.src[start:end])
	}
	return token{kind: k, text: l.src[start:l.pos], span: ast.Span{Start: start, End: l.pos}}, nil
}

// quoted reads `name`, where “ is a backtick.
func (l *lexer) quoted() (token, error) {
	start := l.pos
	l.pos++
	var b strings.Builder
	for {
		if l.pos >= len(l.src) {
			return token{}, lexError(l.src, ast.Span{Start: start, End: l.pos}, "unterminated quoted name")
		}
		c := l.src[l.pos]
		if c == '`' {
			if l.peekByte(1) == '`' {
				b.WriteByte('`')
				l.pos += 2
				continue
			}
			l.pos++
			break
		}
		if c == 0 {
			return token{}, lexError(l.src, ast.Span{Start: l.pos, End: l.pos + 1}, "a name cannot hold a NUL byte")
		}
		b.WriteByte(c)
		l.pos++
	}
	if b.Len() == 0 {
		return token{}, lexError(l.src, ast.Span{Start: start, End: l.pos}, "empty quoted name")
	}
	return token{kind: tQuoted, text: b.String(), span: ast.Span{Start: start, End: l.pos}}, nil
}

func hexVal(c byte) (rune, bool) {
	switch {
	case isDigit(c):
		return rune(c - '0'), true
	case c >= 'a' && c <= 'f':
		return rune(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return rune(c-'A') + 10, true
	}
	return 0, false
}

// str reads a string between quote characters, decoding escapes.
func (l *lexer) str(quote byte) (token, error) {
	start := l.pos
	l.pos++
	var b strings.Builder
	for {
		if l.pos >= len(l.src) {
			return token{}, lexError(l.src, ast.Span{Start: start, End: l.pos}, "unterminated string")
		}
		c := l.src[l.pos]
		switch {
		case c == quote:
			l.pos++
			return token{kind: tString, text: b.String(), span: ast.Span{Start: start, End: l.pos}}, nil
		case c == '\n':
			return token{}, lexError(l.src, ast.Span{Start: start, End: l.pos}, "a string cannot hold a raw line break; use \\n")
		case c == '\\':
			esc := l.peekByte(1)
			at := l.pos
			l.pos += 2
			switch esc {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case '0':
				b.WriteByte(0)
			case 'u':
				var r rune
				for i := 0; i < 4; i++ {
					d, ok := hexVal(l.peekByte(0))
					if !ok {
						return token{}, lexError(l.src, ast.Span{Start: at, End: l.pos}, "\\u needs four hexadecimal digits")
					}
					r = r<<4 | d
					l.pos++
				}
				if !utf8.ValidRune(r) {
					return token{}, lexError(l.src, ast.Span{Start: at, End: l.pos}, "\\u%04X is not a valid character", r)
				}
				b.WriteRune(r)
			default:
				if at+1 >= len(l.src) {
					return token{}, lexError(l.src, ast.Span{Start: start, End: len(l.src)}, "unterminated string")
				}
				// \\, \', \" and any other character stand for themselves.
				_, n := utf8.DecodeRuneInString(l.src[at+1:])
				b.WriteString(l.src[at+1 : at+1+n])
				l.pos = at + 1 + n
			}
		default:
			b.WriteByte(c)
			l.pos++
		}
	}
}
