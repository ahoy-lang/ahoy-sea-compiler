package main

import (
	"fmt"
	"strconv"
)

type Compiler struct {
	lexer    *Lexer
	current  Token
	peek     Token
	codegen  *CodeGenerator
}

func NewCompilerWithCodegen(source string) *Compiler {
	lexer := NewLexer(source)
	c := &Compiler{
		lexer:   lexer,
		codegen: NewCodeGenerator(),
	}
	// Prime the pump
	c.advance()
	c.advance()
	return c
}

func (c *Compiler) advance() Token {
	prev := c.current
	c.current = c.peek
	c.peek = c.lexer.NextToken()
	return prev
}

func (c *Compiler) expect(typ TokenType) error {
	if c.current.Type != typ {
		return fmt.Errorf("expected %s, got %s at line %d", typ, c.current.Type, c.current.Line)
	}
	c.advance()
	return nil
}

func (c *Compiler) match(types ...TokenType) bool {
	for _, t := range types {
		if c.current.Type == t {
			return true
		}
	}
	return false
}

// Compile the entire program
func (c *Compiler) Compile() error {
	for c.current.Type != EOF {
		if err := c.compileTopLevel(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Compiler) compileTopLevel() error {
	// Skip preprocessor
	if c.match(HASH) {
		for c.current.Type != EOF && c.current.Line == c.peek.Line {
			c.advance()
		}
		return nil
	}
	
	// Parse type
	typeStr := ""
	if c.match(INT, VOID, CHAR_KW, FLOAT, DOUBLE) {
		typeStr = c.current.Lexeme
		c.advance()
	} else if c.match(STRUCT, TYPEDEF, ENUM) {
		// Skip for now
		c.advance()
		for !c.match(SEMICOLON, EOF) {
			if c.match(LBRACE) {
				depth := 1
				c.advance()
				for depth > 0 && c.current.Type != EOF {
					if c.match(LBRACE) {
						depth++
					} else if c.match(RBRACE) {
						depth--
					}
					c.advance()
				}
			} else {
				c.advance()
			}
		}
		if c.match(SEMICOLON) {
			c.advance()
		}
		return nil
	}
	
	// Skip pointers
	for c.match(STAR) {
		typeStr += "*"
		c.advance()
	}
	
	// Get identifier
	if !c.match(IDENTIFIER) {
		c.advance()
		return nil
	}
	
	name := c.current.Lexeme
	c.advance()
	
	// Function or variable?
	if c.match(LPAREN) {
		return c.compileFunction(name, typeStr)
	} else {
		return c.compileGlobalVar(name, typeStr)
	}
}

func (c *Compiler) compileFunction(name string, returnType string) error {
	c.advance() // skip (
	
	// Parse parameters
	params := []string{}
	paramCount := 0
	
	for !c.match(RPAREN) && !c.match(EOF) {
		if c.match(VOID) && c.peek.Type == RPAREN {
			c.advance()
			break
		}
		
		// Skip type
		if c.match(INT, VOID, CHAR_KW, FLOAT, DOUBLE, STRUCT, IDENTIFIER) {
			c.advance()
		}
		for c.match(STAR) {
			c.advance()
		}
		
		// Get param name
		if c.match(IDENTIFIER) {
			params = append(params, c.current.Lexeme)
			paramCount++
			c.advance()
		}
		
		// Skip array brackets
		for c.match(LBRACKET) {
			c.advance()
			for !c.match(RBRACKET) && !c.match(EOF) {
				c.advance()
			}
			if c.match(RBRACKET) {
				c.advance()
			}
		}
		
		if c.match(COMMA) {
			c.advance()
		}
	}
	
	if c.match(RPAREN) {
		c.advance()
	}
	
	// Declaration only?
	if c.match(SEMICOLON) {
		c.advance()
		return nil
	}
	
	// Function body
	if c.match(LBRACE) {
		c.codegen.genFunctionPrologue(name, paramCount)
		
		// Store parameters in local vars
		for i, param := range params {
			if i < len(argRegs) {
				c.codegen.allocLocal(param, 8)
			}
		}
		
		c.advance() // skip {
		
		// Compile function body
		for !c.match(RBRACE) && !c.match(EOF) {
			if err := c.compileStatement(); err != nil {
				return err
			}
		}
		
		// Default return
		c.codegen.genReturn(false)
		
		if c.match(RBRACE) {
			c.advance()
		}
	}
	
	return nil
}

func (c *Compiler) compileGlobalVar(name string, typ string) error {
	// Skip array dimensions and initializers
	for !c.match(SEMICOLON) && !c.match(EOF) {
		c.advance()
	}
	
	c.codegen.declareGlobal(name, 8) // Default to 8 bytes
	
	if c.match(SEMICOLON) {
		c.advance()
	}
	return nil
}

func (c *Compiler) compileStatement() error {
	// Variable declaration
	if c.match(INT, CHAR_KW, FLOAT, DOUBLE) {
		return c.compileLocalVar()
	}
	
	// Return
	if c.match(RETURN) {
		c.advance()
		
		if !c.match(SEMICOLON) {
			// Compile return expression
			reg, err := c.compileExpression()
			if err != nil {
				return err
			}
			
			// Move result to RAX
			if reg != RAX {
				c.codegen.emit("    movq %%%s, %%rax", regNames[reg])
			}
			c.codegen.freeReg(reg)
			c.codegen.genReturn(true)
		} else {
			c.codegen.genReturn(false)
		}
		
		if c.match(SEMICOLON) {
			c.advance()
		}
		return nil
	}
	
	// If statement
	if c.match(IF) {
		return c.compileIf()
	}
	
	// While loop
	if c.match(WHILE) {
		return c.compileWhile()
	}
	
	// For loop
	if c.match(FOR) {
		return c.compileFor()
	}
	
	// Block
	if c.match(LBRACE) {
		c.advance()
		for !c.match(RBRACE) && !c.match(EOF) {
			if err := c.compileStatement(); err != nil {
				return err
			}
		}
		if c.match(RBRACE) {
			c.advance()
		}
		return nil
	}
	
	// Expression statement
	if !c.match(SEMICOLON) {
		reg, err := c.compileExpression()
		if err != nil {
			return err
		}
		c.codegen.freeReg(reg)
	}
	
	if c.match(SEMICOLON) {
		c.advance()
	}
	
	return nil
}

func (c *Compiler) compileLocalVar() error {
	c.advance() // skip type
	
	if !c.match(IDENTIFIER) {
		return fmt.Errorf("expected identifier")
	}
	
	varName := c.current.Lexeme
	c.advance()
	
	// Allocate on stack
	c.codegen.allocLocal(varName, 8)
	
	// Handle initialization
	if c.match(ASSIGN) {
		c.advance()
		
		reg, err := c.compileExpression()
		if err != nil {
			return err
		}
		
		c.codegen.genStoreVar(reg, varName)
		c.codegen.freeReg(reg)
	}
	
	if c.match(SEMICOLON) {
		c.advance()
	}
	
	return nil
}

func (c *Compiler) compileIf() error {
	c.advance() // skip if
	
	if c.match(LPAREN) {
		c.advance()
	}
	
	// Compile condition
	reg, err := c.compileExpression()
	if err != nil {
		return err
	}
	
	elseLabel := c.codegen.newLabel()
	endLabel := c.codegen.newLabel()
	
	// Test condition
	c.codegen.emit("    testq %%%s, %%%s", regNames[reg], regNames[reg])
	c.codegen.genCondJump("z", elseLabel) // Jump if zero (false)
	c.codegen.freeReg(reg)
	
	if c.match(RPAREN) {
		c.advance()
	}
	
	// Then branch
	if err := c.compileStatement(); err != nil {
		return err
	}
	
	c.codegen.genJump(endLabel)
	c.codegen.emit("%s:", elseLabel)
	
	// Else branch
	if c.match(ELSE) {
		c.advance()
		if err := c.compileStatement(); err != nil {
			return err
		}
	}
	
	c.codegen.emit("%s:", endLabel)
	return nil
}

func (c *Compiler) compileWhile() error {
	c.advance() // skip while
	
	startLabel := c.codegen.newLabel()
	endLabel := c.codegen.newLabel()
	
	c.codegen.emit("%s:", startLabel)
	
	if c.match(LPAREN) {
		c.advance()
	}
	
	// Compile condition
	reg, err := c.compileExpression()
	if err != nil {
		return err
	}
	
	c.codegen.emit("    testq %%%s, %%%s", regNames[reg], regNames[reg])
	c.codegen.genCondJump("z", endLabel)
	c.codegen.freeReg(reg)
	
	if c.match(RPAREN) {
		c.advance()
	}
	
	// Body
	if err := c.compileStatement(); err != nil {
		return err
	}
	
	c.codegen.genJump(startLabel)
	c.codegen.emit("%s:", endLabel)
	
	return nil
}

func (c *Compiler) compileFor() error {
	c.advance() // skip for
	
	if c.match(LPAREN) {
		c.advance()
	}
	
	// Init
	if !c.match(SEMICOLON) {
		if c.match(INT) {
			c.compileLocalVar()
		} else {
			reg, _ := c.compileExpression()
			c.codegen.freeReg(reg)
			if c.match(SEMICOLON) {
				c.advance()
			}
		}
	} else {
		c.advance()
	}
	
	startLabel := c.codegen.newLabel()
	endLabel := c.codegen.newLabel()
	incrLabel := c.codegen.newLabel()
	
	c.codegen.emit("%s:", startLabel)
	
	// Condition
	if !c.match(SEMICOLON) {
		reg, err := c.compileExpression()
		if err != nil {
			return err
		}
		c.codegen.emit("    testq %%%s, %%%s", regNames[reg], regNames[reg])
		c.codegen.genCondJump("z", endLabel)
		c.codegen.freeReg(reg)
	}
	
	if c.match(SEMICOLON) {
		c.advance()
	}
	
	// Skip increment expression position for now
	_ = c.current  // Save position
	_ = c.peek
	
	// Skip increment for now
	depth := 1
	for depth > 0 && c.current.Type != EOF {
		if c.match(LPAREN) {
			depth++
		} else if c.match(RPAREN) {
			depth--
		}
		if depth > 0 {
			c.advance()
		}
	}
	
	if c.match(RPAREN) {
		c.advance()
	}
	
	// Compile body
	if err := c.compileStatement(); err != nil {
		return err
	}
	
	c.codegen.emit("%s:", incrLabel)
	
	// TODO: Compile increment
	
	c.codegen.genJump(startLabel)
	c.codegen.emit("%s:", endLabel)
	
	return nil
}

// Expression compilation
func (c *Compiler) compileExpression() (int, error) {
	return c.compileAssignment()
}

func (c *Compiler) compileAssignment() (int, error) {
	// Check if it's an assignment
	if c.match(IDENTIFIER) {
		varName := c.current.Lexeme
		
		// Peek ahead to see if it's assignment
		if c.peek.Type == ASSIGN {
			c.advance() // skip identifier
			c.advance() // skip =
			
			reg, err := c.compileAssignment()
			if err != nil {
				return 0, err
			}
			
			c.codegen.genStoreVar(reg, varName)
			return reg, nil
		}
	}
	
	return c.compileComparison()
}

func (c *Compiler) compileComparison() (int, error) {
	left, err := c.compileAdditive()
	if err != nil {
		return 0, err
	}
	
	if c.match(EQ, NE, LT, LE, GT, GE) {
		op := c.current.Type
		c.advance()
		
		right, err := c.compileAdditive()
		if err != nil {
			return 0, err
		}
		
		c.codegen.genCmp(left, right)
		
		condition := ""
		switch op {
		case EQ:
			condition = "e"
		case NE:
			condition = "ne"
		case LT:
			condition = "l"
		case LE:
			condition = "le"
		case GT:
			condition = "g"
		case GE:
			condition = "ge"
		}
		
		c.codegen.genSetCC(condition, left)
		c.codegen.freeReg(right)
		
		return left, nil
	}
	
	return left, nil
}

func (c *Compiler) compileAdditive() (int, error) {
	left, err := c.compileMultiplicative()
	if err != nil {
		return 0, err
	}
	
	for c.match(PLUS, MINUS) {
		op := c.current.Type
		c.advance()
		
		right, err := c.compileMultiplicative()
		if err != nil {
			return 0, err
		}
		
		if op == PLUS {
			c.codegen.genAdd(left, right)
		} else {
			c.codegen.genSub(left, right)
		}
		
		c.codegen.freeReg(right)
	}
	
	return left, nil
}

func (c *Compiler) compileMultiplicative() (int, error) {
	left, err := c.compileUnary()
	if err != nil {
		return 0, err
	}
	
	for c.match(STAR, SLASH) {
		op := c.current.Type
		c.advance()
		
		right, err := c.compileUnary()
		if err != nil {
			return 0, err
		}
		
		if op == STAR {
			c.codegen.genMul(left, right)
		} else {
			c.codegen.genDiv(left, right)
		}
		
		c.codegen.freeReg(right)
	}
	
	return left, nil
}

func (c *Compiler) compileUnary() (int, error) {
	if c.match(MINUS) {
		c.advance()
		reg, err := c.compileUnary()
		if err != nil {
			return 0, err
		}
		c.codegen.emit("    negq %%%s", regNames[reg])
		return reg, nil
	}
	
	return c.compilePrimary()
}

func (c *Compiler) compilePrimary() (int, error) {
	// Number
	if c.match(NUMBER) {
		val, _ := strconv.ParseInt(c.current.Lexeme, 0, 64)
		c.advance()
		
		reg := c.codegen.allocReg()
		c.codegen.genLoadImm(reg, val)
		return reg, nil
	}
	
	// Variable
	if c.match(IDENTIFIER) {
		varName := c.current.Lexeme
		c.advance()
		
		// Function call?
		if c.match(LPAREN) {
			c.advance()
			
			// Parse arguments
			argRegs := []int{}
			for !c.match(RPAREN) && !c.match(EOF) {
				reg, err := c.compileExpression()
				if err != nil {
					return 0, err
				}
				argRegs = append(argRegs, reg)
				
				if c.match(COMMA) {
					c.advance()
				}
			}
			
			// Move args to calling convention registers
			for i, reg := range argRegs {
				if i < len(argRegs) {
					if reg != argRegs[i] {
						c.codegen.emit("    movq %%%s, %%%s", regNames[reg], regNames[argRegs[i]])
					}
					c.codegen.freeReg(reg)
				}
			}
			
			if c.match(RPAREN) {
				c.advance()
			}
			
			c.codegen.genCall(varName, len(argRegs))
			
			// Result in RAX
			return RAX, nil
		}
		
		// Load variable
		reg := c.codegen.allocReg()
		c.codegen.genLoadVar(reg, varName)
		return reg, nil
	}
	
	// Parenthesized expression
	if c.match(LPAREN) {
		c.advance()
		reg, err := c.compileExpression()
		if err != nil {
			return 0, err
		}
		if c.match(RPAREN) {
			c.advance()
		}
		return reg, nil
	}
	
	return 0, fmt.Errorf("unexpected token: %s", c.current.Lexeme)
}
