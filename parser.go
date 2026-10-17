package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// AST Node types
type NodeType int

const (
	NodeProgram NodeType = iota
	NodeFunction
	NodeVarDecl
	NodeReturn
	NodeIf
	NodeWhile
	NodeDoWhile
	NodeFor
	NodeBlock
	NodeExprStmt
	NodeBinaryOp
	NodeUnaryOp
	NodeCall
	NodeIdentifier
	NodeNumber
	NodeString
	NodeAssignment
	NodeArrayAccess
	NodeMemberAccess
	NodeCast
	NodeTernary
	NodeBreak
	NodeContinue
	NodeSwitch
	NodeCase
	NodeAddressOf
	NodeDereference
	NodeCompoundLiteral
	NodeOffsetof
	NodeComma
	NodeGoto
	NodeLabel
	NodeSizeof
)

type ASTNode struct {
	Type     NodeType
	Children []*ASTNode

	// Metadata
	Value    string
	DataType string
	IntValue int // For number literals

	// For function nodes
	Name       string
	Params     []string
	ParamTypes []string
	ReturnType string

	// For operators
	Operator string

	// For variables
	VarName  string
	IsGlobal bool
	Offset   int

	// For member access
	MemberName string // Name of the member being accessed
	IsPointer  bool   // true for -> operator, false for . operator (in member access context)

	// For indirect calls (e.g. cb.fn()): the function-valued expression.
	// Nil for direct calls, which use Name.
	Target *ASTNode

	// For loop slots (set by parseFor; explicit so omitted init/cond/incr
	// can't shift positions and be misclassified).
	ForInit *ASTNode
	ForCond *ASTNode
	ForIncr *ASTNode
	ForBody *ASTNode

	// For arrays and pointers
	ArraySize    int    // Size of array (0 if not an array)
	Dims         []int  // Per-dimension sizes for multi-dim arrays
	PointerLevel int    // Level of pointer indirection
	StructType   string // For struct variables, the struct name

	// For compound literals
	InitFields []string // Field names for designated initializers

	Line   int
	Column int
}

// StructMember represents a member of a struct
type StructMember struct {
	Name     string
	Type     string
	Offset   int
	Size     int
	ArrayLen int   // element count for array members (0 = not an array)
	Dims     []int // per-dimension sizes (multi-dim support)
	Align    int   // element alignment (arrays align like elements)
}

// StructDef represents a struct definition
type StructDef struct {
	Name    string
	Members []StructMember
	Size    int
	Align   int // natural alignment (max member alignment, capped at 8)
}

type Parser struct {
	compiler *Compiler
	tokens   []Token
	pos      int
	structs  map[string]*StructDef // Track struct definitions
	typedefs map[string]string     // Track typedef aliases: alias -> actual type
	enums    map[string]int        // Track enum constants: name -> value
	errors   []error               // Collect all parsing errors
}

func NewParser(source string) *Parser {
	lexer := NewLexer(source)
	tokens := lexer.AllTokens()

	// Initialize with common standard library typedefs
	typedefs := make(map[string]string)
	// stdint.h types
	typedefs["int8_t"] = "char"
	typedefs["int16_t"] = "short"
	typedefs["int32_t"] = "int"
	typedefs["int64_t"] = "long"
	typedefs["uint8_t"] = "unsigned char"
	typedefs["uint16_t"] = "unsigned short"
	typedefs["uint32_t"] = "unsigned int"
	typedefs["uint64_t"] = "unsigned long"
	typedefs["intptr_t"] = "long"
	typedefs["uintptr_t"] = "unsigned long"
	typedefs["size_t"] = "unsigned long"
	typedefs["ssize_t"] = "long"
	// signal.h types from skipped system headers (bell's crash handler
	// takes siginfo_t* and uses struct sigaction / sigset_t).
	typedefs["sig_atomic_t"] = "int"
	typedefs["siginfo_t"] = "struct siginfo_t"
	typedefs["sigset_t"] = "struct sigset_t"
	// stdbool.h
	typedefs["bool"] = "int"
	// Opaque libc types from skipped system headers (used only via
	// pointers; sizes below match glibc x86-64 for .comm allocation).
	typedefs["FILE"] = "struct FILE"
	typedefs["DIR"] = "struct DIR"
	typedefs["pthread_t"] = "unsigned long"
	typedefs["pthread_mutex_t"] = "struct __pthread_mutex"
	typedefs["pthread_cond_t"] = "struct __pthread_cond"
	typedefs["pthread_attr_t"] = "struct __pthread_attr"
	typedefs["pthread_key_t"] = "unsigned int"
	// SDL3 stdint aliases (SDL headers use angle includes the parser
	// cannot follow, so these never get declared; same widths as stdint).
	typedefs["Uint8"] = "unsigned char"
	typedefs["Uint16"] = "unsigned short"
	typedefs["Uint32"] = "unsigned int"
	typedefs["Uint64"] = "unsigned long"
	typedefs["Sint8"] = "signed char"
	typedefs["Sint16"] = "short"
	typedefs["Sint32"] = "int"
	typedefs["Sint64"] = "long"

	// Initialize with common standard library constants
	enums := make(map[string]int)
	// signal.h signals
	enums["SIGSEGV"] = 11 // Segmentation fault
	enums["SIGABRT"] = 6  // Abort signal
	enums["SIGFPE"] = 8   // Floating point exception
	enums["SIGILL"] = 4   // Illegal instruction
	enums["SIGINT"] = 2   // Interrupt
	enums["SIGTERM"] = 15 // Termination signal
	// dirent.h DT_* file types
	enums["DT_UNKNOWN"] = 0
	enums["DT_FIFO"] = 1
	enums["DT_CHR"] = 2
	enums["DT_DIR"] = 4
	enums["DT_BLK"] = 6
	enums["DT_REG"] = 8
	enums["DT_LNK"] = 10
	enums["DT_SOCK"] = 12
	enums["DT_WHT"] = 14

	p := &Parser{
		tokens:   tokens,
		pos:      0,
		structs:  make(map[string]*StructDef),
		typedefs: typedefs,
		enums:    enums,
		errors:   []error{},
	}

	// struct timespec + struct stat from skipped <sys/stat.h> (exact
	// glibc x86-64 layout; bell uses st.st_mtime/st_size via glue.h).
	p.structs["timespec"] = &StructDef{
		Name: "timespec",
		Members: []StructMember{
			{Name: "tv_sec", Type: "long", Offset: 0, Size: 8, Align: 8},
			{Name: "tv_nsec", Type: "long", Offset: 8, Size: 8, Align: 8},
		},
		Size: 16,
		Align: 8,
	}
	p.structs["stat"] = &StructDef{
		Name: "stat",
		Members: []StructMember{
			{Name: "st_dev", Type: "unsigned long", Offset: 0, Size: 8, Align: 8},
			{Name: "st_ino", Type: "unsigned long", Offset: 8, Size: 8, Align: 8},
			{Name: "st_nlink", Type: "unsigned long", Offset: 16, Size: 8, Align: 8},
			{Name: "st_mode", Type: "unsigned int", Offset: 24, Size: 4, Align: 4},
			{Name: "st_uid", Type: "unsigned int", Offset: 28, Size: 4, Align: 4},
			{Name: "st_gid", Type: "unsigned int", Offset: 32, Size: 4, Align: 4},
			{Name: "__pad0", Type: "int", Offset: 36, Size: 4, Align: 4},
			{Name: "st_rdev", Type: "unsigned long", Offset: 40, Size: 8, Align: 8},
			{Name: "st_size", Type: "long", Offset: 48, Size: 8, Align: 8},
			{Name: "st_blksize", Type: "long", Offset: 56, Size: 8, Align: 8},
			{Name: "st_blocks", Type: "long", Offset: 64, Size: 8, Align: 8},
			{Name: "st_atim", Type: "struct timespec", Offset: 72, Size: 16, Align: 8},
			{Name: "st_mtim", Type: "struct timespec", Offset: 88, Size: 16, Align: 8},
			{Name: "st_ctim", Type: "struct timespec", Offset: 104, Size: 16, Align: 8},
		},
		Size: 144,
		Align: 8,
	}
	// struct dirent from skipped <dirent.h> (exact glibc x86-64 layout;
	// bell reads entry->d_name via glue.h).
	p.structs["dirent"] = &StructDef{
		Name: "dirent",
		Members: []StructMember{
			{Name: "d_ino", Type: "unsigned long", Offset: 0, Size: 8, Align: 8},
			{Name: "d_off", Type: "long", Offset: 8, Size: 8, Align: 8},
			{Name: "d_reclen", Type: "unsigned short", Offset: 16, Size: 2, Align: 2},
			{Name: "d_type", Type: "unsigned char", Offset: 18, Size: 1, Align: 1},
			{Name: "d_name", Type: "char", Offset: 19, Size: 256, Align: 1, ArrayLen: 256, Dims: []int{256}},
		},
		Size: 280,
		Align: 8,
	}
	// struct sigaction / siginfo_t / sigset_t from skipped <signal.h>
	// (exact glibc x86-64 layout; bell's crash handler installs handlers
	// via sigaction and reads info->si_addr / info->si_code).
	p.structs["sigaction"] = &StructDef{
		Name: "sigaction",
		Members: []StructMember{
			{Name: "sa_handler", Type: "void*", Offset: 0, Size: 8, Align: 8},
			{Name: "sa_sigaction", Type: "void*", Offset: 0, Size: 8, Align: 8},
			{Name: "sa_mask", Type: "struct sigset_t", Offset: 8, Size: 128, Align: 8},
			{Name: "sa_flags", Type: "int", Offset: 136, Size: 4, Align: 4},
			{Name: "sa_restorer", Type: "void*", Offset: 144, Size: 8, Align: 8},
		},
		Size: 152,
		Align: 8,
	}
	p.structs["siginfo_t"] = &StructDef{
		Name: "siginfo_t",
		Members: []StructMember{
			{Name: "si_signo", Type: "int", Offset: 0, Size: 4, Align: 4},
			{Name: "si_errno", Type: "int", Offset: 4, Size: 4, Align: 4},
			{Name: "si_code", Type: "int", Offset: 8, Size: 4, Align: 4},
			{Name: "__pad0", Type: "int", Offset: 12, Size: 4, Align: 4},
			{Name: "si_addr", Type: "void*", Offset: 16, Size: 8, Align: 8},
		},
		Size: 128,
		Align: 8,
	}
	p.structs["sigset_t"] = &StructDef{
		Name: "sigset_t",
		Members: []StructMember{
			{Name: "__val", Type: "unsigned long", Offset: 0, Size: 128, Align: 8, ArrayLen: 16, Dims: []int{16}},
		},
		Size: 128,
		Align: 8,
	}
	return p
}

func (p *Parser) current() Token {
	if p.pos >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1] // EOF
	}
	return p.tokens[p.pos]
}

func (p *Parser) peek(offset int) Token {
	pos := p.pos + offset
	if pos >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1]
	}
	return p.tokens[pos]
}

func (p *Parser) advance() Token {
	tok := p.current()
	if p.pos < len(p.tokens)-1 {
		p.pos++
	}
	return tok
}

func (p *Parser) expect(typ TokenType) error {
	if p.current().Type != typ {
		return fmt.Errorf("expected %s, got %s at line %d", typ, p.current().Type, p.current().Line)
	}
	p.advance()
	return nil
}

func (p *Parser) match(types ...TokenType) bool {
	for _, t := range types {
		if p.current().Type == t {
			return true
		}
	}
	return false
}

func (p *Parser) isAtEnd() bool {
	return p.current().Type == EOF
}

// recordError adds an error to the error list
func (p *Parser) recordError(err error) {
	if err != nil {
		p.errors = append(p.errors, err)
	}
}

// synchronize attempts to recover from a parse error by advancing to the next safe point
func (p *Parser) synchronize() {
	// Advance past the error
	p.advance()

	// Skip tokens until we find a statement boundary
	for !p.isAtEnd() {
		// Stop at semicolons (end of statement)
		if p.peek(-1).Type == SEMICOLON {
			return
		}

		// Stop at statement keywords
		switch p.current().Type {
		case IF, WHILE, FOR, RETURN, INT, VOID, CHAR_KW, FLOAT, DOUBLE, STRUCT, TYPEDEF:
			return
		}

		p.advance()
	}
}

// resolveTypedef resolves a type through typedef aliases
func (p *Parser) resolveTypedef(typ string) string {
	if resolvedType, ok := p.typedefs[typ]; ok {
		return resolvedType
	}
	return typ
}

// isTypeName checks if the current token could be a type name (typedef)
func (p *Parser) isTypeName() bool {
	if !p.match(IDENTIFIER) {
		return false
	}
	_, isTypedef := p.typedefs[p.current().Lexeme]
	return isTypedef
}

// getTypeSize returns the size in bytes of a type
func (p *Parser) getTypeSize(typ string) int {
	// Remove const/static modifiers
	typ = stripQualifiers(typ)

	// Pointers are 8 bytes
	if len(typ) > 0 && typ[len(typ)-1] == '*' {
		return 8
	}

	// Check for struct types
	if len(typ) > 7 && typ[:7] == "struct " {
		structName := typ[7:]
		if structDef, ok := p.structs[structName]; ok {
			return structDef.Size
		}
		return 8 // Default struct size
	}

	// Check for union types
	if len(typ) > 6 && typ[:6] == "union " {
		unionName := typ[6:]
		if unionDef, ok := p.structs[unionName]; ok {
			return unionDef.Size
		}
		return 8 // Default union size
	}

	// Basic types (shared LP64 table; keeps parser and codegen in agreement)
	if s, ok := cSizeOf(typ); ok {
		return s
	}
	return 4 // Default
}

// getTypeAlign returns the natural alignment in bytes of a type. For struct
// types this is the struct's own alignment (max member alignment), NOT the
// size-based heuristic that previously over-aligned embedded structs (e.g.
// RenderTexture's Texture member was aligned to 8 instead of its true 4,
// producing a 56-byte layout instead of the real 44-byte one).
func (p *Parser) getTypeAlign(typ string) int {
	// Peel qualifiers / arrays / pointers.
	typ = stripQualifiers(typ)
	// Resolve typedef chains to the underlying type.
	for {
		resolved := p.resolveTypedef(typ)
		if resolved == typ {
			break
		}
		typ = resolved
	}
	// Pointers are 8 bytes, 8-aligned.
	if len(typ) > 0 && typ[len(typ)-1] == '*' {
		return 8
	}
	// Struct types: use the struct's stored natural alignment.
	if len(typ) > 7 && typ[:7] == "struct " {
		structName := typ[7:]
		if structDef, ok := p.structs[structName]; ok {
			a := structDef.Align
			if a <= 0 {
				a = structDef.Size
			}
			if a <= 0 {
				a = 1
			}
			if a > 8 {
				a = 8
			}
			return a
		}
		return 8 // Default struct alignment
	}
	// Union types.
	if len(typ) > 6 && typ[:6] == "union " {
		unionName := typ[6:]
		if unionDef, ok := p.structs[unionName]; ok {
			a := unionDef.Align
			if a <= 0 {
				a = unionDef.Size
			}
			if a <= 0 {
				a = 1
			}
			if a > 8 {
				a = 8
			}
			return a
		}
		return 8
	}
	// Basic types: infer alignment from size (capped at 8).
	if s, ok := cSizeOf(typ); ok {
		a := s
		if a > 8 {
			a = 8
		}
		if a <= 0 {
			a = 1
		}
		return a
	}
	return 4 // Default
}

func stripQualifiers(typ string) string {
	typ = trimPrefix(typ, "const ")
	typ = trimPrefix(typ, "static ")
	return typ
}

func trimPrefix(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

// Parse entire program
func (p *Parser) Parse() (*ASTNode, error) {
	program := &ASTNode{
		Type:     NodeProgram,
		Children: []*ASTNode{},
	}

	iterCount := 0
	maxIter := 10000000 // 10 million - much higher limit

	for p.current().Type != EOF {
		iterCount++
		if iterCount > maxIter {
			err := fmt.Errorf("parser appears to be stuck in infinite loop at position %d, token: %s (line %d)", p.pos, p.current().Lexeme, p.current().Line)
			p.recordError(err)
			break
		}

		// Skip preprocessor
		if p.match(HASH) {
			p.skipPreprocessor()
			continue
		}

		node, err := p.parseTopLevel()
		if err != nil {
			p.recordError(fmt.Errorf("line %d: %w", p.current().Line, err))
			// Try to recover and continue parsing
			p.synchronize()
			continue
		}

		if node != nil {
			program.Children = append(program.Children, node)
		}
	}

	// If we collected any errors, report them all
	if len(p.errors) > 0 {
		var errMsg strings.Builder
		errMsg.WriteString(fmt.Sprintf("encountered %d parsing error(s):\n", len(p.errors)))
		for i, err := range p.errors {
			errMsg.WriteString(fmt.Sprintf("  [%d] %v\n", i+1, err))
			if i >= 9 { // Limit to first 10 errors
				errMsg.WriteString(fmt.Sprintf("  ... and %d more errors\n", len(p.errors)-10))
				break
			}
		}
		return program, fmt.Errorf("%s", errMsg.String())
	}

	return program, nil
}

func (p *Parser) skipPreprocessor() {
	for p.current().Type != EOF && p.current().Line == p.peek(1).Line {
		p.advance()
	}
}

func (p *Parser) parseTopLevel() (*ASTNode, error) {
	// Skip preprocessor
	if p.match(HASH) {
		p.skipPreprocessor()
		return nil, nil
	}

	// Static assertions at file scope.
	if p.match(IDENTIFIER) && p.current().Lexeme == "_Static_assert" {
		return p.parseStaticAssert()
	}

	// Handle typedef
	if p.match(TYPEDEF) {
		p.advance()

		// typedef enum { ... } Name; — enums are 4 bytes, map alias to int.
		if p.match(ENUM) {
			alias, err := p.parseEnumDef()
			if err != nil {
				return nil, err
			}
			if alias != "" {
				p.typedefs[alias] = "int"
			}
			return nil, nil
		}

		// typedef struct { ... } Name; or typedef struct Name Name;
		if p.match(STRUCT, UNION) {
			kind := p.current().Lexeme // "struct" or "union"
			p.advance()

			var structName string
			var aliasName string

			// Check if struct has a name
			if p.match(IDENTIFIER) {
				structName = p.current().Lexeme
				p.advance()
			}

			// If there is a body, parse it with the shared body parser
			// (unions, anonymous struct/union members, bitfields,
			// function-pointer members).
			if p.match(LBRACE) {
				// Create temporary struct name if none given
				if structName == "" {
					structName = "__anon_typedef_" + fmt.Sprintf("%d", p.pos)
				}
				if berr := p.parseStructBody(kind, structName, false); berr != nil {
					return nil, berr
				}
			}

			// Get the typedef alias name
			if p.match(IDENTIFIER) {
				aliasName = p.current().Lexeme
				p.advance()
			}

			// Register the typedef
			if aliasName != "" {
				p.typedefs[aliasName] = kind + " " + structName
			}

			if p.match(SEMICOLON) {
				p.advance()
			}
			return nil, nil
		}

		// typedef existing_type new_name;
		existingType := p.parseType()
		if p.match(LPAREN) {
			// Function-pointer typedef: ret [(conv)] (*name)(params).
			// A calling-convention macro (SDLCALL) may sit where the
			// star goes. Unknown shapes skip the whole declaration
			// (always terminates: consumes to ; or EOF).
			isFnPtr := false
			var aliasName string
			// Tentative scan with rewind.
			save := p.pos
			p.advance() // (
			for p.match(IDENTIFIER) {
				p.advance()
			}
			if p.match(STAR) {
				p.advance()
				if p.match(IDENTIFIER) {
					aliasName = p.current().Lexeme
					p.advance()
					if p.match(RPAREN) {
						p.advance()
						// Skip the parameter list (balanced parens).
						if p.match(LPAREN) {
							depth := 0
							for !p.match(EOF) {
								if p.match(LPAREN) {
									depth++
								} else if p.match(RPAREN) {
									depth--
									if depth == 0 {
										p.advance()
										break
									}
								}
								p.advance()
							}
						}
						isFnPtr = true
					}
				}
			}
			if !isFnPtr {
				// Not the shape we handle: rewind and skip to ; .
				p.pos = save
				for !p.match(SEMICOLON) && !p.match(EOF) {
					p.advance()
				}
				if p.match(SEMICOLON) {
					p.advance()
				}
				return nil, nil
			}
			p.typedefs[aliasName] = "void*"
			if p.match(SEMICOLON) {
				p.advance()
			}
			return nil, nil
		}
		if p.match(IDENTIFIER) {
			aliasName := p.current().Lexeme
			p.advance()
			if p.match(LPAREN) {
				// Plain function typedef: `typedef RET name(params);`.
				// Skip the parameter list; functions only occur via
				// pointers, so model as void* (like fn-ptr typedefs).
				depth := 0
				for !p.match(EOF) {
					if p.match(LPAREN) {
						depth++
					} else if p.match(RPAREN) {
						depth--
						if depth == 0 {
							p.advance()
							break
						}
					}
					p.advance()
				}
				p.typedefs[aliasName] = "void*"
			} else {
				p.typedefs[aliasName] = existingType
			}
		}

		if p.match(SEMICOLON) {
			p.advance()
		}
		return nil, nil
	}

	// Struct/union DEFINITIONS only (`struct {` or `struct Name {`). Any
	// other `struct S ...` is a declaration using the type (variable,
	// function returning struct) and falls through to normal parsing.
	// Without this, `struct Big make_big(...)` was swallowed as a
	// definition and the whole file silently vanished.
	if p.match(STRUCT, UNION) {
		isDef := false
		if p.peek(1).Type == LBRACE {
			isDef = true
		} else if p.peek(1).Type == IDENTIFIER && p.peek(2).Type == LBRACE {
			isDef = true
		}
		if isDef {
			err := p.parseStructDef()
			if err != nil {
				return nil, err
			}
			return nil, nil
		}
	}

	if p.match(TYPEDEF) {
		// Handle typedef - need to parse the type and then the alias name
		p.advance() // skip 'typedef'

		// Check if it's a struct/union typedef
		if p.match(STRUCT) {
			// Parse the struct definition
			err := p.parseStructDef()
			if err != nil {
				return nil, err
			}
			// After struct def, there should be the typedef name
			if p.match(IDENTIFIER) {
				_ = p.current().Lexeme // aliasName
				p.advance()
				// Map the alias to the struct (last struct defined)
				// For now, just skip - proper typedef tracking would go here
			}
			if p.match(SEMICOLON) {
				p.advance()
			}
			return nil, nil
		} else {
			// Other typedef - skip for now
			p.skipStructOrTypedef()
			return nil, nil
		}
	}

	if p.match(ENUM) {
		_, err := p.parseEnumDef()
		if err != nil {
			return nil, err
		}
		return nil, nil
	}

	// Parse type
	dataType := p.parseType()

	// Get identifier
	if !p.match(IDENTIFIER) {
		p.advance()
		return nil, nil
	}

	name := p.current().Lexeme
	p.advance()

	// Function or variable?
	if p.match(LPAREN) {
		return p.parseFunction(name, dataType)
	} else {
		return p.parseGlobalVar(name, dataType)
	}
}

func (p *Parser) parseType() string {
	typ := ""

	// Storage class (static/const/inline/extern — accepted, not optimized;
	// inline/extern only affect linkage, which we don't do).
	for {
		if p.match(STATIC, CONST) {
			typ += p.current().Lexeme + " "
			p.advance()
		} else if p.match(IDENTIFIER) && (p.current().Lexeme == "inline" || p.current().Lexeme == "extern" ||
			p.current().Lexeme == "volatile" ||
			p.current().Lexeme == "_Thread_local" || p.current().Lexeme == "__thread") {
			// _Thread_local/__thread accepted but not implemented
			// (single-threaded codegen; bell/stb only use it for
			// failure strings). volatile is accepted and ignored
			// (non-optimizing compiler: no reordering happens anyway).
			typ += p.current().Lexeme + " "
			p.advance()
		} else {
			break
		}
	}

	// Type modifiers (unsigned, signed, long, short)
	for p.match(UNSIGNED, SIGNED, LONG, SHORT) {
		typ += p.current().Lexeme + " "
		p.advance()
	}

	// Base type (optional after modifiers - defaults to int)
	if p.match(INT, VOID, CHAR_KW, FLOAT, DOUBLE) {
		typ += p.current().Lexeme
		p.advance()
	} else if p.match(STRUCT, UNION) {
		structOrUnion := p.current().Lexeme // "struct" or "union"
		p.advance()
		if p.match(IDENTIFIER) {
			typ = structOrUnion + " " + p.current().Lexeme
			p.advance()
		} else if p.match(LBRACE) {
			// Anonymous struct/union definition: parse with the shared
			// full body parser (correct sizes, multi-declarators, nested
			// anonymous members, unions).
			anonName := fmt.Sprintf("__anon_%s_%d", structOrUnion, p.pos)
			typ = structOrUnion + " " + anonName
			savePos := p.pos
			if berr := p.parseStructBody(structOrUnion, anonName, false); berr != nil {
				// Lenient fallback (old behavior skipped junk): rewind,
				// skip the balanced definition, register an empty struct.
				p.pos = savePos
				depth := 0
				for !p.match(EOF) {
					if p.match(LBRACE) {
						depth++
					}
					if p.match(RBRACE) {
						depth--
						if depth == 0 {
							p.advance()
							break
						}
					}
					p.advance()
				}
				p.structs[anonName] = &StructDef{Name: anonName}
			}
		}
	} else if p.match(IDENTIFIER) {
		// Check if this could be a typedef name
		typeName := p.current().Lexeme
		resolvedType := p.resolveTypedef(typeName)

		// If it resolves to something different, it's a typedef
		if resolvedType != typeName {
			p.advance()
			typ += resolvedType
		} else if typ == "" {
			// No modifiers yet - treat as type name (typedef or unknown type)
			p.advance()
			typ = resolvedType
		}
		// Note: If we have modifiers (typ != "") but this identifier is not a typedef,
		// don't consume it - it's likely the variable name, not a type
	}

	// Pointers, allowing interleaved qualifiers (char * const *p).
	// Qualifiers don't affect layout, so only stars are recorded.
	for {
		if p.match(STAR) {
			typ += "*"
			p.advance()
		} else if p.match(CONST) {
			p.advance()
		} else if p.match(IDENTIFIER) && p.current().Lexeme == "volatile" {
			// Interleaved/trailing volatile (e.g. `const char* volatile p`)
			// doesn't affect layout; consume it.
			p.advance()
		} else {
			break
		}
	}

	// If we have modifiers but no base type, default to int
	// (e.g., "long" means "long int", "unsigned" means "unsigned int")
	// Check if typ ends with a space (indicating modifier without base type)
	if len(typ) > 0 && typ[len(typ)-1] == ' ' {
		typ += "int"
	}

	// If typ is still empty, return int as default
	if typ == "" {
		typ = "int"
	}

	return typ
}

// parseFnPtrMember parses a function-pointer struct member after the return
// type: `(*name)(params)`. The current token must be '('. Records it as an
// 8-byte pointer member. Fast path for a very common C idiom.
func (p *Parser) parseFnPtrMember(retType string, offset *int) (StructMember, error) {
	p.advance() // skip (
	if !p.match(STAR) {
		// Not a function pointer member; skip to ; or }
		for !p.match(SEMICOLON, RBRACE, EOF) {
			p.advance()
		}
		return StructMember{}, fmt.Errorf("skip")
	}
	p.advance()
	if !p.match(IDENTIFIER) {
		for !p.match(SEMICOLON, RBRACE, EOF) {
			p.advance()
		}
		return StructMember{}, fmt.Errorf("skip")
	}
	memberName := p.current().Lexeme
	p.advance()
	if !p.match(RPAREN) {
		return StructMember{}, fmt.Errorf("expected ) in function pointer member")
	}
	p.advance()
	// Skip the parameter list (balanced parens, may contain nested parens).
	if !p.match(LPAREN) {
		return StructMember{}, fmt.Errorf("expected (params) in function pointer member")
	}
	depth := 0
	for !p.match(EOF) {
		if p.match(LPAREN) {
			depth++
		} else if p.match(RPAREN) {
			depth--
			if depth == 0 {
				p.advance()
				break
			}
		}
		p.advance()
	}
	memberSize := 8
	alignment := 8
	if *offset%alignment != 0 {
		*offset += alignment - (*offset % alignment)
	}
	m := StructMember{
		Name:   memberName,
		Type:   retType + "*",
		Offset: *offset,
		Size:   memberSize,
	}
	*offset += memberSize
	return m, nil
}

func (p *Parser) parseStructDef() error {
	kind := p.current().Lexeme // "struct" or "union"
	p.advance()                // skip it

	// Get struct name (optional for anonymous structs in typedefs)
	var structName string
	if p.match(IDENTIFIER) {
		structName = p.current().Lexeme
		p.advance()
	}

	// Check for just declaration (struct Foo;) or definition
	if p.match(SEMICOLON) {
		p.advance()
		return nil // Forward declaration, ignore
	}

	if !p.match(LBRACE) {
		// It's a variable declaration using the struct, skip for now
		p.skipStructOrTypedef()
		return nil
	}

	// Anonymous struct - generate a name
	if structName == "" {
		structName = "__anon_struct_" + fmt.Sprintf("%d", p.pos)
	}

	return p.parseStructBody(kind, structName, true)
}

// parseStructBody parses `{ members }` (current token is `{`) and registers
// the definition. Shared by named definitions and nested anonymous ones.
// Unions get all-zero offsets; anonymous struct/union members (no declarator)
// inline their members into the parent (C11 promotion). topLevel controls
// the trailing `varname? ;` (a top-level definition owns it; a member type
// must leave the `;` for the anonymous-member check).
func (p *Parser) parseStructBody(kind string, structName string, topLevel bool) error {
	p.advance() // skip {

	members := []StructMember{}
	currentOffset := 0
	maxMemberSize := 0

	// Bitfield storage-unit tracking: sequential bitfields of the same
	// declared type pack into one storage unit (the unit's full size is
	// reserved so following members keep C layout).
	bitUnitOffset := 0
	bitUnitSize := 0
	bitUsedBits := 0

	// Parse members
	for !p.match(RBRACE) && !p.match(EOF) {
		// Parse member type
		memberType := p.parseType()

		// Anonymous member (no declarator): `struct {...};` — inline.
		if p.match(SEMICOLON) {
			subName := memberType
			for _, q := range []string{"struct ", "union ", "const ", "volatile "} {
				subName = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(subName), q))
			}
			if sub, ok := p.structs[subName]; ok {
				subAlign := 1
				for _, m := range sub.Members {
					a := m.Align
					if a <= 0 {
						a = m.Size
					}
					if a > 8 {
						a = 8
					}
					if a > subAlign {
						subAlign = a
					}
				}
				if currentOffset%subAlign != 0 {
					currentOffset += subAlign - (currentOffset % subAlign)
				}
				for _, m := range sub.Members {
					if m.Name == "" {
						continue
					}
					members = append(members, StructMember{
						Name:     m.Name,
						Type:     m.Type,
						Offset:   currentOffset + m.Offset,
						Size:     m.Size,
						ArrayLen: m.ArrayLen,
						Dims:     m.Dims,
						Align:    m.Align,
					})
				}
				currentOffset += sub.Size
				if sub.Size > maxMemberSize {
					maxMemberSize = sub.Size
				}
			}
			bitUnitSize, bitUsedBits = 0, 0
			p.advance() // skip ;
			continue
		}

		// Function pointer member: ret (*name)(params);
		if p.match(LPAREN) {
			m, ferr := p.parseFnPtrMember(memberType, &currentOffset)
			if ferr != nil {
				// Skip to ; or }
				for !p.match(SEMICOLON, RBRACE, EOF) {
					p.advance()
				}
				if p.match(SEMICOLON) {
					p.advance()
				}
				continue
			}
			members = append(members, m)
			bitUnitSize, bitUsedBits = 0, 0
			if p.match(SEMICOLON) {
				p.advance()
			} else if !p.match(RBRACE) && !p.match(EOF) {
				return fmt.Errorf("expected ; after struct member")
			}
			continue
		}

		// Parse member name(s) - can have multiple per line.
		// Each declarator may add its own stars: int *a, b;
		baseMemberTypeB := memberType
		for {
			memberType = baseMemberTypeB
			for p.match(STAR) {
				memberType += "*"
				p.advance()
			}
			if !p.match(IDENTIFIER) {
				// Unparseable declarator: drop the rest of this member
				// (to ;), the enclosing body (}) or EOF, then leave the
				// declarator loop. Continuing here re-matched the same
				// stop token forever when it was } or EOF (infinite loop
				// in bell's box3d headers). The member-end check below
				// consumes a remaining ; and tolerates } / EOF.
				for !p.match(SEMICOLON, RBRACE, EOF) {
					p.advance()
				}
				break
			}

			memberName := p.current().Lexeme
			p.advance()

			// Bitfield member (e.g. `uint32_t axis : 2;`): reserve the
			// declared type's storage unit so following members keep C
			// layout. Bitfield access itself is not supported; such
			// members get Size 0 and live at their unit's offset.
			if p.match(COLON) {
				p.advance()
				width := 0
				if !p.match(SEMICOLON, RBRACE, EOF, COMMA) {
					wexpr, werr := p.parseExpression()
					if werr == nil {
						if folded, ok := p.evalConstInt(wexpr); ok {
							width = folded
						}
					}
				}
				declSize := p.getTypeSize(memberType)
				if declSize <= 0 {
					declSize = 4
				}
				mAlignBF := declSize
				if mAlignBF > 8 {
					mAlignBF = 8
				}
				if width < 0 || width > declSize*8 {
					width = declSize * 8
				}
				if bitUnitSize != declSize || bitUsedBits+width > declSize*8 {
					// Start a new storage unit at the next aligned offset.
					if currentOffset%mAlignBF != 0 {
						currentOffset += mAlignBF - (currentOffset % mAlignBF)
					}
					bitUnitOffset = currentOffset
					currentOffset += declSize
					bitUnitSize = declSize
					bitUsedBits = width
					if declSize > maxMemberSize {
						maxMemberSize = declSize
					}
				} else {
					bitUsedBits += width
				}
				members = append(members, StructMember{
					Name:   memberName,
					Type:   memberType,
					Offset: bitUnitOffset,
					Size:   0, // storage is reserved by the bitfield unit
					Align:  mAlignBF,
				})
				if p.match(COMMA) {
					p.advance()
					continue
				}
				break
			}

			// Calculate actual member size based on type
			memberSize := p.getTypeSize(memberType)
			elemAlignB := p.getTypeAlign(memberType)

			// Handle arrays (multi-dim supported): int arr[10][20];
			var memberDims []int
			for p.match(LBRACKET) {
				p.advance()
				dim := 0
				if !p.match(RBRACKET) {
					dimExpr, derr := p.parseExpression()
					if derr != nil {
						return derr
					}
					if folded, ok := p.evalConstInt(dimExpr); ok {
						dim = folded
					}
				}
				if !p.match(RBRACKET) {
					return fmt.Errorf("expected ]")
				}
				p.advance()
				if dim <= 0 {
					return fmt.Errorf("array size must be a positive constant")
				}
				memberDims = append(memberDims, dim)
				memberSize = dim * memberSize
			}

			arrayLen := 0
			for _, d := range memberDims {
				if arrayLen == 0 {
					arrayLen = d
				} else {
					arrayLen *= d
				}
			}
			// Align by ELEMENT size (arrays align like their elements).
			alignmentB := elemAlignB
			if alignmentB > 8 {
				alignmentB = 8 // Max alignment is 8 bytes
			}
			if alignmentB > 0 && currentOffset%alignmentB != 0 {
				currentOffset += alignmentB - (currentOffset % alignmentB)
			}
			mAlignB := elemAlignB
			if mAlignB <= 0 {
				mAlignB = 1
			}
			if mAlignB > 8 {
				mAlignB = 8
			}
			members = append(members, StructMember{
				Name:     memberName,
				Type:     memberType,
				Offset:   currentOffset,
				Size:     memberSize,
				ArrayLen: arrayLen,
				Dims:     memberDims,
				Align:    mAlignB,
			})

			currentOffset += memberSize
			bitUnitSize, bitUsedBits = 0, 0
			if memberSize > maxMemberSize {
				maxMemberSize = memberSize
			}

			if p.match(COMMA) {
				p.advance()
				continue
			}
			break
		}

		if p.match(SEMICOLON) {
			p.advance()
		} else if !p.match(RBRACE) && !p.match(EOF) {
			return fmt.Errorf("expected ; after struct member")
		}
	}

	if !p.match(RBRACE) {
		return fmt.Errorf("expected } at end of struct")
	}
	p.advance()

	// Optional variable name and semicolon (top-level definitions only;
	// member types must not consume the declarator or its semicolon).
	if topLevel {
		if p.match(IDENTIFIER) {
			p.advance()
		}
		if p.match(SEMICOLON) {
			p.advance()
		}
	}

	// Unions overlap: every member lives at offset 0 and the size is
	// the largest member (anonymous members contribute via maxMemberSize).
	if kind == "union" {
		for i := range members {
			members[i].Offset = 0
		}
		currentOffset = maxMemberSize
	}

	// Calculate struct's alignment (max of all member alignments, capped at 8)
	structAlignment := 1
	for _, member := range members {
		memberAlign := member.Align
		if memberAlign <= 0 {
			memberAlign = member.Size
		}
		if memberAlign > 8 {
			memberAlign = 8
		}
		if memberAlign > structAlignment {
			structAlignment = memberAlign
		}
	}

	// Add padding at end to make struct size a multiple of its alignment
	if currentOffset%structAlignment != 0 {
		currentOffset += structAlignment - (currentOffset % structAlignment)
	}

	// Store struct definition
	p.structs[structName] = &StructDef{
		Name:    structName,
		Members: members,
		Size:    currentOffset,
		Align:   structAlignment,
	}

	return nil
}

func (p *Parser) parseEnumDef() (string, error) {
	p.advance() // skip 'enum'

	// Optional enum name
	if p.match(IDENTIFIER) {
		_ = p.current().Lexeme // enumName not used yet
		p.advance()
	}

	// Check for just declaration (enum Foo;)
	if p.match(SEMICOLON) {
		p.advance()
		return "", nil // Forward declaration, ignore
	}

	if !p.match(LBRACE) {
		return "", fmt.Errorf("expected { or ; after enum name")
	}
	p.advance() // skip {

	// Parse enum values
	currentValue := 0
	for !p.match(RBRACE) && !p.match(EOF) {
		if !p.match(IDENTIFIER) {
			p.advance()
			continue
		}

		constName := p.current().Lexeme
		p.advance()

		// Check for explicit value
		if p.match(ASSIGN) {
			p.advance()

			// Parse the value as a full constant expression: macro calls
			// (SDL_DEFINE_PIXELFORMAT style), hex literals, shifts/ors,
			// and references to earlier enumerators all fold through
			// evalConstInt. An unfolderable expression keeps the
			// auto-increment value (previous behavior).
			expr, err := p.parseAssignment()
			if err == nil {
				if v, ok := p.evalConstInt(expr); ok {
					currentValue = v
				}
			} else {
				// Skip a malformed expression to the next separator so
				// the rest of the enum still parses.
				for !p.match(COMMA, RBRACE, EOF) {
					p.advance()
				}
			}
		}

		// Store enum constant
		p.enums[constName] = currentValue
		currentValue++

		// Optional comma
		if p.match(COMMA) {
			p.advance()
		}
	}

	if !p.match(RBRACE) {
		return "", fmt.Errorf("expected } at end of enum")
	}
	p.advance()

	// Optional trailing name (typedef alias or variable) and semicolon.
	// Returned so typedef sites can map the alias to int (enums are 4 bytes).
	trailing := ""
	if p.match(IDENTIFIER) {
		trailing = p.current().Lexeme
		p.advance()
	}
	if p.match(SEMICOLON) {
		p.advance()
	}

	return trailing, nil
}

func (p *Parser) skipStructOrTypedef() {
	for !p.match(SEMICOLON, EOF) {
		if p.match(LBRACE) {
			depth := 1
			p.advance()
			for depth > 0 && p.current().Type != EOF {
				if p.match(LBRACE) {
					depth++
				} else if p.match(RBRACE) {
					depth--
				}
				p.advance()
			}
		} else {
			p.advance()
		}
	}
	if p.match(SEMICOLON) {
		p.advance()
	}
}

func (p *Parser) parseFunction(name string, returnType string) (*ASTNode, error) {
	if os.Getenv("SEAPARSE") != "" {
		fmt.Fprintf(os.Stderr, "FUNCDEF %q ret=%q line=%d\n", name, returnType, p.current().Line)
	}
	p.advance() // skip (

	params := []string{}
	paramTypes := []string{}

	for !p.match(RPAREN) && !p.match(EOF) {
		if p.match(VOID) && p.peek(1).Type == RPAREN {
			p.advance()
			break
		}

		paramType := p.parseType()
		paramTypes = append(paramTypes, paramType)

		if p.match(IDENTIFIER) {
			params = append(params, p.current().Lexeme)
			p.advance()
		}

		// Skip array brackets
		for p.match(LBRACKET) {
			p.advance()
			for !p.match(RBRACKET) && !p.match(EOF) {
				p.advance()
			}
			if p.match(RBRACKET) {
				p.advance()
			}
		}

		if p.match(COMMA) {
			p.advance()
			// Check for variadic ...
			if p.match(DOT) && p.peek(1).Type == DOT && p.peek(2).Type == DOT {
				p.advance() // skip first .
				p.advance() // skip second .
				p.advance() // skip third .
				// Variadic function - just continue to closing paren
			}
		}
	}

	if p.match(RPAREN) {
		p.advance()
	}

	// Skip GCC attributes like __THROW, __wur, __attribute__((...)...)
	for p.match(IDENTIFIER) {
		lexeme := p.current().Lexeme

		// Skip anything starting with __ (GCC attributes)
		if len(lexeme) >= 2 && lexeme[0] == '_' && lexeme[1] == '_' {
			p.advance()

			// If followed by (, skip the whole thing
			if p.match(LPAREN) {
				p.advance()
				depth := 1
				for depth > 0 && !p.match(EOF) {
					if p.match(LPAREN) {
						depth++
					} else if p.match(RPAREN) {
						depth--
					}
					p.advance()
				}
			}
		} else if lexeme == "extern" || lexeme == "static" || lexeme == "inline" ||
			lexeme == "_Thread_local" || lexeme == "__thread" {
			// These might appear here too - just skip
			p.advance()
		} else {
			// Not an attribute - stop
			break
		}
	}

	// Declaration only (external function)?
	if p.match(SEMICOLON) {
		p.advance()
		// Return a function node marked as external
		return &ASTNode{
			Type:       NodeFunction,
			Name:       name,
			ReturnType: returnType,
			Params:     params,
			ParamTypes: paramTypes,
			IsGlobal:   true, // Mark as external
			Children:   nil,  // No body
		}, nil
	}

	// Parse body
	body, err := p.parseBlock()
	if err != nil {
		return nil, fmt.Errorf("error parsing function '%s' body: %w (at token '%s', line %d)", name, err, p.current().Lexeme, p.current().Line)
	}

	return &ASTNode{
		Type:       NodeFunction,
		Name:       name,
		ReturnType: returnType,
		Params:     params,
		ParamTypes: paramTypes,
		Children:   []*ASTNode{body},
	}, nil
}

func (p *Parser) parseGlobalVar(name string, dataType string) (*ASTNode, error) {
	var decls []*ASTNode
	declType := dataType
	declName := name
	for {
		node, err := p.parseGlobalDeclarator(declName, declType, dataType)
		if err != nil {
			return nil, err
		}
		decls = append(decls, node)
		// Additional declarators: int a = 1, b = 2; (also with
		// per-declarator pointers: int *a, *b;).
		if !p.match(COMMA) {
			break
		}
		p.advance()
		declType = strings.TrimRight(dataType, "*")
		for p.match(STAR) {
			declType += "*"
			p.advance()
		}
		if !p.match(IDENTIFIER) {
			return nil, fmt.Errorf("expected identifier after ,")
		}
		declName = p.current().Lexeme
		p.advance()
	}
	for !p.match(SEMICOLON) && !p.match(EOF) {
		p.advance()
	}
	if p.match(SEMICOLON) {
		p.advance()
	}
	if len(decls) == 1 {
		return decls[0], nil
	}
	return &ASTNode{
		Type:     NodeBlock,
		Children: decls,
	}, nil
}

func (p *Parser) parseGlobalDeclarator(name string, declType string, dataType string) (*ASTNode, error) {
	var children []*ASTNode
	var dims []int
	// Array dimensions: record constant sizes (needed for .data sizing
	// and indexed addressing of initialized globals).
	for p.match(LBRACKET) {
		p.advance()
		if p.match(RBRACKET) {
			p.advance()
			dims = append(dims, 0) // unsized: infer from init below
			continue
		}
		sizeExpr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if v, ok := p.evalConstInt(sizeExpr); ok && v > 0 {
			dims = append(dims, v)
		} else {
			return nil, fmt.Errorf("array size must be a constant")
		}
		if !p.match(RBRACKET) {
			return nil, fmt.Errorf("expected ]")
		}
		p.advance()
	}
	// Optional constant initializer.
	if p.match(ASSIGN) {
		p.advance()
		if p.match(LBRACE) {
			// Struct/array brace initializer (e.g. globals with designators).
			init, err := p.parseCompoundLiteral(p.resolveTypedef(dataType))
			if err != nil {
				return nil, err
			}
			children = append(children, init)
		} else {
			init, err := p.parseAssignment()
			if err != nil {
				return nil, err
			}
			children = append(children, init)
		}
	}
	arraySize := 0
	if len(dims) > 0 {
		// Unsized single dim infers from a positional brace init.
		if len(dims) == 1 && dims[0] == 0 && len(children) > 0 &&
			children[0].Type == NodeCompoundLiteral && !p.isStructTypeName(declType) {
			allPos := true
			for _, f := range children[0].InitFields {
				if f != "" {
					allPos = false
					break
				}
			}
			if allPos {
				dims[0] = len(children[0].Children)
			}
		}
		total := 1
		for _, d := range dims {
			total *= d
		}
		arraySize = total
	}
	return &ASTNode{
		Type:      NodeVarDecl,
		VarName:   name,
		DataType:  declType,
		IsGlobal:  true,
		Children:  children,
		Dims:      dims,
		ArraySize: arraySize,
	}, nil
}

func (p *Parser) parseBlock() (*ASTNode, error) {
	if !p.match(LBRACE) {
		return nil, fmt.Errorf("expected {")
	}
	p.advance()

	block := &ASTNode{
		Type:     NodeBlock,
		Children: []*ASTNode{},
	}

	for !p.match(RBRACE) && !p.match(EOF) {
		stmt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		if stmt != nil {
			block.Children = append(block.Children, stmt)
		}
	}

	if p.match(RBRACE) {
		p.advance()
	}

	return block, nil
}

// parseStaticAssert parses `_Static_assert(expr, msg);` at block or file
// scope. Foldable-false conditions are compile errors (like C); anything
// else is dropped (it carries no runtime meaning).
func (p *Parser) parseStaticAssert() (*ASTNode, error) {
	p.advance() // skip _Static_assert
	if !p.match(LPAREN) {
		return nil, fmt.Errorf("expected ( after _Static_assert")
	}
	p.advance()
	cond, err := p.parseAssignment()
	if err != nil {
		return nil, err
	}
	if p.match(COMMA) {
		p.advance()
		// message: string literal (or any expression, skipped)
		if _, err := p.parseAssignment(); err != nil {
			return nil, err
		}
	}
	if !p.match(RPAREN) {
		return nil, fmt.Errorf("expected ) in _Static_assert")
	}
	p.advance()
	if p.match(SEMICOLON) {
		p.advance()
	}
	if v, ok := p.evalConstInt(cond); ok && v == 0 {
		return nil, fmt.Errorf("_Static_assert failed")
	}
	return nil, nil
}

func (p *Parser) parseStatement() (*ASTNode, error) {
	if p.match(IDENTIFIER) && p.current().Lexeme == "_Static_assert" {
		return p.parseStaticAssert()
	}
	// Variable declaration (with optional storage class and type modifiers)
	if p.match(INT, CHAR_KW, FLOAT, DOUBLE, VOID, STATIC, CONST, STRUCT, UNION, UNSIGNED, SIGNED, LONG, SHORT) {
		return p.parseVarDecl()
	}

	// `volatile` is not a keyword token (lexes as IDENTIFIER); at statement
	// start it can only be a type qualifier, so route it to parseVarDecl.
	if p.match(IDENTIFIER) && p.current().Lexeme == "volatile" {
		return p.parseVarDecl()
	}

	// Check if this could be a typedef variable declaration
	// Look ahead: if we have IDENTIFIER IDENTIFIER, it might be a typedef
	if p.match(IDENTIFIER) {
		// Check if this identifier is a known typedef
		if _, isTypedef := p.typedefs[p.current().Lexeme]; isTypedef {
			return p.parseVarDecl()
		}
		// Otherwise, check if next token is an identifier (typedef pattern)
		if p.peek(1).Type == IDENTIFIER {
			// Could be a typedef we don't know about yet, or a variable
			// For now, try to parse as variable declaration
			return p.parseVarDecl()
		}
	}

	// Goto statement: goto label;
	if p.match(GOTO) {
		p.advance()
		if !p.match(IDENTIFIER) {
			return nil, fmt.Errorf("expected label name after goto")
		}
		name := p.current().Lexeme
		p.advance()
		if p.match(SEMICOLON) {
			p.advance()
		}
		return &ASTNode{
			Type:  NodeGoto,
			Value: name,
		}, nil
	}

	// Label definition: name: (function-scoped, resolved per function)
	if p.match(IDENTIFIER) && p.peek(1).Type == COLON {
		name := p.current().Lexeme
		p.advance()
		p.advance() // skip :
		return &ASTNode{
			Type:  NodeLabel,
			Value: name,
		}, nil
	}

	// Return
	if p.match(RETURN) {
		return p.parseReturn()
	}

	// If
	if p.match(IF) {
		return p.parseIf()
	}

	// While
	if p.match(WHILE) {
		return p.parseWhile()
	}

	// Do-while
	if p.match(DO) {
		return p.parseDoWhile()
	}

	// For
	if p.match(FOR) {
		return p.parseFor()
	}

	// Switch
	if p.match(SWITCH) {
		return p.parseSwitch()
	}

	// Break
	if p.match(BREAK) {
		p.advance()
		if p.match(SEMICOLON) {
			p.advance()
		}
		return &ASTNode{Type: NodeBreak}, nil
	}

	// Continue
	if p.match(CONTINUE) {
		p.advance()
		if p.match(SEMICOLON) {
			p.advance()
		}
		return &ASTNode{Type: NodeContinue}, nil
	}

	// Block
	if p.match(LBRACE) {
		return p.parseBlock()
	}

	// Expression statement
	if !p.match(SEMICOLON) {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if p.match(SEMICOLON) {
			p.advance()
		}
		return &ASTNode{Type: NodeExprStmt, Children: []*ASTNode{expr}}, nil
	}

	if p.match(SEMICOLON) {
		p.advance()
	}

	return nil, nil
}

// evalConstInt folds a constant integer expression (for array sizes and
// other compile-time contexts): literals, enum names, unary -/+, and
// +,-,*,/,% over constants. sizeof() already folds to a number upstream.
func (p *Parser) evalConstInt(node *ASTNode) (int, bool) {
	if node == nil {
		return 0, false
	}
	switch node.Type {
	case NodeNumber:
		if v, err := strconv.ParseInt(node.Value, 0, 64); err == nil {
			return int(v), true
		}
		return node.IntValue, true
	case NodeIdentifier:
		if v, ok := p.enums[node.VarName]; ok {
			return v, true
		}
		return 0, false
	case NodeUnaryOp:
		if len(node.Children) != 1 {
			return 0, false
		}
		v, ok := p.evalConstInt(node.Children[0])
		if !ok {
			return 0, false
		}
		if node.Operator == "-" {
			return -v, true
		}
		if node.Operator == "+" {
			return v, true
		}
		if node.Operator == "~" {
			return ^v, true
		}
		return 0, false
	case NodeBinaryOp:
		if len(node.Children) != 2 {
			return 0, false
		}
		a, ok1 := p.evalConstInt(node.Children[0])
		b, ok2 := p.evalConstInt(node.Children[1])
		if !ok1 || !ok2 {
			return 0, false
		}
		switch node.Operator {
		case "+":
			return a + b, true
		case "-":
			return a - b, true
		case "*":
			return a * b, true
		case "/":
			if b == 0 {
				return 0, false
			}
			return a / b, true
		case "%":
			if b == 0 {
				return 0, false
			}
			return a % b, true
		case "<<":
			return a << uint(b), true
		case ">>":
			return a >> uint(b), true
		case "|":
			return a | b, true
		case "&":
			return a & b, true
		case "^":
			return a ^ b, true
		}
		return 0, false
	}
	return 0, false
}

func (p *Parser) parseVarDecl() (*ASTNode, error) {
	dataType := p.parseType()

	decls := []*ASTNode{}
	// parseType folds the first declarator's stars into dataType
	// ("float *v" -> "float*"). Later declarators must restart from the
	// star-free base, or `float *v, *n;` would build "float**" for n.
	baseType := strings.TrimRight(dataType, "*")
	for {
		// Pointer declarators: float *v, *n;  T *a = 0, *b = 0
		declType := dataType
		if len(decls) > 0 {
			declType = baseType
		}
		for p.match(STAR) {
			declType += "*"
			p.advance()
		}
		if !p.match(IDENTIFIER) {
			return nil, fmt.Errorf("expected identifier")
		}

		varName := p.current().Lexeme
		p.advance()

		node := &ASTNode{
			Type:     NodeVarDecl,
			VarName:  varName,
			DataType: declType,
		}

		// Handle array declaration (multi-dim): int arr[10][20]
		isArrayDecl := false
		for p.match(LBRACKET) {
			isArrayDecl = true
			p.advance()

			if !p.match(RBRACKET) {
				// Array size: any constant expression (8+1, 14*3, N).
				sizeExpr, err := p.parseExpression()
				if err != nil {
					return nil, err
				}

				if v, ok := p.evalConstInt(sizeExpr); ok && v > 0 {
					node.Dims = append(node.Dims, v)
				} else {
					return nil, fmt.Errorf("array size must be a constant")
				}
			}

			if !p.match(RBRACKET) {
				return nil, fmt.Errorf("expected ']'")
			}
			p.advance()
		}
		if len(node.Dims) > 0 {
			total := 1
			for _, d := range node.Dims {
				total *= d
			}
			node.ArraySize = total
		}

		// Handle initialization
		if p.match(ASSIGN) {
			p.advance()

			// Check if this is a struct/typedef initialization with brace initializer
			if p.match(LBRACE) {
				// Struct-array initializer: `E a[3] = {{...},{...}}`. Each
				// nested brace is one ELEMENT, not a member of the struct
				// (a separate parse path is required; see
				// parseArrayInitializer).
				resolvedType := p.resolveTypedef(dataType)
				var initExpr *ASTNode
				var err error
				if isArrayDecl && len(node.Dims) <= 1 && p.isStructTypeName(resolvedType) {
					initExpr, err = p.parseArrayInitializer(resolvedType)
				} else {
					// This is a compound literal initialization
					initExpr, err = p.parseCompoundLiteral(resolvedType)
				}
				if err != nil {
					return nil, err
				}
				node.Children = []*ASTNode{initExpr}
			} else {
				// Assignment level only: a top-level comma separates
				// declarators (int a = 1, b = 2), it isn't an operator.
				initExpr, err := p.parseAssignment()
				if err != nil {
					return nil, err
				}
				node.Children = []*ASTNode{initExpr}
			}
		}

		// Unsize arrays infer from brace init: `int a[] = {1,2,3}`.
		// (Structs excluded: `struct P p = {1,2}` is not an array.)
		if len(node.Dims) == 0 && len(node.Children) > 0 &&
			node.Children[0].Type == NodeCompoundLiteral && !p.isStructTypeName(declType) {
			allPos := true
			for _, f := range node.Children[0].InitFields {
				if f != "" {
					allPos = false
					break
				}
			}
			if allPos {
				if n := len(node.Children[0].Children); n > 0 {
					node.Dims = []int{n}
					node.ArraySize = n
				}
			}
		}

		decls = append(decls, node)

		// Additional declarators with the same type: int a = 1, b = 2;
		if !p.match(COMMA) {
			break
		}
		p.advance()
	}

	if p.match(SEMICOLON) {
		p.advance()
	}

	if len(decls) == 1 {
		return decls[0], nil
	}
	return &ASTNode{
		Type:     NodeBlock,
		Children: decls,
	}, nil
}

// isStructTypeName reports whether a declared type is a struct/union
// (directly or via typedef), for array-size inference.
func (p *Parser) isStructTypeName(t string) bool {
	t = strings.TrimSpace(t)
	for _, q := range []string{"const ", "volatile ", "static ", "extern "} {
		t = strings.TrimSpace(strings.TrimPrefix(t, q))
	}
	// Strip pointers: `struct S *` is not a struct value.
	t = strings.TrimSpace(strings.TrimRight(t, "*"))
	if strings.HasPrefix(t, "struct ") || strings.HasPrefix(t, "union ") {
		return true
	}
	if r := p.resolveTypedef(t); r != t {
		return p.isStructTypeName(r)
	}
	return false
}

func (p *Parser) parseReturn() (*ASTNode, error) {
	p.advance() // skip return

	node := &ASTNode{Type: NodeReturn}

	if !p.match(SEMICOLON) {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		node.Children = []*ASTNode{expr}
	}

	if p.match(SEMICOLON) {
		p.advance()
	}

	return node, nil
}

func (p *Parser) parseIf() (*ASTNode, error) {
	p.advance() // skip if

	if p.match(LPAREN) {
		p.advance()
	}

	cond, err := p.parseExpression()
	if err != nil {
		return nil, err
	}

	if p.match(RPAREN) {
		p.advance()
	}

	thenStmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}

	node := &ASTNode{
		Type:     NodeIf,
		Children: []*ASTNode{cond, thenStmt},
	}

	if p.match(ELSE) {
		p.advance()
		elseStmt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		node.Children = append(node.Children, elseStmt)
	}

	return node, nil
}

func (p *Parser) parseWhile() (*ASTNode, error) {
	p.advance() // skip while

	if p.match(LPAREN) {
		p.advance()
	}

	cond, err := p.parseExpression()
	if err != nil {
		return nil, err
	}

	if p.match(RPAREN) {
		p.advance()
	}

	body, err := p.parseStatement()
	if err != nil {
		return nil, err
	}

	return &ASTNode{
		Type:     NodeWhile,
		Children: []*ASTNode{cond, body},
	}, nil
}

func (p *Parser) parseDoWhile() (*ASTNode, error) {
	p.advance() // skip do

	body, err := p.parseStatement()
	if err != nil {
		return nil, err
	}

	if !p.match(WHILE) {
		return nil, fmt.Errorf("expected while after do body")
	}
	p.advance()

	if p.match(LPAREN) {
		p.advance()
	}

	cond, err := p.parseExpression()
	if err != nil {
		return nil, err
	}

	if p.match(RPAREN) {
		p.advance()
	}
	if p.match(SEMICOLON) {
		p.advance()
	}

	return &ASTNode{
		Type:     NodeDoWhile,
		Children: []*ASTNode{body, cond},
	}, nil
}

func (p *Parser) parseFor() (*ASTNode, error) {
	p.advance() // skip for

	if p.match(LPAREN) {
		p.advance()
	}

	var init *ASTNode
	if !p.match(SEMICOLON) {
		var err error
		if p.match(INT, CHAR_KW, FLOAT, DOUBLE, VOID, STATIC, CONST, STRUCT, UNION, UNSIGNED, SIGNED, LONG, SHORT) || p.isTypeName() {
			init, err = p.parseVarDecl()
		} else {
			init, err = p.parseExpression()
			if p.match(SEMICOLON) {
				p.advance()
			}
		}
		if err != nil {
			return nil, err
		}
	} else {
		p.advance()
	}

	var cond *ASTNode
	if !p.match(SEMICOLON) {
		var err error
		cond, err = p.parseExpression()
		if err != nil {
			return nil, err
		}
	}

	if p.match(SEMICOLON) {
		p.advance()
	}

	var incr *ASTNode
	if !p.match(RPAREN) {
		var err error
		incr, err = p.parseExpression()
		if err != nil {
			return nil, err
		}
	}

	if p.match(RPAREN) {
		p.advance()
	}

	body, err := p.parseStatement()
	if err != nil {
		return nil, err
	}

	children := []*ASTNode{}
	if init != nil {
		children = append(children, init)
	}
	if cond != nil {
		children = append(children, cond)
	}
	if incr != nil {
		children = append(children, incr)
	}
	children = append(children, body)

	return &ASTNode{
		Type:     NodeFor,
		Children: children,
		ForInit:  init,
		ForCond:  cond,
		ForIncr:  incr,
		ForBody:  body,
	}, nil
}

func (p *Parser) parseSwitch() (*ASTNode, error) {
	p.advance() // skip 'switch'

	if !p.match(LPAREN) {
		return nil, fmt.Errorf("expected '(' after switch")
	}
	p.advance()

	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}

	if !p.match(RPAREN) {
		return nil, fmt.Errorf("expected ')' after switch expression")
	}
	p.advance()

	if !p.match(LBRACE) {
		return nil, fmt.Errorf("expected '{' for switch body")
	}
	p.advance()

	cases := []*ASTNode{}
	var defaultCase *ASTNode

	for !p.match(RBRACE) && !p.isAtEnd() {
		if p.match(CASE) {
			caseNode, err := p.parseCase()
			if err != nil {
				return nil, err
			}
			cases = append(cases, caseNode)
		} else if p.match(DEFAULT) {
			p.advance()
			if !p.match(COLON) {
				return nil, fmt.Errorf("expected ':' after default")
			}
			p.advance()

			// Parse statements until next case/default/}
			stmts := []*ASTNode{}
			for !p.match(CASE, DEFAULT, RBRACE) && !p.isAtEnd() {
				stmt, err := p.parseStatement()
				if err != nil {
					return nil, err
				}
				stmts = append(stmts, stmt)
			}

			defaultCase = &ASTNode{
				Type:     NodeCase,
				Value:    "default",
				Children: stmts,
			}
		} else {
			return nil, fmt.Errorf("expected 'case' or 'default' in switch")
		}
	}

	if !p.match(RBRACE) {
		return nil, fmt.Errorf("expected '}' at end of switch")
	}
	p.advance()

	children := []*ASTNode{expr}
	children = append(children, cases...)
	if defaultCase != nil {
		children = append(children, defaultCase)
	}

	return &ASTNode{
		Type:     NodeSwitch,
		Children: children,
	}, nil
}

func (p *Parser) parseCase() (*ASTNode, error) {
	p.advance() // skip 'case'

	value, err := p.parseExpression()
	if err != nil {
		return nil, err
	}

	if !p.match(COLON) {
		return nil, fmt.Errorf("expected ':' after case value")
	}
	p.advance()

	// Parse statements until next case/default/}
	stmts := []*ASTNode{}
	for !p.match(CASE, DEFAULT, RBRACE) && !p.isAtEnd() {
		stmt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, stmt)
	}

	// First child is the case value, rest are statements
	children := []*ASTNode{value}
	children = append(children, stmts...)

	return &ASTNode{
		Type:     NodeCase,
		Children: children,
	}, nil
}

func (p *Parser) parseExpression() (*ASTNode, error) {
	// Comma operator (lowest precedence): a, b evaluates both, yields b.
	// Separators (call args, init lists, declarators) use parseAssignment
	// directly and are unaffected.
	first, err := p.parseAssignment()
	if err != nil {
		return nil, err
	}
	if !p.match(COMMA) {
		return first, nil
	}
	children := []*ASTNode{first}
	for p.match(COMMA) {
		p.advance()
		next, err := p.parseAssignment()
		if err != nil {
			return nil, err
		}
		children = append(children, next)
	}
	return &ASTNode{
		Type:     NodeComma,
		Children: children,
	}, nil
}

func (p *Parser) parseAssignment() (*ASTNode, error) {
	left, err := p.parseTernary()
	if err != nil {
		return nil, err
	}

	if p.match(ASSIGN, PLUSASSIGN, MINUSASSIGN, STARASSIGN, SLASHASSIGN, PERCENTASSIGN, LSHIFTASSIGN, RSHIFTASSIGN, BANDASSIGN, BORASSIGN, BXORASSIGN) {
		op := p.current().Lexeme
		p.advance()

		right, err := p.parseAssignment()
		if err != nil {
			return nil, err
		}

		return &ASTNode{
			Type:     NodeAssignment,
			Operator: op,
			Children: []*ASTNode{left, right},
		}, nil
	}

	return left, nil
}

func (p *Parser) parseTernary() (*ASTNode, error) {
	cond, err := p.parseLogicalOr()
	if err != nil {
		return nil, err
	}

	if p.match(QUESTION) {
		p.advance()

		thenExpr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}

		if !p.match(COLON) {
			return nil, fmt.Errorf("expected :")
		}
		p.advance()

		elseExpr, err := p.parseTernary()
		if err != nil {
			return nil, err
		}

		return &ASTNode{
			Type:     NodeTernary,
			Children: []*ASTNode{cond, thenExpr, elseExpr},
		}, nil
	}

	return cond, nil
}

func (p *Parser) parseLogicalOr() (*ASTNode, error) {
	left, err := p.parseLogicalAnd()
	if err != nil {
		return nil, err
	}

	for p.match(LOR) {
		op := p.current().Lexeme
		p.advance()

		right, err := p.parseLogicalAnd()
		if err != nil {
			return nil, err
		}

		left = &ASTNode{
			Type:     NodeBinaryOp,
			Operator: op,
			Children: []*ASTNode{left, right},
		}
	}

	return left, nil
}

func (p *Parser) parseLogicalAnd() (*ASTNode, error) {
	left, err := p.parseBitwiseOr()
	if err != nil {
		return nil, err
	}

	for p.match(LAND) {
		op := p.current().Lexeme
		p.advance()

		right, err := p.parseBitwiseOr()
		if err != nil {
			return nil, err
		}

		left = &ASTNode{
			Type:     NodeBinaryOp,
			Operator: op,
			Children: []*ASTNode{left, right},
		}
	}

	return left, nil
}

func (p *Parser) parseBitwiseOr() (*ASTNode, error) {
	left, err := p.parseBitwiseXor()
	if err != nil {
		return nil, err
	}

	for p.match(BOR) {
		op := p.current().Lexeme
		p.advance()

		right, err := p.parseBitwiseXor()
		if err != nil {
			return nil, err
		}

		left = &ASTNode{
			Type:     NodeBinaryOp,
			Operator: op,
			Children: []*ASTNode{left, right},
		}
	}

	return left, nil
}

func (p *Parser) parseBitwiseXor() (*ASTNode, error) {
	left, err := p.parseBitwiseAnd()
	if err != nil {
		return nil, err
	}

	for p.match(BXOR) {
		op := p.current().Lexeme
		p.advance()

		right, err := p.parseBitwiseAnd()
		if err != nil {
			return nil, err
		}

		left = &ASTNode{
			Type:     NodeBinaryOp,
			Operator: op,
			Children: []*ASTNode{left, right},
		}
	}

	return left, nil
}

func (p *Parser) parseBitwiseAnd() (*ASTNode, error) {
	left, err := p.parseEquality()
	if err != nil {
		return nil, err
	}

	for p.match(BAND) {
		op := p.current().Lexeme
		p.advance()

		right, err := p.parseEquality()
		if err != nil {
			return nil, err
		}

		left = &ASTNode{
			Type:     NodeBinaryOp,
			Operator: op,
			Children: []*ASTNode{left, right},
		}
	}

	return left, nil
}

func (p *Parser) parseEquality() (*ASTNode, error) {
	left, err := p.parseComparison()
	if err != nil {
		return nil, err
	}

	for p.match(EQ, NE) {
		op := p.current().Lexeme
		p.advance()

		right, err := p.parseComparison()
		if err != nil {
			return nil, err
		}

		left = &ASTNode{
			Type:     NodeBinaryOp,
			Operator: op,
			Children: []*ASTNode{left, right},
		}
	}

	return left, nil
}

func (p *Parser) parseComparison() (*ASTNode, error) {
	left, err := p.parseShift()
	if err != nil {
		return nil, err
	}

	for p.match(LT, LE, GT, GE) {
		op := p.current().Lexeme
		p.advance()

		right, err := p.parseShift()
		if err != nil {
			return nil, err
		}

		left = &ASTNode{
			Type:     NodeBinaryOp,
			Operator: op,
			Children: []*ASTNode{left, right},
		}
	}

	return left, nil
}

func (p *Parser) parseShift() (*ASTNode, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}

	for p.match(LSHIFT, RSHIFT) {
		op := p.current().Lexeme
		p.advance()

		right, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}

		left = &ASTNode{
			Type:     NodeBinaryOp,
			Operator: op,
			Children: []*ASTNode{left, right},
		}
	}

	return left, nil
}

func (p *Parser) parseAdditive() (*ASTNode, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}

	for p.match(PLUS, MINUS) {
		op := p.current().Lexeme
		p.advance()

		right, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}

		left = &ASTNode{
			Type:     NodeBinaryOp,
			Operator: op,
			Children: []*ASTNode{left, right},
		}
	}

	return left, nil
}

func (p *Parser) parseMultiplicative() (*ASTNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}

	for p.match(STAR, SLASH, PERCENT) {
		op := p.current().Lexeme
		p.advance()

		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}

		left = &ASTNode{
			Type:     NodeBinaryOp,
			Operator: op,
			Children: []*ASTNode{left, right},
		}
	}

	return left, nil
}

func (p *Parser) parseUnary() (*ASTNode, error) {
	if p.match(MINUS, LNOT, BNOT, BAND, STAR, INC, DEC) {
		op := p.current().Lexeme
		p.advance()

		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}

		return &ASTNode{
			Type:     NodeUnaryOp,
			Operator: op,
			Children: []*ASTNode{operand},
		}, nil
	}

	return p.parsePostfix()
}

func (p *Parser) parsePostfix() (*ASTNode, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}

	for {
		if p.match(INC, DEC) {
			op := p.current().Lexeme
			p.advance()
			left = &ASTNode{
				Type:     NodeUnaryOp,
				Operator: op + "_post",
				Children: []*ASTNode{left},
			}
		} else if p.match(LBRACKET) {
			p.advance()
			index, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			if !p.match(RBRACKET) {
				return nil, fmt.Errorf("expected ]")
			}
			p.advance()
			left = &ASTNode{
				Type:     NodeArrayAccess,
				Children: []*ASTNode{left, index},
			}
		} else if p.match(DOT, ARROW) {
			op := p.current().Lexeme
			p.advance()
			if !p.match(IDENTIFIER) {
				return nil, fmt.Errorf("expected identifier")
			}
			member := p.current().Lexeme
			p.advance()
			left = &ASTNode{
				Type:       NodeMemberAccess,
				Operator:   op,
				MemberName: member,
				IsPointer:  (op == "->"),
				Children:   []*ASTNode{left},
			}
		} else if p.match(LPAREN) {
			// Call through any postfix expression: fn(), cb.fn(), (*fp)().
			p.advance()
			args := []*ASTNode{}
			for !p.match(RPAREN) && !p.match(EOF) {
				arg, err := p.parseAssignment()
				if err != nil {
					return nil, err
				}
				args = append(args, arg)
				if p.match(COMMA) {
					p.advance()
				}
			}
			if p.match(RPAREN) {
				p.advance()
			}
			left = &ASTNode{
				Type:     NodeCall,
				Target:   left,
				Children: args,
			}
		} else {
			break
		}
	}

	return left, nil
}

func (p *Parser) parsePrimary() (*ASTNode, error) {
	// Number
	if p.match(NUMBER) {
		value := p.current().Lexeme
		p.advance()
		value, dataType := typeNumberLiteral(value)
		intVal, _ := strconv.Atoi(value)

		return &ASTNode{
			Type:     NodeNumber,
			Value:    value,
			IntValue: intVal,
			DataType: dataType,
		}, nil
	}

	// sizeof operator
	if p.match(SIZEOF) {
		p.advance()

		// sizeof(type) or sizeof(expr)
		if !p.match(LPAREN) {
			return nil, fmt.Errorf("expected '(' after sizeof")
		}
		p.advance()

		// Try to parse as a type
		var sizeVal int
		if p.match(INT, CHAR_KW, VOID, FLOAT, DOUBLE, STRUCT, UNION, UNSIGNED, SIGNED, LONG, SHORT) || p.isTypeName() {
			// Type
			typeName := p.parseType()
			sizeVal = p.getTypeSize(typeName)
		} else {
			// Expression form: defer to selection (needs scope info for
			// array totals; the parser has no symbol table).
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			if !p.match(RPAREN) {
				return nil, fmt.Errorf("expected ')' after sizeof")
			}
			p.advance()

			return &ASTNode{
				Type:     NodeSizeof,
				Children: []*ASTNode{expr},
			}, nil
		}

		if !p.match(RPAREN) {
			return nil, fmt.Errorf("expected ')' after sizeof")
		}
		p.advance()

		return &ASTNode{
			Type:     NodeNumber,
			Value:    fmt.Sprintf("%d", sizeVal),
			IntValue: sizeVal,
		}, nil
	}

	// String (with adjacent-string-literal concatenation, as in C:
	// "a" "b" -> "ab"). Sea used to take only the first literal, which
	// truncated global char* initializers like ahoy_lit_vs to "#version 330\\n"
	// and left the shader unusable.
	if p.match(STRING) {
		value := p.current().Lexeme
		p.advance()
		for p.match(STRING) {
			value += p.current().Lexeme
			p.advance()
		}
		return &ASTNode{
			Type:  NodeString,
			Value: value,
		}, nil
	}

	// Character
	if p.match(CHAR) {
		lexeme := p.current().Lexeme
		p.advance()

		// Convert character literal to numeric value
		var charValue int
		if len(lexeme) == 0 {
			return nil, fmt.Errorf("empty character literal")
		} else if lexeme[0] == '\\' && len(lexeme) > 1 {
			// Escape sequence
			switch lexeme[1] {
			case '0':
				charValue = 0
			case 'n':
				charValue = 10
			case 't':
				charValue = 9
			case 'r':
				charValue = 13
			case '\\':
				charValue = 92
			case '\'':
				charValue = 39
			case '"':
				charValue = 34
			default:
				charValue = int(lexeme[1])
			}
		} else {
			// Regular character
			charValue = int(lexeme[0])
		}

		return &ASTNode{
			Type:  NodeNumber,
			Value: fmt.Sprintf("%d", charValue),
		}, nil
	}

	// Identifier or function call
	if p.match(IDENTIFIER) {
		name := p.current().Lexeme
		p.advance()

		// __builtin_offsetof(Type, member[.sub...]) — compile-time constant.
		if name == "__builtin_offsetof" && p.match(LPAREN) {
			p.advance()
			typeName := p.parseType()
			if !p.match(COMMA) {
				return nil, fmt.Errorf("expected ',' in __builtin_offsetof")
			}
			p.advance()
			memberPath := ""
			for {
				if !p.match(IDENTIFIER) {
					return nil, fmt.Errorf("expected member name in __builtin_offsetof")
				}
				memberPath += p.current().Lexeme
				p.advance()
				if p.match(DOT) {
					memberPath += "."
					p.advance()
					continue
				}
				break
			}
			if !p.match(RPAREN) {
				return nil, fmt.Errorf("expected ')' in __builtin_offsetof")
			}
			p.advance()
			return &ASTNode{
				Type:     NodeOffsetof,
				DataType: typeName,
				Value:    memberPath,
			}, nil
		}

		// Function call
		if p.match(LPAREN) {
			p.advance()

			args := []*ASTNode{}
			for !p.match(RPAREN) && !p.match(EOF) {
				arg, err := p.parseAssignment()
				if err != nil {
					return nil, err
				}
				args = append(args, arg)

				if p.match(COMMA) {
					p.advance()
				}
			}

			if p.match(RPAREN) {
				p.advance()
			}

			return &ASTNode{
				Type:     NodeCall,
				Name:     name,
				Children: args,
			}, nil
		}

		// Variable
		return &ASTNode{
			Type:    NodeIdentifier,
			VarName: name,
		}, nil
	}

	// Parenthesized expression or statement expression
	if p.match(LPAREN) {
		p.advance()

		// Check for statement expression: ({ statements; expr; })
		if p.match(LBRACE) {
			return p.parseStatementExpression()
		}

		// Check for cast: (typename)
		// We need to distinguish (Type)expr from (expr)
		// Strategy: A cast has the pattern: ( type-specifiers * ) where there's a ) after the type
		// Use lookahead to check if this looks like a cast before committing

		isCast := false

		// Definite type keywords indicate a cast
		if p.match(INT, CHAR_KW, FLOAT, DOUBLE, VOID, UNSIGNED, SIGNED, LONG, SHORT, CONST) {
			isCast = true
		} else if p.match(STRUCT, UNION) {
			// struct/union is definitely a type
			isCast = true
		} else if p.isTypeName() {
			// It's a typedef - need to check if it's being used as a type or variable
			// Heuristic: if next token after identifier is * or ), it's likely a cast
			// (TypeName*) expr   -> cast
			// (TypeName) expr    -> could be cast or paren expr
			// (varname + 1)      -> paren expr

			// Peek ahead: after the identifier, what comes next?
			if p.pos+1 < len(p.tokens) {
				nextToken := p.tokens[p.pos+1]
				if nextToken.Type == STAR || nextToken.Type == RPAREN {
					// (TypeName*) or (TypeName) - likely a cast
					isCast = true
				} else if nextToken.Type == LPAREN && p.pos+2 < len(p.tokens) && p.tokens[p.pos+2].Type == STAR {
					// (TypeName (*)(...)) - function-pointer cast
					isCast = true
				}
			}
		}

		if isCast {
			// Parse as cast
			castType := p.parseType()
			if p.match(LPAREN) {
				// Function-pointer cast: (ret (*)(params)). The value
				// passes through (all pointers are addresses); the type
				// marks it callable through for indirect calls.
				p.advance() // (
				if !p.match(STAR) {
					return nil, fmt.Errorf("expected * in function pointer cast")
				}
				p.advance()
				if !p.match(RPAREN) {
					return nil, fmt.Errorf("expected ) in function pointer cast")
				}
				p.advance()
				if !p.match(LPAREN) {
					return nil, fmt.Errorf("expected (params) in function pointer cast")
				}
				depth := 0
				for !p.match(EOF) {
					if p.match(LPAREN) {
						depth++
					} else if p.match(RPAREN) {
						depth--
						if depth == 0 {
							p.advance()
							break
						}
					}
					p.advance()
				}
				if !p.match(RPAREN) {
					return nil, fmt.Errorf("expected ) after function pointer cast")
				}
				p.advance()
				expr, err := p.parseUnary()
				if err != nil {
					return nil, err
				}
				return &ASTNode{
					Type:     NodeCast,
					DataType: castType + "*",
					Children: []*ASTNode{expr},
				}, nil
			}
			if p.match(RPAREN) {
				p.advance()

				// Check for compound literal: (Type){...}
				if p.match(LBRACE) {
					return p.parseCompoundLiteral(castType)
				}

				// Regular cast: (Type)expr
				expr, err := p.parseUnary()
				if err != nil {
					return nil, err
				}
				return &ASTNode{
					Type:     NodeCast,
					DataType: castType,
					Children: []*ASTNode{expr},
				}, nil
			}
			// If no RPAREN, this is a parse error
			return nil, fmt.Errorf("expected ) after type in cast at line %d", p.current().Line)
		}

		// Not a cast, parse as regular parenthesized expression
		expr, err := p.parseExpression()
		if err != nil {
			return nil, fmt.Errorf("error in parenthesized expression: %w", err)
		}

		if !p.match(RPAREN) {
			return nil, fmt.Errorf("expected ) at line %d, got %s (after parsing expression starting at line %d)", p.current().Line, p.current().Lexeme, p.tokens[p.pos-10].Line)
		}
		p.advance()

		return expr, nil
	}

	return nil, fmt.Errorf("unexpected token: %s at line %d", p.current().Lexeme, p.current().Line)
}

// typeNumberLiteral splits a numeric lexeme (which may carry U/L suffixes)
// into clean digits and its C type. Unsuffixed decimals get their type by
// magnitude (int -> long -> unsigned long); suffixes force the type.
// Clean digits matter: suffixes in immediates don't assemble, and huge
// values are silently truncated to 32 bits without a 64-bit type.
func typeNumberLiteral(lexeme string) (digits string, dataType string) {
	// Floats (but not hex integers, whose E is a digit).
	isHex := strings.HasPrefix(lexeme, "0x") || strings.HasPrefix(lexeme, "0X")
	if strings.Contains(lexeme, ".") || (!isHex && (strings.Contains(lexeme, "e") || strings.Contains(lexeme, "E"))) {
		// Strip a float suffix (1.0f); single precision is its own type —
		// without this, float slots got truncated double bits (zeros).
		if strings.HasSuffix(lexeme, "f") || strings.HasSuffix(lexeme, "F") {
			return lexeme[:len(lexeme)-1], "float"
		}
		return lexeme, "double"
	}
	// Split trailing integer suffix [uUlL]+.
	i := len(lexeme)
	for i > 0 && strings.ContainsRune("uUlL", rune(lexeme[i-1])) {
		i--
	}
	suffix := lexeme[i:]
	digits = lexeme[:i]
	hasU := strings.ContainsAny(suffix, "uU")
	longs := strings.Count(strings.ToLower(suffix), "l")
	switch {
	case longs >= 2 && hasU:
		return digits, "unsigned long long"
	case longs >= 2:
		return digits, "long long"
	case longs == 1 && hasU:
		return digits, "unsigned long"
	case longs == 1:
		return digits, "long"
	case hasU:
		return digits, "unsigned int"
	}
	// No suffix: type by magnitude (decimal rules; hex also fits below).
	if v, err := strconv.ParseUint(digits, 0, 64); err == nil {
		if v <= 2147483647 {
			return digits, "int"
		}
		if v <= 9223372036854775807 {
			return digits, "long"
		}
		return digits, "unsigned long"
	}
	return digits, "int"
}

// parseArrayInitializer parses `{...}` for an array declaration whose
// element type is elemType. A nested brace is one ELEMENT, parsed as a
// compound literal of elemType — not a member index of a struct literal.
// parseCompoundLiteral's member lookup sent `stbtt__edge a[6] = {{...}}`
// element 0 through member 0 (a float), found no nested struct for the
// brace, and silently skipped every brace, leaving the array zeroed.
// Only struct/union element types and single dimensions use this path;
// scalar and multi-dimensional arrays keep the old behavior.
func (p *Parser) parseArrayInitializer(elemType string) (*ASTNode, error) {
	if !p.match(LBRACE) {
		return nil, fmt.Errorf("expected { for array initializer")
	}
	p.advance()

	initValues := []*ASTNode{}
	for !p.match(RBRACE) && !p.match(EOF) {
		if p.match(LBRACE) {
			// One struct/union element.
			elem, err := p.parseCompoundLiteral(elemType)
			if err != nil {
				return nil, err
			}
			initValues = append(initValues, elem)
		} else {
			value, err := p.parseAssignment()
			if err != nil {
				return nil, err
			}
			initValues = append(initValues, value)
		}
		if p.match(COMMA) {
			p.advance()
		} else {
			break
		}
	}
	if !p.match(RBRACE) {
		return nil, fmt.Errorf("expected } at end of array initializer")
	}
	p.advance()

	return &ASTNode{
		Type:       NodeCompoundLiteral,
		DataType:   elemType,
		InitFields: make([]string, len(initValues)), // all positional
		Children:   initValues,
	}, nil
}

func (p *Parser) parseCompoundLiteral(typeName string) (*ASTNode, error) {
	// Parse compound literal: {.field1=val1, .field2=val2, ...}
	// or positional: {val1, val2, ...}

	// Resolve typedef
	resolvedType := p.resolveTypedef(typeName)

	if !p.match(LBRACE) {
		return nil, fmt.Errorf("expected { for compound literal")
	}
	p.advance()

	initFields := []string{}
	initValues := []*ASTNode{}
	idx := 0

	for !p.match(RBRACE) && !p.match(EOF) {
		// Check for designated initializer: .fieldname = value
		if p.match(DOT) {
			p.advance()

			if !p.match(IDENTIFIER) {
				return nil, fmt.Errorf("expected field name after .")
			}
			fieldName := p.current().Lexeme
			p.advance()

			if !p.match(ASSIGN) {
				return nil, fmt.Errorf("expected = after field name")
			}
			p.advance()

			value, err := p.parseAssignment()
			if err != nil {
				return nil, err
			}

			initFields = append(initFields, fieldName)
			initValues = append(initValues, value)
		} else {
			// Positional initializer. A nested brace is a struct/union
			// literal for the corresponding member.
			if p.match(LBRACE) {
				nestedType := typeName
				sdName := strings.TrimSpace(typeName)
				for _, q := range []string{"struct ", "union "} {
					sdName = strings.TrimSpace(strings.TrimPrefix(sdName, q))
				}
				if sd, ok := p.structs[sdName]; ok && idx < len(sd.Members) {
					nt := sd.Members[idx].Type
					if bracketIdx := strings.Index(nt, "["); bracketIdx >= 0 {
						nt = strings.TrimSpace(nt[:bracketIdx])
					}
					nestedType = nt
				}
				nestedSDName := strings.TrimSpace(nestedType)
				for _, q := range []string{"struct ", "union "} {
					nestedSDName = strings.TrimSpace(strings.TrimPrefix(nestedSDName, q))
				}
				if _, ok := p.structs[nestedSDName]; ok {
					nested, err := p.parseCompoundLiteral(nestedType)
					if err != nil {
						return nil, err
					}
					initFields = append(initFields, "")
					initValues = append(initValues, nested)
				} else {
					// Skip the brace (scalar/array initializer)
					depth := 0
					for !p.match(EOF) {
						if p.match(LBRACE) {
							depth++
						}
						if p.match(RBRACE) {
							depth--
							if depth == 0 {
								p.advance()
								break
							}
						}
						p.advance()
					}
					initFields = append(initFields, "")
					initValues = append(initValues, &ASTNode{Type: NodeNumber, Value: "0"})
				}
			} else {
				value, err := p.parseAssignment()
				if err != nil {
					return nil, err
				}

				initFields = append(initFields, "") // Empty string means positional
				initValues = append(initValues, value)
			}
		}

		if p.match(COMMA) {
			p.advance()
			idx++
		} else {
			break
		}
	}

	if !p.match(RBRACE) {
		return nil, fmt.Errorf("expected } at end of compound literal")
	}
	p.advance()

	return &ASTNode{
		Type:       NodeCompoundLiteral,
		DataType:   resolvedType,
		InitFields: initFields,
		Children:   initValues,
	}, nil
}

func (p *Parser) parseStatementExpression() (*ASTNode, error) {
	// Parse statement expression: ({ statements; result_expr; })
	// This is a GCC extension used heavily in macros

	if !p.match(LBRACE) {
		return nil, fmt.Errorf("expected { for statement expression")
	}
	p.advance()

	statements := []*ASTNode{}
	var resultExpr *ASTNode

	// Parse statements until we hit the last expression
	for !p.match(RBRACE) && !p.match(EOF) {
		stmt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		statements = append(statements, stmt)
	}

	if !p.match(RBRACE) {
		return nil, fmt.Errorf("expected } at end of statement expression")
	}
	p.advance()

	if !p.match(RPAREN) {
		return nil, fmt.Errorf("expected ) after statement expression")
	}
	p.advance()

	// The last statement should be an expression that yields the result
	// For now, we'll wrap all statements in a block and return the last one
	if len(statements) > 0 {
		lastStmt := statements[len(statements)-1]

		// If last statement is an expression statement, use its value
		if lastStmt.Type == NodeExprStmt && len(lastStmt.Children) > 0 {
			resultExpr = lastStmt.Children[0]
			statements = statements[:len(statements)-1]
		} else {
			resultExpr = lastStmt
			statements = statements[:len(statements)-1]
		}
	}

	// Create a block with all statements except the last
	// and return the result expression wrapped in the block context
	if len(statements) > 0 {
		block := &ASTNode{
			Type:     NodeBlock,
			Children: statements,
		}

		// Wrap in a temporary assignment pattern
		// This makes the IR generator handle it like: { stmts; tmp = expr; } return tmp;
		if resultExpr != nil {
			block.Children = append(block.Children, &ASTNode{
				Type:     NodeExprStmt,
				Children: []*ASTNode{resultExpr},
			})
		}

		return block, nil
	}

	return resultExpr, nil
}
