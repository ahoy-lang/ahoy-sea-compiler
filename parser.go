package main

import (
	"fmt"
	"strconv"
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
)

type ASTNode struct {
	Type     NodeType
	Children []*ASTNode
	
	// Metadata
	Value    string
	DataType string
	IntValue int  // For number literals
	
	// For function nodes
	Name       string
	Params     []string
	ParamTypes []string
	ReturnType string
	
	// For operators
	Operator string
	
	// For variables
	VarName string
	IsGlobal bool
	Offset   int
	
	// For member access
	MemberName   string // Name of the member being accessed
	IsPointer    bool // true for -> operator, false for . operator (in member access context)
	
	// For arrays and pointers
	ArraySize    int  // Size of array (0 if not an array)
	PointerLevel int  // Level of pointer indirection
	StructType   string // For struct variables, the struct name
	
	// For compound literals
	InitFields   []string // Field names for designated initializers
	
	Line   int
	Column int
}

// StructMember represents a member of a struct
type StructMember struct {
	Name   string
	Type   string
	Offset int
	Size   int
}

// StructDef represents a struct definition
type StructDef struct {
	Name    string
	Members []StructMember
	Size    int
}

type Parser struct {
	compiler *Compiler
	tokens   []Token
	pos      int
	structs  map[string]*StructDef // Track struct definitions
	typedefs map[string]string     // Track typedef aliases: alias -> actual type
	enums    map[string]int        // Track enum constants: name -> value
}

func NewParser(source string) *Parser {
	lexer := NewLexer(source)
	tokens := lexer.AllTokens()
	
	return &Parser{
		tokens:   tokens,
		pos:      0,
		structs:  make(map[string]*StructDef),
		typedefs: make(map[string]string),
		enums:    make(map[string]int),
	}
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
	
	// Basic types
	switch typ {
	case "int", "float":
		return 4
	case "long", "double":
		return 8
	case "char":
		return 1
	case "void":
		return 0
	default:
		return 4 // Default
	}
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
	
	for p.current().Type != EOF {
		// Skip preprocessor
		if p.match(HASH) {
			p.skipPreprocessor()
			continue
		}
		
		node, err := p.parseTopLevel()
		if err != nil {
			return nil, err
		}
		
		if node != nil {
			program.Children = append(program.Children, node)
		}
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
	
	// Handle typedef
	if p.match(TYPEDEF) {
		p.advance()
		
		// typedef struct { ... } Name; or typedef struct Name Name;
		if p.match(STRUCT, UNION) {
			p.advance()
			
			var structName string
			var aliasName string
			
			// Check if struct has a name
			if p.match(IDENTIFIER) {
				structName = p.current().Lexeme
				p.advance()
			}
			
			// If there's a body, parse it properly
			if p.match(LBRACE) {
				// Create temporary struct name if none given
				if structName == "" {
					structName = "__anon_typedef_" + fmt.Sprintf("%d", p.pos)
				}
				
				// Parse struct members
				p.advance() // skip {
				
				members := []StructMember{}
				offset := 0
				
				for !p.match(RBRACE) && !p.match(EOF) {
					memberType := p.parseType()
					
					// Parse member name(s) - can have multiple per line like: int r, g, b, a;
					for {
						if !p.match(IDENTIFIER) {
							return nil, fmt.Errorf("expected member name")
						}
						memberName := p.current().Lexeme
						p.advance()
						
						memberSize := p.getTypeSize(memberType)
						
						// Handle arrays: int arr[10];
						if p.match(LBRACKET) {
							p.advance()
							if p.match(NUMBER) {
								sizeVal, _ := strconv.Atoi(p.current().Lexeme)
								memberSize = sizeVal * memberSize
								p.advance()
							}
							if !p.match(RBRACKET) {
								return nil, fmt.Errorf("expected ]")
							}
							p.advance()
						}
						
						members = append(members, StructMember{
							Name:   memberName,
							Type:   memberType,
							Offset: offset,
							Size:   memberSize,
						})
						offset += memberSize
						
						// Continue if we see a comma (multiple declarators on same line)
						if p.match(COMMA) {
							p.advance()
							continue  // Parse next member with same type
						}
						
						// End of declarator list for this type
						break
					}
					
					if !p.match(SEMICOLON) {
						return nil, fmt.Errorf("expected ; after struct member")
					}
					p.advance()
				}
				
				if !p.match(RBRACE) {
					return nil, fmt.Errorf("expected } at end of struct")
				}
				p.advance()
				
				// Store the struct definition
				p.structs[structName] = &StructDef{
					Name:    structName,
					Members: members,
					Size:    offset,
				}
			}
			
			// Get the typedef alias name
			if p.match(IDENTIFIER) {
				aliasName = p.current().Lexeme
				p.advance()
			}
			
			// Register the typedef
			if aliasName != "" {
				p.typedefs[aliasName] = "struct " + structName
			}
			
			if p.match(SEMICOLON) {
				p.advance()
			}
			return nil, nil
		}
		
		// typedef existing_type new_name;
		existingType := p.parseType()
		if p.match(IDENTIFIER) {
			aliasName := p.current().Lexeme
			p.advance()
			p.typedefs[aliasName] = existingType
		}
		
		if p.match(SEMICOLON) {
			p.advance()
		}
		return nil, nil
	}
	
	// Skip struct/union/typedef/enum - parse struct/union definitions
	if p.match(STRUCT, UNION) {
		err := p.parseStructDef()
		if err != nil {
			return nil, err
		}
		return nil, nil
	}
	
	if p.match(TYPEDEF) {
		p.skipStructOrTypedef()
		return nil, nil
	}
	
	if p.match(ENUM) {
		err := p.parseEnumDef()
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
	
	// Storage class
	if p.match(STATIC, CONST) {
		typ += p.current().Lexeme + " "
		p.advance()
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
		structOrUnion := p.current().Lexeme  // "struct" or "union"
		p.advance()
		if p.match(IDENTIFIER) {
			typ = structOrUnion + " " + p.current().Lexeme
			p.advance()
		} else if p.match(LBRACE) {
			// Anonymous struct/union definition
			// Generate a unique name for it
			anonName := fmt.Sprintf("__anon_%s_%d", structOrUnion, p.pos)
			typ = structOrUnion + " " + anonName
			
			// Parse the struct/union definition properly
			p.advance() // skip {
			
			var members []StructMember
			offset := 0
			for !p.match(RBRACE) && !p.match(EOF) {
				// Parse field type
				fieldType := p.parseType()
				
				// Parse field name
				if !p.match(IDENTIFIER) {
					// Skip this - might be an error but continue parsing
					p.advance()
					continue
				}
				fieldName := p.current().Lexeme
				p.advance()
				
				// Calculate size (simplified - just use 8 bytes for everything)
				fieldSize := 8
				
				members = append(members, StructMember{
					Name:   fieldName,
					Type:   fieldType,
					Offset: offset,
					Size:   fieldSize,
				})
				
				// For unions, all members are at offset 0
				if structOrUnion == "union" {
					offset = 0
				} else {
					offset += fieldSize
				}
				
				// Expect semicolon
				if !p.match(SEMICOLON) {
					// Skip if no semicolon - just continue to next iteration
					continue
				}
				p.advance() // Skip the semicolon
			}
			
			// We're now at } - advance past it
			if p.match(RBRACE) {
				p.advance()
			} else {
				// Shouldn't happen but skip to closing brace if needed
				for !p.match(RBRACE) && !p.match(EOF) {
					p.advance()
				}
				if p.match(RBRACE) {
					p.advance()
				}
			}
			
			// Register the anonymous struct/union
			p.structs[anonName] = &StructDef{
				Name:    anonName,
				Members: members,
				Size:    offset,
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
		} else {
			// Has modifiers like "static" - check if this looks like a type name
			// If the identifier starts with uppercase or is known pattern, treat as type
			// Otherwise it's the variable name
			// For now, consume it as a type name if we haven't seen a base type yet
			p.advance()
			typ += typeName
		}
	}
	
	// Pointers
	for p.match(STAR) {
		typ += "*"
		p.advance()
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

func (p *Parser) parseStructDef() error {
	p.advance() // skip 'struct'
	
	// Get struct name
	if !p.match(IDENTIFIER) {
		return fmt.Errorf("expected struct name")
	}
	structName := p.current().Lexeme
	p.advance()
	
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
	p.advance() // skip {
	
	members := []StructMember{}
	currentOffset := 0
	
	// Parse members
	for !p.match(RBRACE) && !p.match(EOF) {
		// Parse member type
		memberType := p.parseType()
		
		// Parse member name(s) - can have multiple per line
		for {
			if !p.match(IDENTIFIER) {
				return fmt.Errorf("expected member name in struct")
			}
			
			memberName := p.current().Lexeme
			p.advance()
			
			// For now, all types are 8 bytes
			memberSize := 8
			
			// Handle arrays: int arr[10];
			if p.match(LBRACKET) {
				p.advance()
				if p.match(NUMBER) {
					sizeVal, _ := strconv.Atoi(p.current().Lexeme)
					memberSize = sizeVal * 8
					p.advance()
				}
				if !p.match(RBRACKET) {
					return fmt.Errorf("expected ]")
				}
				p.advance()
			}
			
			members = append(members, StructMember{
				Name:   memberName,
				Type:   memberType,
				Offset: currentOffset,
				Size:   memberSize,
			})
			
			currentOffset += memberSize
			
			if p.match(COMMA) {
				p.advance()
				continue
			}
			break
		}
		
		if !p.match(SEMICOLON) {
			return fmt.Errorf("expected ; after struct member")
		}
		p.advance()
	}
	
	if !p.match(RBRACE) {
		return fmt.Errorf("expected } at end of struct")
	}
	p.advance()
	
	// Optional variable name and semicolon
	if p.match(IDENTIFIER) {
		p.advance()
	}
	if p.match(SEMICOLON) {
		p.advance()
	}
	
	// Store struct definition
	p.structs[structName] = &StructDef{
		Name:    structName,
		Members: members,
		Size:    currentOffset,
	}
	
	return nil
}

func (p *Parser) parseEnumDef() error {
	p.advance() // skip 'enum'
	
	// Optional enum name
	if p.match(IDENTIFIER) {
		_ = p.current().Lexeme // enumName not used yet
		p.advance()
	}
	
	// Check for just declaration (enum Foo;)
	if p.match(SEMICOLON) {
		p.advance()
		return nil // Forward declaration, ignore
	}
	
	if !p.match(LBRACE) {
		return fmt.Errorf("expected { or ; after enum name")
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
			
			// Parse the value - for simplicity, only handle number literals
			if p.match(NUMBER) {
				value, err := strconv.Atoi(p.current().Lexeme)
				if err == nil {
					currentValue = value
				}
				p.advance()
			} else {
				// Skip complex expressions
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
		return fmt.Errorf("expected } at end of enum")
	}
	p.advance()
	
	// Optional variable name and semicolon
	if p.match(IDENTIFIER) {
		p.advance()
	}
	if p.match(SEMICOLON) {
		p.advance()
	}
	
	return nil
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
			IsGlobal:   true,  // Mark as external
			Children:   nil,   // No body
		}, nil
	}
	
	// Parse body
	body, err := p.parseBlock()
	if err != nil {
		return nil, err
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
	// Skip initializers and array dims for now
	for !p.match(SEMICOLON) && !p.match(EOF) {
		p.advance()
	}
	
	if p.match(SEMICOLON) {
		p.advance()
	}
	
	return &ASTNode{
		Type:     NodeVarDecl,
		VarName:  name,
		DataType: dataType,
		IsGlobal: true,
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

func (p *Parser) parseStatement() (*ASTNode, error) {
	// Variable declaration (with optional storage class and type modifiers)
	if p.match(INT, CHAR_KW, FLOAT, DOUBLE, STATIC, CONST, STRUCT, UNION, UNSIGNED, SIGNED, LONG, SHORT) {
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

func (p *Parser) parseVarDecl() (*ASTNode, error) {
	dataType := p.parseType()
	
	if !p.match(IDENTIFIER) {
		return nil, fmt.Errorf("expected identifier")
	}
	
	varName := p.current().Lexeme
	p.advance()
	
	node := &ASTNode{
		Type:     NodeVarDecl,
		VarName:  varName,
		DataType: dataType,
	}
	
	// Handle array declaration: int arr[10]
	if p.match(LBRACKET) {
		p.advance()
		
		if !p.match(RBRACKET) {
			// Array size
			sizeExpr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			
			// For now, only support constant sizes
			if sizeExpr.Type == NodeNumber {
				node.ArraySize = sizeExpr.IntValue
			} else {
				return nil, fmt.Errorf("array size must be a constant")
			}
		}
		
		if !p.match(RBRACKET) {
			return nil, fmt.Errorf("expected ']'")
		}
		p.advance()
	}
	
	// Handle initialization
	if p.match(ASSIGN) {
		p.advance()
		
		// Check if this is a struct/typedef initialization with brace initializer
		if p.match(LBRACE) {
			// This is a compound literal initialization
			resolvedType := p.resolveTypedef(dataType)
			initExpr, err := p.parseCompoundLiteral(resolvedType)
			if err != nil {
				return nil, err
			}
			node.Children = []*ASTNode{initExpr}
		} else {
			initExpr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			node.Children = []*ASTNode{initExpr}
		}
	}
	
	if p.match(SEMICOLON) {
		p.advance()
	}
	
	return node, nil
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

func (p *Parser) parseFor() (*ASTNode, error) {
	p.advance() // skip for
	
	if p.match(LPAREN) {
		p.advance()
	}
	
	var init *ASTNode
	if !p.match(SEMICOLON) {
		var err error
		if p.match(INT) {
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
	return p.parseAssignment()
}

func (p *Parser) parseAssignment() (*ASTNode, error) {
	left, err := p.parseTernary()
	if err != nil {
		return nil, err
	}
	
	if p.match(ASSIGN, PLUSASSIGN, MINUSASSIGN, STARASSIGN, SLASHASSIGN) {
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
		intVal, _ := strconv.Atoi(value)
		return &ASTNode{
			Type:     NodeNumber,
			Value:    value,
			IntValue: intVal,
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
			// Expression - parse and get its type
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			// For now, assume all expressions are int-sized
			sizeVal = p.getTypeSize(expr.DataType)
			if sizeVal == 0 {
				sizeVal = 4 // Default to int size
			}
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
	
	// String
	if p.match(STRING) {
		value := p.current().Lexeme
		p.advance()
		return &ASTNode{
			Type:  NodeString,
			Value: value,
		}, nil
	}
	
	// Character
	if p.match(CHAR) {
		value := p.current().Lexeme
		p.advance()
		return &ASTNode{
			Type:  NodeNumber,
			Value: value,
		}, nil
	}
	
	// Identifier or function call
	if p.match(IDENTIFIER) {
		name := p.current().Lexeme
		p.advance()
		
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
				}
			}
		}
		
		if isCast {
			// Parse as cast
			castType := p.parseType()
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
			return nil, err
		}
		
		if !p.match(RPAREN) {
			return nil, fmt.Errorf("expected ) at line %d, got %s", p.current().Line, p.current().Lexeme)
		}
		p.advance()
		
		return expr, nil
	}
	
	return nil, fmt.Errorf("unexpected token: %s at line %d", p.current().Lexeme, p.current().Line)
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
			// Positional initializer
			value, err := p.parseAssignment()
			if err != nil {
				return nil, err
			}
			
			initFields = append(initFields, "") // Empty string means positional
			initValues = append(initValues, value)
		}
		
		if p.match(COMMA) {
			p.advance()
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
