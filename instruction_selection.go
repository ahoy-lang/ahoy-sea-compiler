package main

import "fmt"

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
	Type      string // "reg", "imm", "var", "label", "mem", "array"
	Value     string
	Offset    int
	IsGlobal  bool
	Size      int
	IndexTemp *Operand // For array indexing, holds the calculated offset
}

type IRInstruction struct {
	Op   OpCode
	Dst  *Operand
	Src1 *Operand
	Src2 *Operand
}

type InstructionSelector struct {
	instructions []*IRInstruction
	currentFunc  string
	labelCounter int
	tempCounter  int
	
	// Symbol tables
	localVars    map[string]*Symbol
	globalVars   map[string]*Symbol
	stringLits   map[string]string
	structs      map[string]*StructDef  // Struct definitions from parser
	typedefs     map[string]string      // Typedef aliases from parser
	
	stackOffset  int
}

func NewInstructionSelector() *InstructionSelector {
	is := &InstructionSelector{
		instructions: []*IRInstruction{},
		localVars:    make(map[string]*Symbol),
		globalVars:   make(map[string]*Symbol),
		stringLits:   make(map[string]string),
		structs:      make(map[string]*StructDef),
		typedefs:     make(map[string]string),
	}
	
	// Add standard library external symbols
	is.globalVars["stderr"] = &Symbol{
		Name:     "stderr",
		Type:     "void*",
		IsGlobal: true,
	}
	is.globalVars["stdout"] = &Symbol{
		Name:     "stdout",
		Type:     "void*",
		IsGlobal: true,
	}
	is.globalVars["stdin"] = &Symbol{
		Name:     "stdin",
		Type:     "void*",
		IsGlobal: true,
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
		
		// Allocate parameters
		argRegs := []string{"rdi", "rsi", "rdx", "rcx", "r8", "r9"}
		for i, param := range node.Params {
			is.stackOffset -= 8
			is.localVars[param] = &Symbol{
				Name:   param,
				Offset: is.stackOffset,
				Size:   8,
			}
			
			// Move from argument register to stack
			if i < len(argRegs) {
				argReg := &Operand{Type: "reg", Value: argRegs[i]}
				paramOp := &Operand{Type: "var", Value: param, Offset: is.stackOffset}
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
			is.localVars[node.VarName] = &Symbol{
				Name:      node.VarName,
				Offset:    is.stackOffset,
				Size:      varSize,
				ArraySize: node.ArraySize,
				Type:      dataType,
			}
			
			// Handle initialization (only for non-arrays for now)
			if len(node.Children) > 0 && node.ArraySize == 0 {
				result, err := is.selectExpression(node.Children[0])
				if err != nil {
					return err
				}
				
				varOp := &Operand{Type: "var", Value: node.VarName, Offset: is.stackOffset}
				is.emit(OpStore, varOp, result, nil)
			}
		}
		
	case NodeReturn:
		if len(node.Children) > 0 {
			result, err := is.selectExpression(node.Children[0])
			if err != nil {
				return err
			}
			retReg := &Operand{Type: "reg", Value: "rax"}
			is.emit(OpMov, retReg, result, nil)
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
		if sym, ok := is.localVars[node.VarName]; ok {
			temp := is.newTemp()
			varOp := &Operand{Type: "var", Value: node.VarName, Offset: sym.Offset}
			is.emit(OpLoad, temp, varOp, nil)
			return temp, nil
		} else if _, ok := is.globalVars[node.VarName]; ok {
			temp := is.newTemp()
			varOp := &Operand{Type: "var", Value: node.VarName, IsGlobal: true}
			is.emit(OpLoad, temp, varOp, nil)
			return temp, nil
		}
		return nil, fmt.Errorf("undefined variable: %s", node.VarName)
		
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
		
		// Get base variable
		if baseNode.Type != NodeIdentifier {
			return nil, fmt.Errorf("member access base must be identifier")
		}
		
		varName := baseNode.VarName
		var baseOffset int
		var isGlobal bool
		var structType string
		
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
		
		// Resolve typedef aliases to actual struct types
		structType = is.resolveType(structType)
		
		// Extract struct name from type (e.g., "struct Point*" -> "Point")
		structName := structType
		
		// Strip pointers
		for len(structName) > 0 && structName[len(structName)-1] == '*' {
			structName = structName[:len(structName)-1]
		}
		
		// Strip "struct " or "union " prefix
		if len(structName) > 7 && structName[:7] == "struct " {
			structName = structName[7:]
		} else if len(structName) > 6 && structName[:6] == "union " {
			structName = structName[6:]
		}
		
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
		
		// Calculate final offset: base + member_offset
		finalOffset := baseOffset + memberOffset
		
		// Load member value
		result := is.newTemp()
		if isPtr {
			// ptr->member: load pointer, then load from (ptr + offset)
			ptrTemp := is.newTemp()
			ptrOp := &Operand{Type: "var", Value: varName, Offset: baseOffset, IsGlobal: isGlobal}
			is.emit(OpLoad, ptrTemp, ptrOp, nil)
			
			// Add member offset to pointer
			if memberOffset != 0 {
				offsetOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", memberOffset)}
				ptrWithOffset := is.newTemp()
				is.emit(OpAdd, ptrWithOffset, ptrTemp, offsetOp)
				ptrTemp = ptrWithOffset
			}
			
			// Load from pointer
			memberOp := &Operand{Type: "ptr", IndexTemp: ptrTemp}
			is.emit(OpLoad, result, memberOp, nil)
		} else {
			// struct.member: direct access
			memberOp := &Operand{Type: "var", Value: varName, Offset: finalOffset, IsGlobal: isGlobal}
			is.emit(OpLoad, result, memberOp, nil)
		}
		
		return result, nil
		
	case NodeAssignment:
		// Handle array assignment: arr[i] = value
		if node.Children[0].Type == NodeArrayAccess {
			arrayNode := node.Children[0]
			baseNode := arrayNode.Children[0]
			
			if baseNode.Type != NodeIdentifier {
				return nil, fmt.Errorf("array base must be identifier")
			}
			
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
			
			// Calculate offset
			elementSize := &Operand{Type: "imm", Value: "8"}
			offsetTemp := is.newTemp()
			is.emit(OpMul, offsetTemp, index, elementSize)
			
			// Store to array[index]
			arrayOp := &Operand{
				Type:      "array",
				Value:     varName,
				Offset:    baseOffset,
				IsGlobal:  isGlobal,
				IndexTemp: offsetTemp,
			}
			is.emit(OpStore, arrayOp, value, nil)
			return value, nil
		}
		
		// Handle member access assignment: struct.member = value or ptr->member = value
		if node.Children[0].Type == NodeMemberAccess {
			memberNode := node.Children[0]
			baseNode := memberNode.Children[0]
			memberName := memberNode.MemberName
			isPtr := memberNode.IsPointer
			
			if baseNode.Type != NodeIdentifier {
				return nil, fmt.Errorf("member access base must be identifier")
			}
			
			varName := baseNode.VarName
			var baseOffset int
			var isGlobal bool
			var structType string
			
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
			
			// Extract struct name
			structName := structType
			if len(structType) > 7 && structType[:7] == "struct " {
				structName = structType[7:]
			} else if len(structType) > 6 && structType[:6] == "union " {
				structName = structType[6:]
			}
			
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
				ptrTemp := is.newTemp()
				ptrOp := &Operand{Type: "var", Value: varName, Offset: baseOffset, IsGlobal: isGlobal}
				is.emit(OpLoad, ptrTemp, ptrOp, nil)
				
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
				// struct.member: direct store
				finalOffset := baseOffset + memberOffset
				memberOp := &Operand{Type: "var", Value: varName, Offset: finalOffset, IsGlobal: isGlobal}
				is.emit(OpStore, memberOp, value, nil)
			}
			
			return value, nil
		}
		
		// Regular variable assignment
		if node.Children[0].Type != NodeIdentifier {
			return nil, fmt.Errorf("invalid assignment target")
		}
		
		value, err := is.selectExpression(node.Children[1])
		if err != nil {
			return nil, err
		}
		
		varName := node.Children[0].VarName
		
		if node.Operator != "=" {
			// Compound assignment
			oldValue, err := is.selectExpression(node.Children[0])
			if err != nil {
				return nil, err
			}
			
			temp := is.newTemp()
			switch node.Operator {
			case "+=":
				is.emit(OpAdd, temp, oldValue, value)
			case "-=":
				is.emit(OpSub, temp, oldValue, value)
			case "*=":
				is.emit(OpMul, temp, oldValue, value)
			case "/=":
				is.emit(OpDiv, temp, oldValue, value)
			}
			value = temp
		}
		
		if sym, ok := is.localVars[varName]; ok {
			varOp := &Operand{Type: "var", Value: varName, Offset: sym.Offset}
			is.emit(OpStore, varOp, value, nil)
		} else if _, ok := is.globalVars[varName]; ok {
			varOp := &Operand{Type: "var", Value: varName, IsGlobal: true}
			is.emit(OpStore, varOp, value, nil)
		}
		
		return value, nil
		
	case NodeCall:
		// Evaluate arguments
		args := []*Operand{}
		for _, argNode := range node.Children {
			arg, err := is.selectExpression(argNode)
			if err != nil {
				return nil, err
			}
			args = append(args, arg)
		}
		
		// Move arguments to calling convention registers
		argRegs := []string{"rdi", "rsi", "rdx", "rcx", "r8", "r9"}
		for i, arg := range args {
			if i < len(argRegs) {
				regOp := &Operand{Type: "reg", Value: argRegs[i]}
				is.emit(OpMov, regOp, arg, nil)
			}
		}
		
		// Call function
		result := is.newTemp()
		funcOp := &Operand{Type: "label", Value: node.Name}
		is.emit(OpCall, result, funcOp, &Operand{Type: "imm", Value: fmt.Sprintf("%d", len(args))})
		
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
		if len(structType) > 7 && structType[:7] == "struct " {
			structName = structType[7:]
		} else if len(structType) > 6 && structType[:6] == "union " {
			structName = structType[6:]
		}
		
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
			
			// Find field offset
			var fieldOffset int
			if fieldName == "" {
				// Positional - use index
				if i < len(structDef.Members) {
					fieldOffset = structDef.Members[i].Offset
				} else {
					return nil, fmt.Errorf("too many initializers for struct %s", structName)
				}
			} else {
				// Named field
				found := false
				for _, member := range structDef.Members {
					if member.Name == fieldName {
						fieldOffset = member.Offset
						found = true
						break
					}
				}
				if !found {
					return nil, fmt.Errorf("struct %s has no member %s", structName, fieldName)
				}
			}
			
			// Store value to field
			finalOffset := baseOffset + fieldOffset
			fieldOp := &Operand{Type: "var", Value: tempName, Offset: finalOffset}
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
		// For now, just evaluate the expression and ignore the cast
		// TODO: Handle different type sizes and conversions properly
		if len(node.Children) < 1 {
			return nil, fmt.Errorf("cast needs operand")
		}
		return is.selectExpression(node.Children[0])
		
	default:
		return nil, fmt.Errorf("unknown expression type: %d", node.Type)
	}
}
