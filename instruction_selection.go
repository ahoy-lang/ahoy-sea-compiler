package main

import (
	"fmt"
	"strings"
)

// Three-address code intermediate representation
type OpCode int

const (
	OpNop OpCode = iota
	OpAdd
	OpSub
	OpMul
	OpDiv
	OpMod
	OpNeg
	OpAnd
	OpOr
	OpXor
	OpNot
	OpShl
	OpShr
	OpEq
	OpNe
	OpLt
	OpLe
	OpGt
	OpGe
	OpMov
	OpLoad
	OpStore
	OpLoadAddr
	OpCall
	OpRet
	OpJmp
	OpJz
	OpJnz
	OpLabel
	OpPush
	OpPop
	OpParam
)

type Operand struct {
	Type       string // "reg", "imm", "var", "label", "mem", "array"
	Value      string
	Offset     int
	IsGlobal   bool
	Size       int
	IndexTemp  *Operand // For array indexing, holds the calculated offset
	DataType   string   // Track the C type (e.g., "int", "struct Foo*")
	SourcePtr  *Operand // For dereferenced values, track the original pointer
}

type IRInstruction struct {
	Op   OpCode
	Dst  *Operand
	Src1 *Operand
	Src2 *Operand
}

type FunctionSignature struct {
	ReturnType string
	ParamTypes []string
}

type InstructionSelector struct {
	instructions []*IRInstruction
	currentFunc  string
	labelCounter int
	tempCounter  int
	
	// Symbol tables
	localVars    map[string]*Symbol
	globalVars   map[string]*Symbol
	functions    map[string]*FunctionSignature // Track function signatures
	stringLits   map[string]string
	structs      map[string]*StructDef  // Struct definitions from parser
	typedefs     map[string]string      // Typedef aliases from parser
	enums        map[string]int         // Enum constants from parser
	
	stackOffset  int
}

func NewInstructionSelector() *InstructionSelector {
	is := &InstructionSelector{
		instructions: []*IRInstruction{},
		localVars:    make(map[string]*Symbol),
		globalVars:   make(map[string]*Symbol),
		functions:    make(map[string]*FunctionSignature),
		stringLits:   make(map[string]string),
		structs:      make(map[string]*StructDef),
		typedefs:     make(map[string]string),
		enums:        make(map[string]int),
	}
	
	// Add standard library external symbols
	is.globalVars["stderr"] = &Symbol{
		Name:       "stderr",
		Type:       "void*",
		IsGlobal:   true,
		IsExternal: true,
	}
	is.globalVars["stdout"] = &Symbol{
		Name:       "stdout",
		Type:       "void*",
		IsGlobal:   true,
		IsExternal: true,
	}
	is.globalVars["stdin"] = &Symbol{
		Name:       "stdin",
		Type:       "void*",
		IsGlobal:   true,
		IsExternal: true,
	}
	
	// Add raylib color constants as external symbols
	colorType := "Color"
	rayColors := []string{"RED", "WHITE", "BLACK", "GRAY", "LIGHTGRAY", "DARKGRAY",
		"YELLOW", "GOLD", "ORANGE", "PINK", "MAROON", "GREEN", "LIME", "DARKGREEN",
		"SKYBLUE", "BLUE", "DARKBLUE", "PURPLE", "VIOLET", "DARKPURPLE",
		"BEIGE", "BROWN", "DARKBROWN", "RAYWHITE", "MAGENTA"}
	for _, color := range rayColors {
		is.globalVars[color] = &Symbol{
			Name:     color,
			Type:     colorType,
			IsGlobal: true,
		}
	}
	
	return is
}

func (is *InstructionSelector) newTemp() *Operand {
	is.tempCounter++
	return &Operand{
		Type:  "temp",
		Value: fmt.Sprintf("t%d", is.tempCounter),
	}
}

func (is *InstructionSelector) newLabel(prefix string) string {
	is.labelCounter++
	return fmt.Sprintf("%s_%d", prefix, is.labelCounter)
}

func (is *InstructionSelector) emit(op OpCode, dst, src1, src2 *Operand) {
	is.instructions = append(is.instructions, &IRInstruction{
		Op:   op,
		Dst:  dst,
		Src1: src1,
		Src2: src2,
	})
}

// getTypeSize returns the size in bytes of a type
func (is *InstructionSelector) getTypeSize(typ string) int {
	return is.getTypeSizeHelper(typ, make(map[string]bool))
}

func (is *InstructionSelector) getTypeSizeHelper(typ string, visited map[string]bool) int {
	// Prevent infinite recursion
	if visited[typ] {
		return 8 // Default size to break cycle
	}
	visited[typ] = true
	
	// Remove const/static modifiers
	typ = strings.TrimSpace(typ)
	for {
		trimmed := false
		for _, prefix := range []string{"static ", "const ", "extern ", "volatile ", "register "} {
			if strings.HasPrefix(typ, prefix) {
				typ = strings.TrimSpace(typ[len(prefix):])
				trimmed = true
				break
			}
		}
		if !trimmed {
			break
		}
	}
	
	// Pointers are 8 bytes
	if len(typ) > 0 && typ[len(typ)-1] == '*' {
		return 8
	}
	
	// Check for struct types
	if len(typ) > 7 && typ[:7] == "struct " {
		structName := typ[7:]
		structName = strings.TrimSpace(structName)
		if structDef, ok := is.structs[structName]; ok {
			return structDef.Size
		}
		return 8 // Default struct size
	}
	
	// Check for typedef'd types
	if actualType, ok := is.typedefs[typ]; ok {
		return is.getTypeSizeHelper(actualType, visited)
	}
	
	// Basic types
	switch typ {
	case "char", "signed char", "unsigned char":
		return 1
	case "short", "short int", "signed short", "unsigned short":
		return 2
	case "int", "signed int", "unsigned int", "long", "signed long", "unsigned long":
		return 4
	case "long long", "signed long long", "unsigned long long":
		return 8
	case "float":
		return 4
	case "double":
		return 8
	case "void":
		return 0
	default:
		return 8
	}
}

// isLargeStruct returns true if the type is a struct larger than 16 bytes
func (is *InstructionSelector) isLargeStruct(typ string) bool {
	return is.isLargeStructHelper(typ, make(map[string]bool))
}

func (is *InstructionSelector) isLargeStructHelper(typ string, visited map[string]bool) bool {
	// Prevent infinite recursion
	if visited[typ] {
		return false
	}
	visited[typ] = true
	
	// Remove qualifiers
	typ = strings.TrimSpace(typ)
	for {
		trimmed := false
		for _, prefix := range []string{"static ", "const ", "extern ", "volatile ", "register "} {
			if strings.HasPrefix(typ, prefix) {
				typ = strings.TrimSpace(typ[len(prefix):])
				trimmed = true
				break
			}
		}
		if !trimmed {
			break
		}
	}
	
	// Not a large struct if it's a pointer
	if len(typ) > 0 && typ[len(typ)-1] == '*' {
		return false
	}
	
	// Check if it's a struct type
	structName := typ
	if len(typ) > 7 && typ[:7] == "struct " {
		structName = typ[7:]
	} else if actualType, ok := is.typedefs[typ]; ok {
		// Resolve typedef
		return is.isLargeStructHelper(actualType, visited)
	} else {
		// Not a struct
		return false
	}
	
	structName = strings.TrimSpace(structName)
	if structDef, ok := is.structs[structName]; ok {
		return structDef.Size > 16
	}
	
	return false
}

// resolveType resolves typedef aliases to actual struct names
// Handles pointers by stripping them before resolution and re-adding after
func (is *InstructionSelector) resolveType(typ string) string {
	// Count and strip pointers
	pointerCount := 0
	for len(typ) > 0 && typ[len(typ)-1] == '*' {
		pointerCount++
		typ = typ[:len(typ)-1]
	}
	
	// Resolve typedef if it exists
	if resolvedType, ok := is.typedefs[typ]; ok {
		typ = resolvedType
	}
	
	// Re-add pointers
	for i := 0; i < pointerCount; i++ {
		typ += "*"
	}
	
	return typ
}

func (is *InstructionSelector) SelectInstructions(ast *ASTNode) error {
	for _, child := range ast.Children {
		if err := is.selectNode(child); err != nil {
			return err
		}
	}
	return nil
}

func (is *InstructionSelector) selectNode(node *ASTNode) error {
	if node == nil {
		return nil
	}
	
	switch node.Type {
	case NodeProgram:
		for _, child := range node.Children {
			if err := is.selectNode(child); err != nil {
				return err
			}
		}
		
	case NodeFunction:
		// Track the function signature
		is.functions[node.Name] = &FunctionSignature{
			ReturnType: node.ReturnType,
			ParamTypes: node.ParamTypes,
		}
		
		// Skip external function declarations (no body)
		if node.Children == nil || len(node.Children) == 0 {
			// External function - just track it (no code generation)
			return nil
		}
		
		is.currentFunc = node.Name
		is.localVars = make(map[string]*Symbol)
		is.stackOffset = 0
		
		// Emit function label
		is.emit(OpLabel, &Operand{Type: "label", Value: node.Name}, nil, nil)
		
		// Check if this function returns a large struct (>16 bytes)
		// If so, the first parameter (RDI) is a hidden pointer to the return buffer
		var hiddenRetPtr *Symbol
		paramRegStartIdx := 0
		
		if node.ReturnType != "" && is.isLargeStruct(node.ReturnType) {
			// Allocate space for hidden return pointer
			is.stackOffset -= 8
			hiddenRetPtr = &Symbol{
				Name:   "__retptr",
				Type:   node.ReturnType + "*",
				Offset: is.stackOffset,
				Size:   8,
			}
			is.localVars["__retptr"] = hiddenRetPtr
			
			// Save the hidden pointer from RDI directly to stack (use "mem" not "var" to avoid register allocation)
			retPtrReg := &Operand{Type: "reg", Value: "rdi"}
			retPtrMem := &Operand{Type: "mem", Offset: is.stackOffset}
			is.emit(OpStore, retPtrMem, retPtrReg, nil)
			
			// Regular parameters start at RSI (index 1)
			paramRegStartIdx = 1
		}
		
		// Allocate parameters
		argRegs := []string{"rdi", "rsi", "rdx", "rcx", "r8", "r9"}
		for i, param := range node.Params {
			is.stackOffset -= 8
			paramType := ""
			if i < len(node.ParamTypes) {
				paramType = node.ParamTypes[i]
			}
			is.localVars[param] = &Symbol{
				Name:   param,
				Type:   paramType,
				Offset: is.stackOffset,
				Size:   8,
			}
			
			// Move from argument register to stack
			// Account for hidden pointer if present  
			// Use "mem" type to prevent register allocation
			regIdx := i + paramRegStartIdx
			if regIdx < len(argRegs) {
				argReg := &Operand{Type: "reg", Value: argRegs[regIdx]}
				paramOp := &Operand{Type: "mem", Offset: is.stackOffset}
				is.emit(OpStore, paramOp, argReg, nil)
			}
		}
		
		// Function body
		if len(node.Children) > 0 {
			if err := is.selectNode(node.Children[0]); err != nil {
				return err
			}
		}
		
		// Default return if no explicit return
		is.emit(OpRet, nil, nil, nil)
		
	case NodeVarDecl:
		// Calculate size based on type and array size
		varSize := 8  // Default for int/pointer
		dataType := node.DataType
		
		// Strip storage class specifiers (static, const, extern, etc.)
		dataType = strings.TrimSpace(dataType)
		for {
			trimmed := false
			for _, prefix := range []string{"static ", "const ", "extern ", "volatile ", "register "} {
				if strings.HasPrefix(dataType, prefix) {
					dataType = strings.TrimSpace(dataType[len(prefix):])
					trimmed = true
					break
				}
			}
			if !trimmed {
				break
			}
		}
		
		// Check if it's a struct type
		if len(dataType) > 7 && dataType[:7] == "struct " {
			structName := dataType[7:]
			// Remove pointer indicator if present
			if len(structName) > 0 && structName[len(structName)-1] == '*' {
				varSize = 8  // Pointer to struct
			} else if structDef, ok := is.structs[structName]; ok {
				varSize = structDef.Size
			}
		}
		
		if node.ArraySize > 0 {
			varSize = node.ArraySize * varSize  // Array: count * element size
		}
		
		if node.IsGlobal {
			is.globalVars[node.VarName] = &Symbol{
				Name:      node.VarName,
				IsGlobal:  true,
				Size:      varSize,
				ArraySize: node.ArraySize,
				Type:      dataType,
			}
		} else {
			is.stackOffset -= varSize
			varOffset := is.stackOffset  // Save the variable's offset
			is.localVars[node.VarName] = &Symbol{
				Name:      node.VarName,
				Offset:    varOffset,
				Size:      varSize,
				ArraySize: node.ArraySize,
				Type:      dataType,
			}
			
			// Handle initialization (only for non-arrays for now)
			if len(node.Children) > 0 && node.ArraySize == 0 {
				initExpr := node.Children[0]
				
				// Check if this is a compound literal initializing a struct
				if initExpr.Type == NodeCompoundLiteral {
					// For compound literals, we need to copy the struct
					// The compound literal creates a temporary and returns its address
					// We need to copy from that temp to our variable
					
					result, err := is.selectExpression(initExpr)
					if err != nil {
						return err
					}
					
					// Get struct size
					structSize := varSize
					
					// Copy struct data from compound literal temp to our variable
					// result contains the address of the temporary
					// We need to copy structSize bytes
					for offset := 0; offset < structSize; offset += 8 {
						// Load from compound literal temp
						// result is a temp register containing the address
						srcOp := &Operand{
							Type:      "ptr",
							IndexTemp: result,
						}
						if offset > 0 {
							offsetOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", offset)}
							addrTemp := is.newTemp()
							is.emit(OpAdd, addrTemp, result, offsetOp)
							srcOp.IndexTemp = addrTemp
						}
						
						valueTemp := is.newTemp()
						is.emit(OpLoad, valueTemp, srcOp, nil)
						
						// Store to our variable using the saved offset
						dstOp := &Operand{Type: "var", Value: node.VarName, Offset: varOffset + offset}
						is.emit(OpStore, dstOp, valueTemp, nil)
					}
				} else {
					// Regular initialization
					result, err := is.selectExpression(initExpr)
					if err != nil {
						return err
					}
					
					varOp := &Operand{Type: "var", Value: node.VarName, Offset: varOffset}
					is.emit(OpStore, varOp, result, nil)
				}
			}
		}
		
	case NodeReturn:
		if len(node.Children) > 0 {
			result, err := is.selectExpression(node.Children[0])
			if err != nil {
				return err
			}
			
			// Check if we're returning a large struct
			funcSig := is.functions[is.currentFunc]
			if funcSig != nil && funcSig.ReturnType != "" && is.isLargeStruct(funcSig.ReturnType) {
				// Large struct return: copy to hidden pointer location
				// The hidden pointer is saved in __retptr
				if retPtr, ok := is.localVars["__retptr"]; ok {
					// Load the hidden pointer
					ptrTemp := is.newTemp()
					ptrVar := &Operand{Type: "var", Value: "__retptr", Offset: retPtr.Offset}
					is.emit(OpLoad, ptrTemp, ptrVar, nil)
					
					// Copy the struct from result to the hidden pointer location
					// For now, we'll use a simple memcpy approach
					structSize := is.getTypeSize(funcSig.ReturnType)
					
					// If result is already a memory location, copy from it
					if result.Type == "mem" || result.Type == "var" {
						// Generate copy loop - for simplicity, copy 8 bytes at a time
						for offset := 0; offset < structSize; offset += 8 {
							srcTemp := is.newTemp()
							srcOp := &Operand{
								Type:   "mem",
								Offset: result.Offset + offset,
							}
							is.emit(OpLoad, srcTemp, srcOp, nil)
							
							// Store to hidden pointer + offset
							dstOp := &Operand{
								Type:      "ptr",
								IndexTemp: ptrTemp,
							}
							// Add offset if needed
							if offset > 0 {
								offsetOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", offset)}
								addrTemp := is.newTemp()
								is.emit(OpAdd, addrTemp, ptrTemp, offsetOp)
								dstOp.IndexTemp = addrTemp
							}
							is.emit(OpStore, dstOp, srcTemp, nil)
						}
					}
					
					// Return the hidden pointer in RAX
					retReg := &Operand{Type: "reg", Value: "rax"}
					is.emit(OpMov, retReg, ptrTemp, nil)
				}
			} else {
				// Regular return: move result to RAX
				retReg := &Operand{Type: "reg", Value: "rax"}
				is.emit(OpMov, retReg, result, nil)
			}
		}
		is.emit(OpRet, nil, nil, nil)
		
	case NodeIf:
		cond, err := is.selectExpression(node.Children[0])
		if err != nil {
			return err
		}
		
		elseLabel := is.newLabel(".L_else")
		endLabel := is.newLabel(".L_endif")
		
		is.emit(OpJz, &Operand{Type: "label", Value: elseLabel}, cond, nil)
		
		// Then branch
		if err := is.selectNode(node.Children[1]); err != nil {
			return err
		}
		is.emit(OpJmp, &Operand{Type: "label", Value: endLabel}, nil, nil)
		
		// Else branch
		is.emit(OpLabel, &Operand{Type: "label", Value: elseLabel}, nil, nil)
		if len(node.Children) > 2 {
			if err := is.selectNode(node.Children[2]); err != nil {
				return err
			}
		}
		
		is.emit(OpLabel, &Operand{Type: "label", Value: endLabel}, nil, nil)
		
	case NodeWhile:
		startLabel := is.newLabel(".L_while_start")
		endLabel := is.newLabel(".L_while_end")
		
		is.emit(OpLabel, &Operand{Type: "label", Value: startLabel}, nil, nil)
		
		cond, err := is.selectExpression(node.Children[0])
		if err != nil {
			return err
		}
		
		is.emit(OpJz, &Operand{Type: "label", Value: endLabel}, cond, nil)
		
		if err := is.selectNode(node.Children[1]); err != nil {
			return err
		}
		
		is.emit(OpJmp, &Operand{Type: "label", Value: startLabel}, nil, nil)
		is.emit(OpLabel, &Operand{Type: "label", Value: endLabel}, nil, nil)
		
	case NodeFor:
		// Parse for loop structure
		idx := 0
		var init, cond, incr, body *ASTNode
		
		if idx < len(node.Children) {
			// Determine what we have
			if node.Children[idx].Type == NodeVarDecl || node.Children[idx].Type == NodeExprStmt {
				init = node.Children[idx]
				idx++
			}
		}
		
		if idx < len(node.Children) && (node.Children[idx].Type == NodeBinaryOp || 
			node.Children[idx].Type == NodeIdentifier || node.Children[idx].Type == NodeNumber) {
			cond = node.Children[idx]
			idx++
		}
		
		if idx < len(node.Children) && (node.Children[idx].Type == NodeBinaryOp || 
			node.Children[idx].Type == NodeAssignment || node.Children[idx].Type == NodeUnaryOp) {
			incr = node.Children[idx]
			idx++
		}
		
		if idx < len(node.Children) {
			body = node.Children[idx]
		}
		
		// Generate code
		if init != nil {
			is.selectNode(init)
		}
		
		startLabel := is.newLabel(".L_for_start")
		endLabel := is.newLabel(".L_for_end")
		
		is.emit(OpLabel, &Operand{Type: "label", Value: startLabel}, nil, nil)
		
		if cond != nil {
			condResult, err := is.selectExpression(cond)
			if err != nil {
				return err
			}
			is.emit(OpJz, &Operand{Type: "label", Value: endLabel}, condResult, nil)
		}
		
		if body != nil {
			is.selectNode(body)
		}
		
		if incr != nil {
			is.selectExpression(incr)
		}
		
		is.emit(OpJmp, &Operand{Type: "label", Value: startLabel}, nil, nil)
		is.emit(OpLabel, &Operand{Type: "label", Value: endLabel}, nil, nil)
		
	case NodeBlock:
		for _, stmt := range node.Children {
			if err := is.selectNode(stmt); err != nil {
				return err
			}
		}
		
	case NodeSwitch:
		// switch (expr) { case val1: ... case val2: ... default: ... }
		if len(node.Children) < 1 {
			return fmt.Errorf("switch needs expression")
		}
		
		// Evaluate switch expression
		switchExpr, err := is.selectExpression(node.Children[0])
		if err != nil {
			return err
		}
		
		endLabel := is.newLabel(".L_switch_end")
		
		// Process each case
		for i := 1; i < len(node.Children); i++ {
			caseNode := node.Children[i]
			if caseNode.Type != NodeCase {
				continue
			}
			
			// Check if this is default case
			if caseNode.Value == "default" {
				// Default case - just execute statements
				for j := 0; j < len(caseNode.Children); j++ {
					if err := is.selectNode(caseNode.Children[j]); err != nil {
						return err
					}
				}
				continue
			}
			
			// Regular case - first child is the value, rest are statements
			if len(caseNode.Children) < 1 {
				continue
			}
			
			// Generate case label
			caseLabel := is.newLabel(".L_case")
			nextCaseLabel := is.newLabel(".L_case_next")
			
			// Compare with case value
			caseValue, err := is.selectExpression(caseNode.Children[0])
			if err != nil {
				return err
			}
			
			cmp := is.newTemp()
			is.emit(OpEq, cmp, switchExpr, caseValue)
			is.emit(OpJz, &Operand{Type: "label", Value: nextCaseLabel}, cmp, nil)
			
			// Case body
			is.emit(OpLabel, &Operand{Type: "label", Value: caseLabel}, nil, nil)
			for j := 1; j < len(caseNode.Children); j++ {
				stmt := caseNode.Children[j]
				if stmt.Type == NodeBreak {
					is.emit(OpJmp, &Operand{Type: "label", Value: endLabel}, nil, nil)
					break
				}
				if err := is.selectNode(stmt); err != nil {
					return err
				}
			}
			
			// Next case label
			is.emit(OpLabel, &Operand{Type: "label", Value: nextCaseLabel}, nil, nil)
		}
		
		is.emit(OpLabel, &Operand{Type: "label", Value: endLabel}, nil, nil)
		
	case NodeExprStmt:
		if len(node.Children) > 0 {
			_, err := is.selectExpression(node.Children[0])
			return err
		}
		
	case NodeBreak:
		// Break is handled inside switch/while/for
		return nil
		
	case NodeContinue:
		// Continue is handled inside loops
		return nil
		
	default:
		// Expression as statement
		_, err := is.selectExpression(node)
		return err
	}
	
	return nil
}

func (is *InstructionSelector) selectExpression(node *ASTNode) (*Operand, error) {
	if node == nil {
		return nil, nil
	}
	
	switch node.Type {
	case NodeNumber:
		return &Operand{Type: "imm", Value: node.Value}, nil
		
	case NodeString:
		label := is.newLabel(".str")
		is.stringLits[label] = node.Value
		return &Operand{Type: "label", Value: label}, nil
		
	case NodeIdentifier:
		// Check for enum constants first
		if val, ok := is.enums[node.VarName]; ok {
			return &Operand{Type: "imm", Value: fmt.Sprintf("%d", val)}, nil
		}
		
		if sym, ok := is.localVars[node.VarName]; ok {
			temp := is.newTemp()
			temp.DataType = sym.Type
			varOp := &Operand{Type: "var", Value: node.VarName, Offset: sym.Offset}
			is.emit(OpLoad, temp, varOp, nil)
			return temp, nil
		} else if sym, ok := is.globalVars[node.VarName]; ok {
			temp := is.newTemp()
			temp.DataType = sym.Type
			varOp := &Operand{Type: "var", Value: node.VarName, IsGlobal: true}
			is.emit(OpLoad, temp, varOp, nil)
			return temp, nil
		} else if _, ok := is.functions[node.VarName]; ok {
			// Function name used as value (function pointer)
			// Return a label operand representing the function address
			return &Operand{Type: "label", Value: node.VarName}, nil
		}
		return nil, fmt.Errorf("undefined variable: %s (in function: %s)", node.VarName, is.currentFunc)
		
	case NodeBinaryOp:
		left, err := is.selectExpression(node.Children[0])
		if err != nil {
			return nil, err
		}
		
		right, err := is.selectExpression(node.Children[1])
		if err != nil {
			return nil, err
		}
		
		result := is.newTemp()
		
		switch node.Operator {
		case "+":
			is.emit(OpAdd, result, left, right)
		case "-":
			is.emit(OpSub, result, left, right)
		case "*":
			is.emit(OpMul, result, left, right)
		case "/":
			is.emit(OpDiv, result, left, right)
		case "%":
			is.emit(OpMod, result, left, right)
		case "&":
			is.emit(OpAnd, result, left, right)
		case "|":
			is.emit(OpOr, result, left, right)
		case "^":
			is.emit(OpXor, result, left, right)
		case "<<":
			is.emit(OpShl, result, left, right)
		case ">>":
			is.emit(OpShr, result, left, right)
		case "==":
			is.emit(OpEq, result, left, right)
		case "!=":
			is.emit(OpNe, result, left, right)
		case "<":
			is.emit(OpLt, result, left, right)
		case "<=":
			is.emit(OpLe, result, left, right)
		case ">":
			is.emit(OpGt, result, left, right)
		case ">=":
			is.emit(OpGe, result, left, right)
		case "&&":
			// Short-circuit AND
			endLabel := is.newLabel(".L_and_end")
			is.emit(OpJz, &Operand{Type: "label", Value: endLabel}, left, nil)
			is.emit(OpMov, result, right, nil)
			is.emit(OpLabel, &Operand{Type: "label", Value: endLabel}, nil, nil)
		case "||":
			// Short-circuit OR
			endLabel := is.newLabel(".L_or_end")
			is.emit(OpJnz, &Operand{Type: "label", Value: endLabel}, left, nil)
			is.emit(OpMov, result, right, nil)
			is.emit(OpLabel, &Operand{Type: "label", Value: endLabel}, nil, nil)
		default:
			return nil, fmt.Errorf("unknown binary operator: %s", node.Operator)
		}
		
		return result, nil
		
	case NodeUnaryOp:
		operand, err := is.selectExpression(node.Children[0])
		if err != nil {
			return nil, err
		}
		
		result := is.newTemp()
		
		switch node.Operator {
		case "-":
			is.emit(OpNeg, result, operand, nil)
		case "!":
			is.emit(OpNot, result, operand, nil)
		case "~":
			// Bitwise NOT
			allOnes := &Operand{Type: "imm", Value: "-1"}
			is.emit(OpXor, result, operand, allOnes)
		case "++":
			// Pre-increment
			one := &Operand{Type: "imm", Value: "1"}
			is.emit(OpAdd, operand, operand, one)
			is.emit(OpMov, result, operand, nil)
		case "--":
			// Pre-decrement
			one := &Operand{Type: "imm", Value: "1"}
			is.emit(OpSub, operand, operand, one)
			is.emit(OpMov, result, operand, nil)
		case "++_post":
			// Post-increment
			is.emit(OpMov, result, operand, nil)
			one := &Operand{Type: "imm", Value: "1"}
			is.emit(OpAdd, operand, operand, one)
		case "--_post":
			// Post-decrement
			is.emit(OpMov, result, operand, nil)
			one := &Operand{Type: "imm", Value: "1"}
			is.emit(OpSub, operand, operand, one)
		case "&":
			// Address-of operator
			if node.Children[0].Type != NodeIdentifier {
				return nil, fmt.Errorf("& operator requires identifier")
			}
			varName := node.Children[0].VarName
			if sym, ok := is.localVars[varName]; ok {
				// Return address of local variable (rbp + offset)
				result.Type = "addr"
				result.Value = varName
				result.Offset = sym.Offset
			} else if _, ok := is.globalVars[varName]; ok {
				result.Type = "addr"
				result.Value = varName
				result.IsGlobal = true
			} else {
				return nil, fmt.Errorf("undefined variable: %s", varName)
			}
			return result, nil
		case "*":
			// Dereference operator - load from pointer
			// operand contains the address, load from it
			is.emit(OpLoad, result, &Operand{Type: "ptr", Value: operand.Value, IndexTemp: operand}, nil)
			
			// If operand has type info like "Type*", result should be "Type"
			if operand.DataType != "" && strings.HasSuffix(operand.DataType, "*") {
				result.DataType = strings.TrimSpace(operand.DataType[:len(operand.DataType)-1])
			}
			return result, nil
		default:
			return nil, fmt.Errorf("unknown unary operator: %s", node.Operator)
		}
		
		return result, nil
		
	case NodeArrayAccess:
		// arr[index] - compute address and load
		if len(node.Children) < 2 {
			return nil, fmt.Errorf("array access needs 2 operands")
		}
		
		// Get base - can be an identifier, member access, or any pointer expression
		baseNode := node.Children[0]
		
		// Get index
		index, err := is.selectExpression(node.Children[1])
		if err != nil {
			return nil, err
		}
		
		// Check if base is a simple identifier (local/global array)
		if baseNode.Type == NodeIdentifier {
			varName := baseNode.VarName
			var baseOffset int
			var isGlobal bool
			var varType string
			
			if sym, ok := is.localVars[varName]; ok {
				baseOffset = sym.Offset
				isGlobal = false
				varType = sym.Type
			} else if sym, ok := is.globalVars[varName]; ok {
				baseOffset = 0
				isGlobal = true
				varType = sym.Type
			} else {
				return nil, fmt.Errorf("undefined array: %s", varName)
			}
			
			// Determine element type and size
			var elementType string
			var elementSize int
			
			if strings.Contains(varType, "*") {
				// Pointer type - element is what it points to
				elementType = strings.TrimSuffix(strings.TrimSpace(varType), "*")
				elementSize = is.getTypeSize(elementType)
			} else {
				// Array type - for now assume 8-byte elements
				elementType = varType
				elementSize = 8
			}
			
			// Calculate byte offset: index * elementSize
			elementSizeOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", elementSize)}
			byteOffset := is.newTemp()
			is.emit(OpMul, byteOffset, index, elementSizeOp)
			
			// Check if the variable is a pointer type
			// Pointers need to be dereferenced, not accessed as arrays
			if strings.Contains(varType, "*") {
				// It's a pointer - load the pointer value first, then index it
				baseAddr, err := is.selectExpression(baseNode)
				if err != nil {
					return nil, err
				}
				
				// Add base address + offset
				finalAddr := is.newTemp()
				is.emit(OpAdd, finalAddr, baseAddr, byteOffset)
				
				// Load from the computed address
				result := is.newTemp()
				result.DataType = elementType
				ptrOp := &Operand{
					Type:      "ptr",
					IndexTemp: finalAddr,
					DataType:  elementType,
				}
				is.emit(OpLoad, result, ptrOp, nil)
				return result, nil
			}
			
			// Use the optimized array access path for actual arrays
			result := is.newTemp()
			arrayOp := &Operand{
				Type:      "array",
				Value:     varName,
				Offset:    baseOffset,
				IsGlobal:  isGlobal,
				IndexTemp: byteOffset,
			}
			is.emit(OpLoad, result, arrayOp, nil)
			return result, nil
		} else {
			// Base is a complex expression (member access, pointer, etc.)
			// Evaluate it to get the pointer/array address
			baseAddr, err := is.selectExpression(baseNode)
			if err != nil {
				return nil, err
			}
			
			// For complex expressions, assume 8-byte elements for now
			// TODO: Determine actual element size from baseAddr type
			elementSizeOp := &Operand{Type: "imm", Value: "8"}
			byteOffset := is.newTemp()
			is.emit(OpMul, byteOffset, index, elementSizeOp)
			
			// Add base address + offset
			finalAddr := is.newTemp()
			is.emit(OpAdd, finalAddr, baseAddr, byteOffset)
			
			// Load from the computed address
			result := is.newTemp()
			ptrOp := &Operand{
				Type:      "ptr",
				IndexTemp: finalAddr,
			}
			is.emit(OpLoad, result, ptrOp, nil)
			return result, nil
		}
		
	case NodeMemberAccess:
		// struct.member or ptr->member
		if len(node.Children) < 1 {
			return nil, fmt.Errorf("member access needs base")
		}
		
		baseNode := node.Children[0]
		memberName := node.MemberName
		isPtr := node.IsPointer  // true for -> operator
		
		var baseTemp *Operand
		var structType string
		
		// Handle different base node types
		if baseNode.Type == NodeIdentifier {
			// Simple case: variable.member or variable->member
			varName := baseNode.VarName
			var baseOffset int
			var isGlobal bool
			
			// Look up variable
			if sym, ok := is.localVars[varName]; ok {
				baseOffset = sym.Offset
				isGlobal = false
				structType = sym.Type
			} else if sym, ok := is.globalVars[varName]; ok {
				baseOffset = 0
				isGlobal = true
				structType = sym.Type
			} else {
				return nil, fmt.Errorf("undefined variable: %s", varName)
			}
			
			// Create temp for base address
			baseTemp = &Operand{Type: "var", Value: varName, Offset: baseOffset, IsGlobal: isGlobal, DataType: structType}
		} else if !isPtr && baseNode.Type == NodeUnaryOp && baseNode.Operator == "*" {
			// Special case: (*ptr).member should be treated as ptr->member
			// Evaluate the pointer expression
			baseTempVal, err := is.selectExpression(baseNode.Children[0])
			if err != nil {
				return nil, err
			}
			baseTemp = baseTempVal
			structType = baseTemp.DataType
			if structType == "" {
				structType = baseNode.Children[0].DataType
			}
			// Treat it as pointer access
			isPtr = true
		} else {
			// Complex expression as base (e.g., (cast)->member, deref->member)
			// Evaluate the base expression
			baseTempVal, err := is.selectExpression(baseNode)
			if err != nil {
				return nil, err
			}
			baseTemp = baseTempVal
			
			// Get type from base operand first (set by cast/dereference)
			structType = baseTemp.DataType
			
			// If still empty, try to get from base node
			if structType == "" {
				structType = baseNode.DataType
			}
			
			// If type is still empty, try to infer from structure
			if structType == "" {
				// Check if it's a dereference of a typed expression
				if baseNode.Type == NodeUnaryOp && baseNode.Operator == "*" && len(baseNode.Children) > 0 {
					// Get type from the dereferenced expression
					innerType := baseNode.Children[0].DataType
					// Remove one level of pointer
					if strings.HasSuffix(innerType, "*") {
						structType = innerType[:len(innerType)-1]
						structType = strings.TrimSpace(structType)
					}
				}
			}
			
			if structType == "" {
				return nil, fmt.Errorf("member access on expression with unknown type")
			}
		}
		
		// Resolve typedef aliases to actual struct types
		structType = is.resolveType(structType)
		
		// Extract struct name from type (e.g., "struct Point*" -> "Point")
		structName := structType
		origStructType := structType  // Save for error reporting
		
		// Strip pointers
		for len(structName) > 0 && structName[len(structName)-1] == '*' {
			structName = structName[:len(structName)-1]
		}
		structName = strings.TrimSpace(structName)
		
		// Strip "struct " or "union " prefix
		if len(structName) > 7 && structName[:7] == "struct " {
			structName = structName[7:]
		} else if len(structName) > 6 && structName[:6] == "union " {
			structName = structName[6:]
		}
		structName = strings.TrimSpace(structName)
		
		// Find struct definition
		structDef, ok := is.structs[structName]
		if !ok {
			// Better error message for debugging
			if origStructType == "" && structName == "" {
				return nil, fmt.Errorf("member access '%s' on expression with no type information (base type: %v, base node type: %d)", memberName, baseTemp, baseNode.Type)
			}
			return nil, fmt.Errorf("undefined struct: '%s' (from type: '%s')", structName, origStructType)
		}
		
		// Find member offset and size
		memberOffset := -1
		memberSize := 8  // Default
		for _, member := range structDef.Members {
			if member.Name == memberName {
				memberOffset = member.Offset
				memberSize = member.Size
				break
			}
		}
		
		if memberOffset == -1 {
			return nil, fmt.Errorf("struct %s has no member %s", structName, memberName)
		}
		
		// Load member value
		result := is.newTemp()
		if isPtr {
			// ptr->member: load pointer value, then load from (ptr + memberOffset)
			var ptrTemp *Operand
			
			if baseTemp.Type == "var" {
				// Load the pointer from variable
				ptrTempReg := is.newTemp()
				is.emit(OpLoad, ptrTempReg, baseTemp, nil)
				ptrTemp = ptrTempReg
			} else {
				// Base is already a value (temp/reg)
				ptrTemp = baseTemp
			}
			
			// Add member offset to pointer
			if memberOffset != 0 {
				offsetOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", memberOffset)}
				ptrWithOffset := is.newTemp()
				is.emit(OpAdd, ptrWithOffset, ptrTemp, offsetOp)
				ptrTemp = ptrWithOffset
			}
			
			// Load from pointer with correct size
			memberOp := &Operand{Type: "ptr", IndexTemp: ptrTemp, Size: memberSize}
			is.emit(OpLoad, result, memberOp, nil)
		} else {
			// struct.member: direct access
			// This only works for simple variable bases
			if baseTemp.Type == "var" {
				finalOffset := baseTemp.Offset + memberOffset
				memberOp := &Operand{Type: "var", Value: baseTemp.Value, Offset: finalOffset, IsGlobal: baseTemp.IsGlobal, Size: memberSize}
				is.emit(OpLoad, result, memberOp, nil)
			} else if baseTemp.Type == "temp" {
				// Temp holds a struct value (from statement expression or function return)
				// Treat the temp as a pointer to the struct and load the member
				// Add member offset to the temp pointer
				var ptrTemp *Operand
				if memberOffset != 0 {
					offsetOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", memberOffset)}
					ptrWithOffset := is.newTemp()
					is.emit(OpAdd, ptrWithOffset, baseTemp, offsetOp)
					ptrTemp = ptrWithOffset
				} else {
					ptrTemp = baseTemp
				}
				
				// Load from pointer
				memberOp := &Operand{Type: "ptr", IndexTemp: ptrTemp}
				is.emit(OpLoad, result, memberOp, nil)
			} else {
				return nil, fmt.Errorf("dot access on non-variable expression not yet supported (in function: %s, member: %s, baseType: %s)", 
					is.currentFunc, memberName, baseTemp.Type)
			}
		}
		
		return result, nil
		
	case NodeAssignment:
		// Expand compound assignments to regular assignments
		// e.g., x += 5 becomes x = x + 5
		if node.Operator != "=" {
			oldValue, err := is.selectExpression(node.Children[0])
			if err != nil {
				return nil, err
			}
			
			rightValue, err := is.selectExpression(node.Children[1])
			if err != nil {
				return nil, err
			}
			
			temp := is.newTemp()
			switch node.Operator {
			case "+=":
				is.emit(OpAdd, temp, oldValue, rightValue)
			case "-=":
				is.emit(OpSub, temp, oldValue, rightValue)
			case "*=":
				is.emit(OpMul, temp, oldValue, rightValue)
			case "/=":
				is.emit(OpDiv, temp, oldValue, rightValue)
			case "%=":
				is.emit(OpMod, temp, oldValue, rightValue)
			case "&=":
				is.emit(OpAnd, temp, oldValue, rightValue)
			case "|=":
				is.emit(OpOr, temp, oldValue, rightValue)
			case "^=":
				is.emit(OpXor, temp, oldValue, rightValue)
			case "<<=":
				is.emit(OpShl, temp, oldValue, rightValue)
			case ">>=":
				is.emit(OpShr, temp, oldValue, rightValue)
			default:
				return nil, fmt.Errorf("unsupported compound assignment: %s", node.Operator)
			}
			
			// Replace the right side with the computed value
			node.Children[1] = &ASTNode{
				Type: NodeIdentifier, // Placeholder - will use temp operand
			}
			// Update operator to simple assignment
			node.Operator = "="
			// Update right side value
			node.Children[1] = &ASTNode{
				Type: NodeNumber,
				Value: "", // Will be replaced by temp below
			}
			// Store the temp as the value to assign
			// Fall through to regular assignment handling with temp as the value
			
			// Continue with normal assignment, but using temp as value
			var assignValue = temp
			
			// Now handle the assignment based on lvalue type
			if node.Children[0].Type == NodeArrayAccess {
				arrayNode := node.Children[0]
				baseNode := arrayNode.Children[0]
				
				// Get index
				index, err := is.selectExpression(arrayNode.Children[1])
				if err != nil {
					return nil, err
				}
				
				// Calculate byte offset: index * 8
				elementSize := &Operand{Type: "imm", Value: "8"}
				byteOffset := is.newTemp()
				is.emit(OpMul, byteOffset, index, elementSize)
				
				// Check if base is a simple identifier (local/global array)
				if baseNode.Type == NodeIdentifier {
					varName := baseNode.VarName
					var baseOffset int
					var isGlobal bool
					
					if sym, ok := is.localVars[varName]; ok {
						baseOffset = sym.Offset
						isGlobal = false
					} else if _, ok := is.globalVars[varName]; ok {
						baseOffset = 0
						isGlobal = true
					} else {
						return nil, fmt.Errorf("undefined array: %s", varName)
					}
					
					arrayOp := &Operand{
						Type:      "array",
						Value:     varName,
						Offset:    baseOffset,
						IsGlobal:  isGlobal,
						IndexTemp: byteOffset,
					}
					is.emit(OpStore, arrayOp, assignValue, nil)
				} else {
					baseAddr, err := is.selectExpression(baseNode)
					if err != nil {
						return nil, err
					}
					
					finalAddr := is.newTemp()
					is.emit(OpAdd, finalAddr, baseAddr, byteOffset)
					
					ptrOp := &Operand{
						Type:      "ptr",
						IndexTemp: finalAddr,
					}
					is.emit(OpStore, ptrOp, assignValue, nil)
				}
				
				return assignValue, nil
			}
			
			// Handle member access compound assignment
			if node.Children[0].Type == NodeMemberAccess {
				memberNode := node.Children[0]
				baseNode := memberNode.Children[0]
				memberName := memberNode.MemberName
				isPtr := memberNode.IsPointer
				
				var structType string
				var baseTemp *Operand
				
				if baseNode.Type == NodeIdentifier {
					varName := baseNode.VarName
					var baseOffset int
					var isGlobal bool
					
					if sym, ok := is.localVars[varName]; ok {
						baseOffset = sym.Offset
						isGlobal = false
						structType = sym.Type
					} else if sym, ok := is.globalVars[varName]; ok {
						baseOffset = 0
						isGlobal = true
						structType = sym.Type
					} else {
						return nil, fmt.Errorf("undefined variable: %s", varName)
					}
					
					baseTemp = &Operand{Type: "var", Value: varName, Offset: baseOffset, IsGlobal: isGlobal}
				} else {
					baseTempVal, err := is.selectExpression(baseNode)
					if err != nil {
						return nil, err
					}
					baseTemp = baseTempVal
					structType = baseTemp.DataType
					if structType == "" {
						structType = baseNode.DataType
					}
				}
				
				structType = is.resolveType(structType)
				structName := structType
				for len(structName) > 0 && structName[len(structName)-1] == '*' {
					structName = structName[:len(structName)-1]
				}
				structName = strings.TrimSpace(structName)
				
				if len(structName) > 7 && structName[:7] == "struct " {
					structName = structName[7:]
				} else if len(structName) > 6 && structName[:6] == "union " {
					structName = structName[6:]
				}
				structName = strings.TrimSpace(structName)
				
				structDef, ok := is.structs[structName]
				if !ok {
					return nil, fmt.Errorf("undefined struct: %s", structName)
				}
				
				memberOffset := -1
				for _, member := range structDef.Members {
					if member.Name == memberName {
						memberOffset = member.Offset
						break
					}
				}
				
				if memberOffset == -1 {
					return nil, fmt.Errorf("struct %s has no member %s", structName, memberName)
				}
				
				if isPtr {
					var ptrTemp *Operand
					
					if baseTemp.Type == "var" {
						ptrTempReg := is.newTemp()
						is.emit(OpLoad, ptrTempReg, baseTemp, nil)
						ptrTemp = ptrTempReg
					} else {
						ptrTemp = baseTemp
					}
					
					if memberOffset != 0 {
						offsetOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", memberOffset)}
						ptrWithOffset := is.newTemp()
						is.emit(OpAdd, ptrWithOffset, ptrTemp, offsetOp)
						ptrTemp = ptrWithOffset
					}
					
					memberOp := &Operand{Type: "ptr", IndexTemp: ptrTemp}
					is.emit(OpStore, memberOp, assignValue, nil)
				} else {
					if baseTemp.Type == "var" {
						finalOffset := baseTemp.Offset + memberOffset
						memberOp := &Operand{Type: "var", Value: baseTemp.Value, Offset: finalOffset, IsGlobal: baseTemp.IsGlobal}
						is.emit(OpStore, memberOp, assignValue, nil)
						return assignValue, nil
					} else {
						return nil, fmt.Errorf("dot access on complex expression for assignment not yet supported")
					}
				}
				
				return assignValue, nil
			}
			
			// Handle dereference compound assignment
			if node.Children[0].Type == NodeUnaryOp && node.Children[0].Operator == "*" {
				ptrExpr, err := is.selectExpression(node.Children[0].Children[0])
				if err != nil {
					return nil, err
				}
				
				ptrOp := &Operand{Type: "ptr", IndexTemp: ptrExpr}
				is.emit(OpStore, ptrOp, assignValue, nil)
				return assignValue, nil
			}
			
			// Handle regular variable compound assignment
			if node.Children[0].Type == NodeIdentifier {
				varName := node.Children[0].VarName
				
				if sym, ok := is.localVars[varName]; ok {
					varOp := &Operand{Type: "var", Value: varName, Offset: sym.Offset}
					is.emit(OpStore, varOp, assignValue, nil)
				} else if _, ok := is.globalVars[varName]; ok {
					varOp := &Operand{Type: "var", Value: varName, IsGlobal: true}
					is.emit(OpStore, varOp, assignValue, nil)
				}
				
				return assignValue, nil
			}
			
			return nil, fmt.Errorf("invalid compound assignment target")
		}
		
		// Handle array assignment: arr[i] = value or expr[i] = value
		if node.Children[0].Type == NodeArrayAccess {
			arrayNode := node.Children[0]
			baseNode := arrayNode.Children[0]
			
			// Get index
			index, err := is.selectExpression(arrayNode.Children[1])
			if err != nil {
				return nil, err
			}
			
			// Get value to store
			value, err := is.selectExpression(node.Children[1])
			if err != nil {
				return nil, err
			}
			
			// Calculate byte offset: index * 8
			elementSize := &Operand{Type: "imm", Value: "8"}
			byteOffset := is.newTemp()
			is.emit(OpMul, byteOffset, index, elementSize)
			
			// Check if base is a simple identifier (local/global array)
			if baseNode.Type == NodeIdentifier {
				varName := baseNode.VarName
				var baseOffset int
				var isGlobal bool
				
				if sym, ok := is.localVars[varName]; ok {
					baseOffset = sym.Offset
					isGlobal = false
				} else if _, ok := is.globalVars[varName]; ok {
					baseOffset = 0
					isGlobal = true
				} else {
					return nil, fmt.Errorf("undefined array: %s", varName)
				}
				
				// Use the optimized array access path
				arrayOp := &Operand{
					Type:      "array",
					Value:     varName,
					Offset:    baseOffset,
					IsGlobal:  isGlobal,
					IndexTemp: byteOffset,
				}
				is.emit(OpStore, arrayOp, value, nil)
			} else {
				// Base is a complex expression (member access, pointer, etc.)
				// Evaluate it to get the pointer/array address
				baseAddr, err := is.selectExpression(baseNode)
				if err != nil {
					return nil, err
				}
				
				// Add base address + offset
				finalAddr := is.newTemp()
				is.emit(OpAdd, finalAddr, baseAddr, byteOffset)
				
				// Store to the computed address
				ptrOp := &Operand{
					Type:      "ptr",
					IndexTemp: finalAddr,
				}
				is.emit(OpStore, ptrOp, value, nil)
			}
			
			return value, nil
		}
		
		// Handle member access assignment: struct.member = value or ptr->member = value
		if node.Children[0].Type == NodeMemberAccess {
			memberNode := node.Children[0]
			baseNode := memberNode.Children[0]
			memberName := memberNode.MemberName
			isPtr := memberNode.IsPointer
			
			var structType string
			var baseTemp *Operand
			
			// Handle base - can be identifier or complex expression
			if baseNode.Type == NodeIdentifier {
				varName := baseNode.VarName
				var baseOffset int
				var isGlobal bool
				
				// Look up variable
				if sym, ok := is.localVars[varName]; ok {
					baseOffset = sym.Offset
					isGlobal = false
					structType = sym.Type
				} else if sym, ok := is.globalVars[varName]; ok {
					baseOffset = 0
					isGlobal = true
					structType = sym.Type
				} else {
					return nil, fmt.Errorf("undefined variable: %s", varName)
				}
				
				baseTemp = &Operand{Type: "var", Value: varName, Offset: baseOffset, IsGlobal: isGlobal}
			} else {
				// Complex expression - evaluate it
				baseTempVal, err := is.selectExpression(baseNode)
				if err != nil {
					return nil, err
				}
				baseTemp = baseTempVal
				structType = baseTemp.DataType
				if structType == "" {
					structType = baseNode.DataType
				}
			}
			
			// Resolve typedef and get struct name
			structType = is.resolveType(structType)
			
			// Extract struct name
			structName := structType
			// Strip pointers
			for len(structName) > 0 && structName[len(structName)-1] == '*' {
				structName = structName[:len(structName)-1]
			}
			structName = strings.TrimSpace(structName)
			
			if len(structName) > 7 && structName[:7] == "struct " {
				structName = structName[7:]
			} else if len(structName) > 6 && structName[:6] == "union " {
				structName = structName[6:]
			}
			structName = strings.TrimSpace(structName)
			
			// Find struct definition
			structDef, ok := is.structs[structName]
			if !ok {
				return nil, fmt.Errorf("undefined struct: %s", structName)
			}
			
			// Find member offset
			memberOffset := -1
			for _, member := range structDef.Members {
				if member.Name == memberName {
					memberOffset = member.Offset
					break
				}
			}
			
			if memberOffset == -1 {
				return nil, fmt.Errorf("struct %s has no member %s", structName, memberName)
			}
			
			// Get value to store
			value, err := is.selectExpression(node.Children[1])
			if err != nil {
				return nil, err
			}
			
			// Store to member
			if isPtr {
				// ptr->member: load pointer, add offset, store
				var ptrTemp *Operand
				
				if baseTemp.Type == "var" {
					// Load pointer from variable
					ptrTempReg := is.newTemp()
					is.emit(OpLoad, ptrTempReg, baseTemp, nil)
					ptrTemp = ptrTempReg
				} else {
					// Base is already a value
					ptrTemp = baseTemp
				}
				
				// Add member offset
				if memberOffset != 0 {
					offsetOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", memberOffset)}
					ptrWithOffset := is.newTemp()
					is.emit(OpAdd, ptrWithOffset, ptrTemp, offsetOp)
					ptrTemp = ptrWithOffset
				}
				
				// Store to pointer
				memberOp := &Operand{Type: "ptr", IndexTemp: ptrTemp}
				is.emit(OpStore, memberOp, value, nil)
			} else {
				// struct.member: direct access
				// Need to compute address of base, add member offset, and store
				
				if baseTemp.Type == "var" {
					// Simple variable
					finalOffset := baseTemp.Offset + memberOffset
					memberOp := &Operand{Type: "var", Value: baseTemp.Value, Offset: finalOffset, IsGlobal: baseTemp.IsGlobal}
					is.emit(OpStore, memberOp, value, nil)
					return value, nil
				} else {
					// Complex expression - need address
					// For now, treat as error - dot operator on complex expressions needs address-of support
					return nil, fmt.Errorf("dot access on complex expression for assignment not yet supported")
				}
			}
			
			return value, nil
		}
		
		// Dereference assignment: *ptr = value
		if node.Children[0].Type == NodeUnaryOp && node.Children[0].Operator == "*" {
			// Get the pointer expression
			ptrExpr, err := is.selectExpression(node.Children[0].Children[0])
			if err != nil {
				return nil, err
			}
			
			// Get value to store
			value, err := is.selectExpression(node.Children[1])
			if err != nil {
				return nil, err
			}
			
			// Store to pointer
			ptrOp := &Operand{Type: "ptr", IndexTemp: ptrExpr}
			is.emit(OpStore, ptrOp, value, nil)
			return value, nil
		}
		
		// Regular variable assignment
		if node.Children[0].Type != NodeIdentifier {
			return nil, fmt.Errorf("invalid assignment target: type=%d, operator=%s (in function: %s)", 
				node.Children[0].Type, node.Operator, is.currentFunc)
		}
		
		value, err := is.selectExpression(node.Children[1])
		if err != nil {
			return nil, err
		}
		
		varName := node.Children[0].VarName
		
		if sym, ok := is.localVars[varName]; ok {
			varOp := &Operand{Type: "var", Value: varName, Offset: sym.Offset}
			is.emit(OpStore, varOp, value, nil)
		} else if _, ok := is.globalVars[varName]; ok {
			varOp := &Operand{Type: "var", Value: varName, IsGlobal: true}
			is.emit(OpStore, varOp, value, nil)
		}
		
		return value, nil
		
	case NodeCall:
		// Check if this function returns a large struct
		var returnType string
		if funcSig, ok := is.functions[node.Name]; ok {
			returnType = funcSig.ReturnType
		}
		
		// Evaluate arguments
		args := []*Operand{}
		for _, argNode := range node.Children {
			arg, err := is.selectExpression(argNode)
			if err != nil {
				return nil, err
			}
			args = append(args, arg)
		}
		
		// Check if we need to allocate space for a large struct return
		var retSlot *Operand
		argRegs := []string{"rdi", "rsi", "rdx", "rcx", "r8", "r9"}
		argStartIdx := 0
		
		if returnType != "" && is.isLargeStruct(returnType) {
			// Allocate space for return value on stack
			structSize := is.getTypeSize(returnType)
			is.stackOffset -= structSize
			if is.stackOffset%16 != 0 {
				is.stackOffset -= is.stackOffset % 16
			}
			
			retSlotOffset := is.stackOffset
			retSlot = &Operand{
				Type:     "mem",  // Use "mem" type to prevent register allocation
				Offset:   retSlotOffset,
				DataType: returnType,
			}
			
			argStartIdx = 1 // Regular arguments start at rsi
		}
		
		// Move arguments to calling convention registers
		// Do this BEFORE OpLoadAddr to avoid register conflicts
		for i, arg := range args {
			regIdx := i + argStartIdx
			if regIdx < len(argRegs) {
				regOp := &Operand{Type: "reg", Value: argRegs[regIdx]}
				is.emit(OpMov, regOp, arg, nil)
			}
		}
		
		// NOW emit the hidden pointer load (after args are in place)
		if retSlot != nil {
			is.emit(OpLoadAddr, &Operand{Type: "reg", Value: "rdi"}, retSlot, nil)
		}
		
		// Call function
		result := is.newTemp()
		funcOp := &Operand{Type: "label", Value: node.Name}
		is.emit(OpCall, result, funcOp, &Operand{Type: "imm", Value: fmt.Sprintf("%d", len(args))})
		
		// If we used a return slot, the result is there, not in rax
		if retSlot != nil {
			result.DataType = returnType
			result.Offset = retSlot.Offset
			result.Type = "mem"
		} else if returnType != "" {
			// Check if this is a struct return that uses RAX+RDX (9-16 bytes)
			structSize := is.getTypeSize(returnType)
			if structSize > 8 && structSize <= 16 {
				// Struct is returned in RAX (first 8 bytes) + RDX (next 8 bytes)
				// We need to save both registers to memory
				is.stackOffset -= 16  // Allocate space for full struct
				if is.stackOffset%16 != 0 {
					is.stackOffset -= is.stackOffset % 16
				}
				
				// Save RAX (first 8 bytes)
				raxOp := &Operand{Type: "reg", Value: "rax"}
				firstPart := &Operand{Type: "mem", Offset: is.stackOffset}
				is.emit(OpStore, firstPart, raxOp, nil)
				
				// Save RDX (second 8 bytes)
				rdxOp := &Operand{Type: "reg", Value: "rdx"}
				secondPart := &Operand{Type: "mem", Offset: is.stackOffset + 8}
				is.emit(OpStore, secondPart, rdxOp, nil)
				
				// Result points to the combined struct on stack
				result.Type = "mem"
				result.Offset = is.stackOffset
				result.DataType = returnType
			}
		}
		
		return result, nil
		
	case NodeTernary:
		cond, err := is.selectExpression(node.Children[0])
		if err != nil {
			return nil, err
		}
		
		elseLabel := is.newLabel(".L_ternary_else")
		endLabel := is.newLabel(".L_ternary_end")
		result := is.newTemp()
		
		is.emit(OpJz, &Operand{Type: "label", Value: elseLabel}, cond, nil)
		
		thenVal, err := is.selectExpression(node.Children[1])
		if err != nil {
			return nil, err
		}
		is.emit(OpMov, result, thenVal, nil)
		is.emit(OpJmp, &Operand{Type: "label", Value: endLabel}, nil, nil)
		
		is.emit(OpLabel, &Operand{Type: "label", Value: elseLabel}, nil, nil)
		elseVal, err := is.selectExpression(node.Children[2])
		if err != nil {
			return nil, err
		}
		is.emit(OpMov, result, elseVal, nil)
		
		is.emit(OpLabel, &Operand{Type: "label", Value: endLabel}, nil, nil)
		
		return result, nil
		
	case NodeCompoundLiteral:
		// Create temporary struct and initialize fields
		// Extract struct name from type
		structType := node.DataType
		structName := structType
		// Strip pointers
		for len(structName) > 0 && structName[len(structName)-1] == '*' {
			structName = structName[:len(structName)-1]
		}
		structName = strings.TrimSpace(structName)
		
		if len(structName) > 7 && structName[:7] == "struct " {
			structName = structName[7:]
		} else if len(structName) > 6 && structName[:6] == "union " {
			structName = structName[6:]
		}
		structName = strings.TrimSpace(structName)
		
		// Find struct definition
		structDef, ok := is.structs[structName]
		if !ok {
			return nil, fmt.Errorf("undefined struct: %s", structName)
		}
		
		// Allocate temporary struct on stack
		tempName := is.newLabel(".compound_lit")
		is.stackOffset -= structDef.Size
		is.localVars[tempName] = &Symbol{
			Name:   tempName,
			Offset: is.stackOffset,
			Size:   structDef.Size,
			Type:   structType,
		}
		
		baseOffset := is.stackOffset
		
		// Initialize fields
		for i, fieldName := range node.InitFields {
			if i >= len(node.Children) {
				break
			}
			
			value, err := is.selectExpression(node.Children[i])
			if err != nil {
				return nil, err
			}
			
			// Find field offset and size
			var fieldOffset int
			var fieldSize int
			if fieldName == "" {
				// Positional - use index
				if i < len(structDef.Members) {
					fieldOffset = structDef.Members[i].Offset
					fieldSize = structDef.Members[i].Size
				} else {
					return nil, fmt.Errorf("too many initializers for struct %s", structName)
				}
			} else {
				// Named field
				found := false
				for _, member := range structDef.Members {
					if member.Name == fieldName {
						fieldOffset = member.Offset
						fieldSize = member.Size
						found = true
						break
					}
				}
				if !found {
					return nil, fmt.Errorf("struct %s has no member %s", structName, fieldName)
				}
			}
			
			// Store value to field with correct size
			finalOffset := baseOffset + fieldOffset
			fieldOp := &Operand{Type: "var", Value: tempName, Offset: finalOffset, Size: fieldSize}
			is.emit(OpStore, fieldOp, value, nil)
		}
		
		// Return address of temporary
		result := is.newTemp()
		addrOp := &Operand{Type: "addr", Value: tempName, Offset: baseOffset}
		is.emit(OpLoad, result, addrOp, nil)
		return result, nil
		
	case NodeBlock:
		// Statement expression: ({ stmts; expr; })
		// Execute all statements and return the last expression value
		var lastValue *Operand
		
		for _, stmt := range node.Children {
			if stmt.Type == NodeExprStmt && len(stmt.Children) > 0 {
				// Expression statement - evaluate it
				val, err := is.selectExpression(stmt.Children[0])
				if err != nil {
					return nil, err
				}
				lastValue = val
			} else {
				// Regular statement
				err := is.selectNode(stmt)
				if err != nil {
					return nil, err
				}
			}
		}
		
		if lastValue == nil {
			// No expression value, return 0
			lastValue = &Operand{Type: "imm", Value: "0"}
		}
		
		return lastValue, nil
		
	case NodeCast:
		// Type cast: (Type)expr
		if len(node.Children) < 1 {
			return nil, fmt.Errorf("cast needs operand")
		}
		result, err := is.selectExpression(node.Children[0])
		if err != nil {
			return nil, err
		}
		// Preserve the cast type information
		result.DataType = node.DataType
		return result, nil
		
	default:
		return nil, fmt.Errorf("unknown expression type: %d", node.Type)
	}
}
