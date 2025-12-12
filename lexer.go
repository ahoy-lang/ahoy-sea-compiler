package main

import (
	"fmt"
	"unicode"
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
	FOR
	RETURN
	BREAK
	CONTINUE
	SWITCH
	CASE
	DEFAULT
	SIZEOF
	
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

type Token struct {
	Type    TokenType
	Lexeme  string
	Line    int
	Column  int
}

type Lexer struct {
	source  string
	pos     int
	line    int
	column  int
	start   int
}

func NewLexer(source string) *Lexer {
	return &Lexer{
		source: source,
		pos:    0,
		line:   1,
		column: 1,
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
	"for":      FOR,
	"return":   RETURN,
	"break":    BREAK,
	"continue": CONTINUE,
	"switch":   SWITCH,
	"case":     CASE,
	"default":  DEFAULT,
	"sizeof":   SIZEOF,
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
		l.column = 1
	} else {
		l.column++
	}
	return ch
}

func (l *Lexer) skipWhitespace() {
	for {
		ch := l.current()
		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' {
			l.advance()
		} else if ch == '/' && l.peek(1) == '/' {
			// Single-line comment
			for l.current() != '\n' && l.current() != 0 {
				l.advance()
			}
		} else if ch == '/' && l.peek(1) == '*' {
			// Multi-line comment
			l.advance()
			l.advance()
			for {
				if l.current() == 0 {
					break
				}
				if l.current() == '*' && l.peek(1) == '/' {
					l.advance()
					l.advance()
					break
				}
				l.advance()
			}
		} else {
			break
		}
	}
}

func (l *Lexer) NextToken() Token {
	l.skipWhitespace()
	
	l.start = l.pos
	startLine := l.line
	startColumn := l.column
	
	ch := l.current()
	
	if ch == 0 {
		return Token{Type: EOF, Line: l.line, Column: l.column}
	}
	
	// Preprocessor directives
	if ch == '#' {
		start := l.pos
		for l.current() != '\n' && l.current() != 0 {
			l.advance()
		}
		directiveLine := l.source[start:l.pos]
		return Token{Type: HASH, Lexeme: directiveLine, Line: startLine, Column: startColumn}
	}
	
	// Identifiers and keywords
	if unicode.IsLetter(rune(ch)) || ch == '_' {
		start := l.pos
		for unicode.IsLetter(rune(l.current())) || unicode.IsDigit(rune(l.current())) || l.current() == '_' {
			l.advance()
		}
		lexeme := l.source[start:l.pos]
		
		if tokenType, ok := keywords[lexeme]; ok {
			return Token{Type: tokenType, Lexeme: lexeme, Line: startLine, Column: startColumn}
		}
		return Token{Type: IDENTIFIER, Lexeme: lexeme, Line: startLine, Column: startColumn}
	}
	
	// Numbers
	if unicode.IsDigit(rune(ch)) {
		start := l.pos
		for unicode.IsDigit(rune(l.current())) || l.current() == '.' || l.current() == 'x' || 
			l.current() == 'X' || (l.current() >= 'a' && l.current() <= 'f') || 
			(l.current() >= 'A' && l.current() <= 'F') {
			l.advance()
		}
		// Handle suffixes like L, U, UL, etc.
		for l.current() == 'L' || l.current() == 'U' || l.current() == 'l' || l.current() == 'u' {
			l.advance()
		}
		return Token{Type: NUMBER, Lexeme: l.source[start:l.pos], Line: startLine, Column: startColumn}
	}
	
	// Strings
	if ch == '"' {
		l.advance()
		start := l.pos
		for l.current() != '"' && l.current() != 0 {
			if l.current() == '\\' {
				l.advance()
			}
			l.advance()
		}
		lexeme := l.source[start:l.pos]
		l.advance() // closing "
		return Token{Type: STRING, Lexeme: lexeme, Line: startLine, Column: startColumn}
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
		return Token{Type: CHAR, Lexeme: lexeme, Line: startLine, Column: startColumn}
	}
	
	// Two-character operators
	l.advance()
	switch ch {
	case '+':
		if l.current() == '+' {
			l.advance()
			return Token{Type: INC, Lexeme: "++", Line: startLine, Column: startColumn}
		}
		if l.current() == '=' {
			l.advance()
			return Token{Type: PLUSASSIGN, Lexeme: "+=", Line: startLine, Column: startColumn}
		}
		return Token{Type: PLUS, Lexeme: "+", Line: startLine, Column: startColumn}
	case '-':
		if l.current() == '-' {
			l.advance()
			return Token{Type: DEC, Lexeme: "--", Line: startLine, Column: startColumn}
		}
		if l.current() == '>' {
			l.advance()
			return Token{Type: ARROW, Lexeme: "->", Line: startLine, Column: startColumn}
		}
		if l.current() == '=' {
			l.advance()
			return Token{Type: MINUSASSIGN, Lexeme: "-=", Line: startLine, Column: startColumn}
		}
		return Token{Type: MINUS, Lexeme: "-", Line: startLine, Column: startColumn}
	case '*':
		if l.current() == '=' {
			l.advance()
			return Token{Type: STARASSIGN, Lexeme: "*=", Line: startLine, Column: startColumn}
		}
		return Token{Type: STAR, Lexeme: "*", Line: startLine, Column: startColumn}
	case '/':
		if l.current() == '=' {
			l.advance()
			return Token{Type: SLASHASSIGN, Lexeme: "/=", Line: startLine, Column: startColumn}
		}
		return Token{Type: SLASH, Lexeme: "/", Line: startLine, Column: startColumn}
	case '%':
		if l.current() == '=' {
			l.advance()
			return Token{Type: PERCENTASSIGN, Lexeme: "%=", Line: startLine, Column: startColumn}
		}
		return Token{Type: PERCENT, Lexeme: "%", Line: startLine, Column: startColumn}
	case '=':
		if l.current() == '=' {
			l.advance()
			return Token{Type: EQ, Lexeme: "==", Line: startLine, Column: startColumn}
		}
		return Token{Type: ASSIGN, Lexeme: "=", Line: startLine, Column: startColumn}
	case '!':
		if l.current() == '=' {
			l.advance()
			return Token{Type: NE, Lexeme: "!=", Line: startLine, Column: startColumn}
		}
		return Token{Type: LNOT, Lexeme: "!", Line: startLine, Column: startColumn}
	case '<':
		if l.current() == '=' {
			l.advance()
			return Token{Type: LE, Lexeme: "<=", Line: startLine, Column: startColumn}
		}
		if l.current() == '<' {
			l.advance()
			if l.current() == '=' {
				l.advance()
				return Token{Type: LSHIFTASSIGN, Lexeme: "<<=", Line: startLine, Column: startColumn}
			}
			return Token{Type: LSHIFT, Lexeme: "<<", Line: startLine, Column: startColumn}
		}
		return Token{Type: LT, Lexeme: "<", Line: startLine, Column: startColumn}
	case '>':
		if l.current() == '=' {
			l.advance()
			return Token{Type: GE, Lexeme: ">=", Line: startLine, Column: startColumn}
		}
		if l.current() == '>' {
			l.advance()
			if l.current() == '=' {
				l.advance()
				return Token{Type: RSHIFTASSIGN, Lexeme: ">>=", Line: startLine, Column: startColumn}
			}
			return Token{Type: RSHIFT, Lexeme: ">>", Line: startLine, Column: startColumn}
		}
		return Token{Type: GT, Lexeme: ">", Line: startLine, Column: startColumn}
	case '&':
		if l.current() == '&' {
			l.advance()
			return Token{Type: LAND, Lexeme: "&&", Line: startLine, Column: startColumn}
		}
		if l.current() == '=' {
			l.advance()
			return Token{Type: BANDASSIGN, Lexeme: "&=", Line: startLine, Column: startColumn}
		}
		return Token{Type: BAND, Lexeme: "&", Line: startLine, Column: startColumn}
	case '|':
		if l.current() == '|' {
			l.advance()
			return Token{Type: LOR, Lexeme: "||", Line: startLine, Column: startColumn}
		}
		if l.current() == '=' {
			l.advance()
			return Token{Type: BORASSIGN, Lexeme: "|=", Line: startLine, Column: startColumn}
		}
		return Token{Type: BOR, Lexeme: "|", Line: startLine, Column: startColumn}
	case '^':
		if l.current() == '=' {
			l.advance()
			return Token{Type: BXORASSIGN, Lexeme: "^=", Line: startLine, Column: startColumn}
		}
		return Token{Type: BXOR, Lexeme: "^", Line: startLine, Column: startColumn}
	case '~':
		return Token{Type: BNOT, Lexeme: "~", Line: startLine, Column: startColumn}
	case '.':
		return Token{Type: DOT, Lexeme: ".", Line: startLine, Column: startColumn}
	case '?':
		return Token{Type: QUESTION, Lexeme: "?", Line: startLine, Column: startColumn}
	case '(':
		return Token{Type: LPAREN, Lexeme: "(", Line: startLine, Column: startColumn}
	case ')':
		return Token{Type: RPAREN, Lexeme: ")", Line: startLine, Column: startColumn}
	case '{':
		return Token{Type: LBRACE, Lexeme: "{", Line: startLine, Column: startColumn}
	case '}':
		return Token{Type: RBRACE, Lexeme: "}", Line: startLine, Column: startColumn}
	case '[':
		return Token{Type: LBRACKET, Lexeme: "[", Line: startLine, Column: startColumn}
	case ']':
		return Token{Type: RBRACKET, Lexeme: "]", Line: startLine, Column: startColumn}
	case ';':
		return Token{Type: SEMICOLON, Lexeme: ";", Line: startLine, Column: startColumn}
	case ',':
		return Token{Type: COMMA, Lexeme: ",", Line: startLine, Column: startColumn}
	case ':':
		return Token{Type: COLON, Lexeme: ":", Line: startLine, Column: startColumn}
	}
	
	return Token{Type: EOF, Lexeme: string(ch), Line: startLine, Column: startColumn}
}

func (l *Lexer) AllTokens() []Token {
	var tokens []Token
	for {
		tok := l.NextToken()
		tokens = append(tokens, tok)
		if tok.Type == EOF {
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
		IF: "IF", ELSE: "ELSE", WHILE: "WHILE", FOR: "FOR", RETURN: "RETURN",
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
