package main

import (
	"fmt"
	"strings"
)

type TokenType int

const (
	EOF TokenType = iota
	IDENTIFIER
	NUMBER
	STRING
	CHAR
	
	// Keywords
	INT
	VOID
	CHAR_KW
	FLOAT
	DOUBLE
	UNSIGNED
	SIGNED
	LONG
	SHORT
	STRUCT
	UNION
	TYPEDEF
	ENUM
	CONST
	STATIC
	IF
	ELSE
	WHILE
	DO
	FOR
	RETURN
	BREAK
	CONTINUE
	SWITCH
	CASE
	DEFAULT
	SIZEOF
	GOTO
	
	// Operators
	PLUS
	MINUS
	STAR
	SLASH
	PERCENT
	ASSIGN
	PLUSASSIGN
	MINUSASSIGN
	STARASSIGN
	SLASHASSIGN
	PERCENTASSIGN
	LSHIFTASSIGN
	RSHIFTASSIGN
	BANDASSIGN
	BORASSIGN
	BXORASSIGN
	EQ
	NE
	LT
	LE
	GT
	GE
	LAND
	LOR
	LNOT
	BAND
	BOR
	BXOR
	BNOT
	LSHIFT
	RSHIFT
	INC
	DEC
	ARROW
	DOT
	QUESTION
	
	// Delimiters
	LPAREN
	RPAREN
	LBRACE
	RBRACE
	LBRACKET
	RBRACKET
	SEMICOLON
	COMMA
	COLON
	
	// Preprocessor
	INCLUDE
	DEFINE
	HASH
)

// Token is 24 bytes (down from 32): Column was write-only. Every byte
// counts at ~3M tokens per large TU (slice growth + GC scanning).
type Token struct {
	Type   TokenType
	Lexeme string
	Line   int
}

type Lexer struct {
	source string
	pos    int
	line   int
	start  int
}

func NewLexer(source string) *Lexer {
	return &Lexer{
		source: source,
		pos:    0,
		line:   1,
	}
}

var keywords = map[string]TokenType{
	"int":      INT,
	"void":     VOID,
	"char":     CHAR_KW,
	"float":    FLOAT,
	"double":   DOUBLE,
	"unsigned": UNSIGNED,
	"signed":   SIGNED,
	"long":     LONG,
	"short":    SHORT,
	"struct":   STRUCT,
	"union":    UNION,
	"typedef":  TYPEDEF,
	"enum":     ENUM,
	"const":    CONST,
	"static":   STATIC,
	"if":       IF,
	"else":     ELSE,
	"while":    WHILE,
	"do":       DO,
	"for":      FOR,
	"return":   RETURN,
	"break":    BREAK,
	"continue": CONTINUE,
	"switch":   SWITCH,
	"case":     CASE,
	"default":  DEFAULT,
	"sizeof":   SIZEOF,
	"goto":     GOTO,
}

func (l *Lexer) current() byte {
	if l.pos >= len(l.source) {
		return 0
	}
	return l.source[l.pos]
}

func (l *Lexer) peek(offset int) byte {
	pos := l.pos + offset
	if pos >= len(l.source) {
		return 0
	}
	return l.source[pos]
}

func (l *Lexer) advance() byte {
	if l.pos >= len(l.source) {
		return 0
	}
	ch := l.source[l.pos]
	l.pos++
	if ch == '\n' {
		l.line++
	}
	return ch
}

// skipWhitespace skips blanks and comments with direct indexing (one call
// per token; the old per-byte method calls cost ~40% of lex time).
func (l *Lexer) skipWhitespace() {
	src := l.source
	pos, line := l.pos, l.line
	n := len(src)
	for pos < n {
		c := src[pos]
		if c == ' ' || c == '\t' || c == '\r' {
			pos++
			continue
		}
		if c == '\n' {
			pos++
			line++
			continue
		}
		if c == '/' && pos+1 < n && src[pos+1] == '/' {
			for pos < n && src[pos] != '\n' {
				pos++
			}
			continue
		}
		if c == '/' && pos+1 < n && src[pos+1] == '*' {
			pos += 2
			for pos < n {
				if src[pos] == '\n' {
					pos++
					line++
					continue
				}
				if src[pos] == '*' && pos+1 < n && src[pos+1] == '/' {
					pos += 2
					break
				}
				pos++
			}
			continue
		}
		break
	}
	l.pos, l.line = pos, line
}

// isIdentChar reports identifier bytes (ASCII C; the old unicode package
// calls cost ~5% of lex time with zero benefit).
func isIdentChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func (l *Lexer) NextToken() Token {
	l.skipWhitespace()

	l.start = l.pos
	startLine := l.line

	ch := l.current()

	if ch == 0 {
		return Token{Type: EOF, Line: l.line}
	}
	
	// Preprocessor directives
	if ch == '#' {
		src := l.source
		start := l.pos
		pos := start + 1
		for pos < len(src) && src[pos] != '\n' {
			pos++
		}
		l.pos = pos
		directiveLine := src[start:pos]
		return Token{Type: HASH, Lexeme: directiveLine, Line: startLine}
	}
	
	// Identifiers and keywords (direct scan: no per-byte method calls,
	// no line/column branches — identifiers never span lines).
	if isIdentStart(ch) {
		src := l.source
		start := l.pos
		pos := start + 1
		for pos < len(src) && isIdentChar(src[pos]) {
			pos++
		}
		l.pos = pos
		lexeme := src[start:pos]
		
		if tokenType, ok := keywords[lexeme]; ok {
			return Token{Type: tokenType, Lexeme: lexeme, Line: startLine}
		}
		return Token{Type: IDENTIFIER, Lexeme: lexeme, Line: startLine}
	}
	
	// Numbers
	if ch >= '0' && ch <= '9' {
		src := l.source
		n := len(src)
		start := l.pos
		pos := start + 1
		for pos < n {
			c := src[pos]
			if (c >= '0' && c <= '9') || c == '.' || c == 'x' ||
				c == 'X' || (c >= 'a' && c <= 'f') ||
				(c >= 'A' && c <= 'F') {
				pos++
			} else {
				break
			}
		}
		l.pos = pos
		// Decimal exponent: e/E [+-]? digits (e.g. 1e-5). Not for hex
		// (0xE5's E is a digit) — and a bare trailing e/E stays as-is
		// only if no digits follow (garbage in, error out downstream).
		lex := l.source[start:l.pos]
		isHex := strings.HasPrefix(lex, "0x") || strings.HasPrefix(lex, "0X")
		if !isHex && len(lex) > 0 && (lex[len(lex)-1] == 'e' || lex[len(lex)-1] == 'E') {
			// Peek: optional sign then required digit.
			j := l.pos
			if j < len(l.source) && (l.source[j] == '+' || l.source[j] == '-') {
				j++
			}
			if j < len(l.source) && l.source[j] >= '0' && l.source[j] <= '9' {
				for j < len(l.source) && l.source[j] >= '0' && l.source[j] <= '9' {
					j++
				}
				l.pos = j
			}
		}
		// Handle suffixes like L, U, UL, F (float), etc.
		for l.pos < len(l.source) {
			c := l.source[l.pos]
			if c == 'L' || c == 'U' || c == 'l' || c == 'u' || c == 'F' || c == 'f' {
				l.pos++
			} else {
				break
			}
		}
		return Token{Type: NUMBER, Lexeme: l.source[start:l.pos], Line: startLine}
	}
	
	// Strings
	if ch == '"' {
		src := l.source
		n := len(src)
		start := l.pos + 1
		pos := start
		for pos < n && src[pos] != '"' {
			if src[pos] == '\\' {
				pos += 2
				continue
			}
			if src[pos] == '\n' {
				l.line++
			}
			pos++
		}
		if pos > n {
			pos = n // trailing backslash at EOF: match advance() clamping
		}
		lexeme := src[start:pos]
		if pos < n {
			pos++ // closing "
		}
		l.pos = pos
		return Token{Type: STRING, Lexeme: lexeme, Line: startLine}
	}
	
	// Character literals
	if ch == '\'' {
		l.advance()
		start := l.pos
		if l.current() == '\\' {
			l.advance()
		}
		l.advance()
		lexeme := l.source[start:l.pos]
		l.advance() // closing '
		return Token{Type: CHAR, Lexeme: lexeme, Line: startLine}
	}
	
	// Two-character operators
	l.advance()
	switch ch {
	case '+':
		if l.current() == '+' {
			l.advance()
			return Token{Type: INC, Lexeme: "++", Line: startLine}
		}
		if l.current() == '=' {
			l.advance()
			return Token{Type: PLUSASSIGN, Lexeme: "+=", Line: startLine}
		}
		return Token{Type: PLUS, Lexeme: "+", Line: startLine}
	case '-':
		if l.current() == '-' {
			l.advance()
			return Token{Type: DEC, Lexeme: "--", Line: startLine}
		}
		if l.current() == '>' {
			l.advance()
			return Token{Type: ARROW, Lexeme: "->", Line: startLine}
		}
		if l.current() == '=' {
			l.advance()
			return Token{Type: MINUSASSIGN, Lexeme: "-=", Line: startLine}
		}
		return Token{Type: MINUS, Lexeme: "-", Line: startLine}
	case '*':
		if l.current() == '=' {
			l.advance()
			return Token{Type: STARASSIGN, Lexeme: "*=", Line: startLine}
		}
		return Token{Type: STAR, Lexeme: "*", Line: startLine}
	case '/':
		if l.current() == '=' {
			l.advance()
			return Token{Type: SLASHASSIGN, Lexeme: "/=", Line: startLine}
		}
		return Token{Type: SLASH, Lexeme: "/", Line: startLine}
	case '%':
		if l.current() == '=' {
			l.advance()
			return Token{Type: PERCENTASSIGN, Lexeme: "%=", Line: startLine}
		}
		return Token{Type: PERCENT, Lexeme: "%", Line: startLine}
	case '=':
		if l.current() == '=' {
			l.advance()
			return Token{Type: EQ, Lexeme: "==", Line: startLine}
		}
		return Token{Type: ASSIGN, Lexeme: "=", Line: startLine}
	case '!':
		if l.current() == '=' {
			l.advance()
			return Token{Type: NE, Lexeme: "!=", Line: startLine}
		}
		return Token{Type: LNOT, Lexeme: "!", Line: startLine}
	case '<':
		if l.current() == '=' {
			l.advance()
			return Token{Type: LE, Lexeme: "<=", Line: startLine}
		}
		if l.current() == '<' {
			l.advance()
			if l.current() == '=' {
				l.advance()
				return Token{Type: LSHIFTASSIGN, Lexeme: "<<=", Line: startLine}
			}
			return Token{Type: LSHIFT, Lexeme: "<<", Line: startLine}
		}
		return Token{Type: LT, Lexeme: "<", Line: startLine}
	case '>':
		if l.current() == '=' {
			l.advance()
			return Token{Type: GE, Lexeme: ">=", Line: startLine}
		}
		if l.current() == '>' {
			l.advance()
			if l.current() == '=' {
				l.advance()
				return Token{Type: RSHIFTASSIGN, Lexeme: ">>=", Line: startLine}
			}
			return Token{Type: RSHIFT, Lexeme: ">>", Line: startLine}
		}
		return Token{Type: GT, Lexeme: ">", Line: startLine}
	case '&':
		if l.current() == '&' {
			l.advance()
			return Token{Type: LAND, Lexeme: "&&", Line: startLine}
		}
		if l.current() == '=' {
			l.advance()
			return Token{Type: BANDASSIGN, Lexeme: "&=", Line: startLine}
		}
		return Token{Type: BAND, Lexeme: "&", Line: startLine}
	case '|':
		if l.current() == '|' {
			l.advance()
			return Token{Type: LOR, Lexeme: "||", Line: startLine}
		}
		if l.current() == '=' {
			l.advance()
			return Token{Type: BORASSIGN, Lexeme: "|=", Line: startLine}
		}
		return Token{Type: BOR, Lexeme: "|", Line: startLine}
	case '^':
		if l.current() == '=' {
			l.advance()
			return Token{Type: BXORASSIGN, Lexeme: "^=", Line: startLine}
		}
		return Token{Type: BXOR, Lexeme: "^", Line: startLine}
	case '~':
		return Token{Type: BNOT, Lexeme: "~", Line: startLine}
	case '.':
		return Token{Type: DOT, Lexeme: ".", Line: startLine}
	case '?':
		return Token{Type: QUESTION, Lexeme: "?", Line: startLine}
	case '(':
		return Token{Type: LPAREN, Lexeme: "(", Line: startLine}
	case ')':
		return Token{Type: RPAREN, Lexeme: ")", Line: startLine}
	case '{':
		return Token{Type: LBRACE, Lexeme: "{", Line: startLine}
	case '}':
		return Token{Type: RBRACE, Lexeme: "}", Line: startLine}
	case '[':
		return Token{Type: LBRACKET, Lexeme: "[", Line: startLine}
	case ']':
		return Token{Type: RBRACKET, Lexeme: "]", Line: startLine}
	case ';':
		return Token{Type: SEMICOLON, Lexeme: ";", Line: startLine}
	case ',':
		return Token{Type: COMMA, Lexeme: ",", Line: startLine}
	case ':':
		return Token{Type: COLON, Lexeme: ":", Line: startLine}
	}
	
	return Token{Type: EOF, Lexeme: string(ch), Line: startLine}
}

func (l *Lexer) AllTokens() []Token {
	// Preallocate: dense generated code runs ~1 token per 3 bytes;
	// under-reserving regrew ~150MB of token copies on large TUs, while
	// Token is now 24 bytes so over-reserving is cheap (cap only).
	tokens := make([]Token, 0, len(l.source)/3)
	// Safety cap against a runaway lexer. Bell's generated TU is ~1.8M
	// tokens, so keep plenty of headroom (2M used to truncate the stream
	// deep inside a function, yielding a bogus "expected }" parse error
	// at a fake EOF).
	maxTokens := 32000000
	for {
		tok := l.NextToken()
		if tok.Type == EOF {
			if tok.Lexeme == "" {
				tokens = append(tokens, tok) // keep terminator: parser needs it
				break // real end of input
			}
			// Unknown byte (the old code ended the whole translation
			// unit here, silently dropping everything after). Skip it.
			l.advance()
			continue
		}
		tokens = append(tokens, tok)
		if len(tokens) > maxTokens {
			// Add EOF and break
			tokens = append(tokens, Token{Type: EOF, Lexeme: "", Line: l.line})
			break
		}
	}
	return tokens
}

func (t TokenType) String() string {
	names := map[TokenType]string{
		EOF: "EOF", IDENTIFIER: "IDENTIFIER", NUMBER: "NUMBER", STRING: "STRING", CHAR: "CHAR",
		INT: "INT", VOID: "VOID", CHAR_KW: "CHAR_KW", FLOAT: "FLOAT", DOUBLE: "DOUBLE",
		STRUCT: "STRUCT", TYPEDEF: "TYPEDEF", ENUM: "ENUM", CONST: "CONST", STATIC: "STATIC",
		IF: "IF", ELSE: "ELSE", WHILE: "WHILE", DO: "DO", FOR: "FOR", RETURN: "RETURN",
		BREAK: "BREAK", CONTINUE: "CONTINUE", SWITCH: "SWITCH", CASE: "CASE", DEFAULT: "DEFAULT",
		SIZEOF: "SIZEOF", PLUS: "PLUS", MINUS: "MINUS", STAR: "STAR", SLASH: "SLASH",
		PERCENT: "PERCENT", ASSIGN: "ASSIGN", EQ: "EQ", NE: "NE", LT: "LT", LE: "LE",
		GT: "GT", GE: "GE", LAND: "LAND", LOR: "LOR", LNOT: "LNOT", BAND: "BAND",
		BOR: "BOR", BXOR: "BXOR", BNOT: "BNOT", LSHIFT: "LSHIFT", RSHIFT: "RSHIFT",
		INC: "INC", DEC: "DEC", ARROW: "ARROW", DOT: "DOT", QUESTION: "QUESTION",
		LPAREN: "LPAREN", RPAREN: "RPAREN", LBRACE: "LBRACE", RBRACE: "RBRACE",
		LBRACKET: "LBRACKET", RBRACKET: "RBRACKET", SEMICOLON: "SEMICOLON", COMMA: "COMMA",
		COLON: "COLON", INCLUDE: "INCLUDE", DEFINE: "DEFINE", HASH: "HASH",
	}
	if name, ok := names[t]; ok {
		return name
	}
	return fmt.Sprintf("UNKNOWN(%d)", t)
}
