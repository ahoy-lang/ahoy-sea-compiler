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
	OpMovFloat
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
	OpSetArg  // Special opcode for setting up function arguments - bypasses register allocator
	OpStackAlloc // Dst=imm byte count: subq from rsp for stack call args
	OpStackFree  // Dst=imm byte count: addq back after call
	OpCvt        // Numeric conversion: Src2=imm mode (f2d,d2f,i2f,i2d,f2i,d2i)
	OpSetAl      // Dst=imm count: mov to %eax for varargs %al (before call)
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
	varCounter   int  // Counter to make variable names unique
	
	// Symbol tables
	localVars    map[string]*Symbol  // Current active binding for each variable name
	allLocalVars map[string]*Symbol  // All local variables (with unique keys)
	globalVars   map[string]*Symbol
	functions    map[string]*FunctionSignature // Track function signatures
	funcIsLocal  map[string]bool // static/inline-only functions: no .globl
	stringLits   map[string]string
	structs      map[string]*StructDef  // Struct definitions from parser
	typedefs     map[string]string      // Typedef aliases from parser
	enums        map[string]int         // Enum constants from parser

	// slotSizes maps frame offsets (negative rbp offsets) to their declared
	// byte size for the CURRENT function. emit() stamps it onto var/mem
	// operands so loads/stores use width-correct movs (movl for int).
	// Without this, 8-byte movq on packed 4-byte locals read/wrote
	// neighboring variables (infinite loops, corruption). Reset per function.
	slotSizes    map[int]int

	stackOffset  int
}

func NewInstructionSelector() *InstructionSelector {
	is := &InstructionSelector{
		instructions: []*IRInstruction{},
		localVars:    make(map[string]*Symbol),
		allLocalVars: make(map[string]*Symbol),
		globalVars:   make(map[string]*Symbol),
		functions:    make(map[string]*FunctionSignature),
		funcIsLocal:  make(map[string]bool),
		stringLits:   make(map[string]string),
		structs:      make(map[string]*StructDef),
		typedefs:     make(map[string]string),
		enums:        make(map[string]int),
		slotSizes:    make(map[int]int),
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

// cSizeOf maps a basic C type spelling to its LP64 size.
// ok=false when typ is not a basic type (struct/enum/typedef names).
// Compile-time only; deliberately exhaustive over common spellings so
// "long int" and "unsigned long" don't silently become 4.
func cSizeOf(typ string) (size int, ok bool) {
	t := strings.TrimSpace(typ)
	for {
		changed := false
		for _, q := range []string{"const ", "static ", "extern ", "volatile ", "register ", "inline "} {
			if strings.HasPrefix(t, q) {
				t = strings.TrimSpace(t[len(q):])
				changed = true
				break
			}
		}
		if !changed {
			break
		}
	}
	longs := 0
	var core []string
	for _, w := range strings.Fields(t) {
		switch w {
		case "signed", "unsigned":
			// no size effect
		case "long":
			longs++
		default:
			core = append(core, w)
		}
	}
	if longs > 0 {
		return 8, true // long, long long, long int, unsigned long, ...
	}
	if len(core) == 0 {
		return 4, true // bare "unsigned" / "signed" means int
	}
	switch core[0] {
	case "char":
		return 1, true
	case "short":
		return 2, true
	case "int":
		return 4, true
	case "float":
		return 4, true
	case "double":
		return 8, true
	case "void":
		return 0, true
	case "bool":
		return 4, true // raylib ABI: bool is a 4-byte enum; matches our mapping
	}
	return 0, false
}

// is64BitIntType reports whether an integer type is 64-bit wide (LP64).
// Unknown spellings default to 64-bit, matching getTypeSize's default.
func (is *InstructionSelector) is64BitIntType(t string) bool {
	t = strings.TrimSpace(is.resolveType(t))
	if strings.Contains(t, "*") {
		return true
	}
	if s, ok := cSizeOf(t); ok {
		return s == 8
	}
	return true
}

// isUnsignedIntType reports whether a type is unsigned (after typedef
// resolution) AND at least int rank. Narrower unsigned types (char, short)
// promote to signed int under C rules, so they don't trigger unsigned
// arithmetic. Pointers never count.
func (is *InstructionSelector) isUnsignedIntType(t string) bool {
	rt := strings.TrimSpace(is.resolveType(t))
	if strings.Contains(rt, "*") {
		return false
	}
	hasUnsigned := false
	for _, w := range strings.Fields(rt) {
		if w == "unsigned" {
			hasUnsigned = true
			break
		}
	}
	if !hasUnsigned {
		return false
	}
	if s, ok := cSizeOf(rt); ok {
		return s >= 4
	}
	return true
}

// Falls back to 8 for unknown/void (fast default, never 0).
// cvtMode names the conversion between two types for OpCvt ("" = none).
// Only float/double/signed-32-bit-int crossings convert; everything else
// (pointers, same kinds, unknowns) passes through bitwise as before.
func cvtMode(srcT, dstT string) string {
	srcF, srcD, srcI := cvtClass(srcT)
	dstF, dstD, dstI := cvtClass(dstT)
	if srcF && dstD {
		return "f2d"
	}
	if srcD && dstF {
		return "d2f"
	}
	if srcI && dstF {
		return "i2f"
	}
	if srcI && dstD {
		return "i2d"
	}
	if srcF && dstI {
		return "f2i"
	}
	if srcD && dstI {
		return "d2i"
	}
	return ""
}

// cvtClass buckets a type for conversions.
func cvtClass(t string) (isFloat, isDouble, isInt32 bool) {
	t = strings.TrimSpace(t)
	if strings.Contains(t, "*") || t == "" {
		return false, false, false
	}
	base := t
	for _, q := range []string{"const ", "volatile ", "register ", "static ", "extern "} {
		base = strings.TrimSpace(strings.TrimPrefix(base, q))
	}
	if base == "float" {
		return true, false, false
	}
	if base == "double" {
		return false, true, false
	}
	if s, ok := cSizeOf(base); ok && s == 4 {
		return false, false, true
	}
	return false, false, false
}

func (is *InstructionSelector) elemSizeForPtr(ptrType string) int {
	t := is.resolveType(strings.TrimSpace(ptrType))
	if strings.HasSuffix(t, "*") {
		elem := strings.TrimSpace(strings.TrimSuffix(t, "*"))
		if strings.HasSuffix(elem, "*") {
			return 8 // pointer elements
		}
		if s := is.getTypeSize(elem); s > 0 {
			return s
		}
	}
	return 8
}

// structDefFor returns the struct definition for an ELEMENT type, or nil.
// The argument must already be one level down (arrayElemSize element type,
// or a pointer type with exactly one level trimmed): any remaining star
// means pointer elements (value-load), a bare struct name means struct
// elements (address-yielding).
func (is *InstructionSelector) structDefFor(elemType string) *StructDef {
	t := is.resolveType(strings.TrimSpace(elemType))
	for _, q := range []string{"const ", "volatile ", "static ", "extern "} {
		t = strings.TrimSpace(strings.TrimPrefix(t, q))
	}
	if strings.Contains(t, "*") {
		return nil // pointer (or deeper) elements: load the value
	}
	t = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(t, "struct "), "union "))
	t = strings.TrimSpace(t)
	if t == "" {
		return nil
	}
	if def, ok := is.structs[t]; ok {
		return def
	}
	return nil
}

// funcHasWord reports whether a type string contains a storage word.
func funcHasWord(typ, word string) bool {
	for _, w := range strings.Fields(typ) {
		if w == word {
			return true
		}
	}
	return false
}

// arrayElemSize returns the element size for indexing an array/pointer
// variable: declared element type for true arrays, pointee type otherwise.
func (is *InstructionSelector) arrayElemSize(sym *Symbol) (elemType string, elemSize int) {
	if sym.ArraySize > 0 {
		if s := is.getTypeSize(sym.Type); s > 0 {
			return sym.Type, s
		}
		return sym.Type, 8
	}
	t := is.resolveType(strings.TrimSpace(sym.Type))
	if strings.HasSuffix(t, "*") {
		elem := strings.TrimSpace(strings.TrimSuffix(t, "*"))
		if !strings.HasSuffix(elem, "*") {
			if s := is.getTypeSize(elem); s > 0 {
				return elem, s
			}
		}
		return elem, 8
	}
	return sym.Type, 8
}

// lvalueAddrSize materializes the address of an assignable location
// (struct member, dereference, array element) into a temp, plus the access
// size. ok=false means "not an lvalue we address" (caller falls through).
// Direct codegen, no optimization — mirrors the load paths.
func (is *InstructionSelector) lvalueAddrSize(target *ASTNode) (addr *Operand, size int, ok bool, err error) {
	switch target.Type {
	case NodeMemberAccess:
		if len(target.Children) < 1 {
			return nil, 0, false, nil
		}
		baseNode := target.Children[0]
		memberName := target.MemberName
		isPtr := target.IsPointer
		// Resolve struct + member via identifier base when possible.
		var structType string
		var baseTemp *Operand
		var baseIsIdent bool
		var baseVar string
		var baseGlobal bool
		var baseOff int
		if baseNode.Type == NodeIdentifier {
			baseIsIdent = true
			baseVar = baseNode.VarName
			if sym, found := is.localVars[baseVar]; found {
				structType = sym.Type
				baseOff = sym.Offset
			} else if sym, found := is.globalVars[baseVar]; found {
				structType = sym.Type
				baseGlobal = true
			} else {
			return nil, 0, false, fmt.Errorf("undefined variable: %s", baseVar)
			}
		} else {
			bt, berr := is.selectExpression(baseNode)
			if berr != nil {
				return nil, 0, false, berr
			}
			baseTemp = bt
			structType = bt.DataType
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
		for _, q := range []string{"const ", "volatile ", "static ", "extern "} {
			structName = strings.TrimSpace(strings.TrimPrefix(structName, q))
		}
		if len(structName) > 7 && structName[:7] == "struct " {
			structName = structName[7:]
		} else if len(structName) > 6 && structName[:6] == "union " {
			structName = structName[6:]
		}
		structName = strings.TrimSpace(structName)
		structDef, found := is.structs[structName]
		if !found {
			return nil, 0, false, nil
		}
		memberOffset := -1
		memberSize := 8
		for _, m := range structDef.Members {
			if m.Name == memberName {
				memberOffset = m.Offset
				memberSize = m.Size
				break
			}
		}
		if memberOffset == -1 {
			return nil, 0, false, nil
		}
		if !isPtr && baseIsIdent && !baseGlobal {
			// Local struct member: address is a pure stack offset.
			res := is.newTemp()
			is.emit(OpLoadAddr, res, &Operand{Type: "var", Value: baseVar, Offset: baseOff + memberOffset}, nil)
			return res, memberSize, true, nil
		}
		// Pointer/global/complex base: derive address then add member offset.
		var ptrTemp *Operand
		if baseIsIdent {
			if isPtr {
				// p->m: need the pointer VALUE.
				ptrTemp = is.newTemp()
				if baseGlobal {
					is.emit(OpLoad, ptrTemp, &Operand{Type: "var", Value: baseVar, IsGlobal: true}, nil)
				} else {
					is.emit(OpLoad, ptrTemp, &Operand{Type: "var", Value: baseVar, Offset: baseOff}, nil)
				}
			} else if baseGlobal {
				// g.m: address of the global struct.
				ptrTemp = is.newTemp()
				is.emit(OpLoadAddr, ptrTemp, &Operand{Type: "var", Value: baseVar, IsGlobal: true}, nil)
			} else {
				// Should have been handled by the local fast path above,
				// but stay correct if reached.
				ptrTemp = is.newTemp()
				is.emit(OpLoadAddr, ptrTemp, &Operand{Type: "var", Value: baseVar, Offset: baseOff}, nil)
			}
		} else {
			ptrTemp = baseTemp
		}
		if !isPtr && !baseIsIdent {
			// dot on a complex base value (not addressable) — give up.
			return nil, 0, false, nil
		}
		if memberOffset != 0 {
			addRes := is.newTemp()
			is.emit(OpAdd, addRes, ptrTemp, &Operand{Type: "imm", Value: fmt.Sprintf("%d", memberOffset)})
			ptrTemp = addRes
		}
		return ptrTemp, memberSize, true, nil
	case NodeUnaryOp:
		if target.Operator != "*" || len(target.Children) < 1 {
			return nil, 0, false, nil
		}
		ptr, perr := is.selectExpression(target.Children[0])
		if perr != nil {
			return nil, 0, false, perr
		}
		return ptr, 8, true, nil
	case NodeArrayAccess:
		if len(target.Children) < 2 {
			return nil, 0, false, nil
		}
		baseNode := target.Children[0]
		index, ierr := is.selectExpression(target.Children[1])
		if ierr != nil {
			return nil, 0, false, ierr
		}
		if baseNode.Type == NodeIdentifier {
			var sym *Symbol
			if s, found := is.localVars[baseNode.VarName]; found {
				sym = s
			} else if s, found := is.globalVars[baseNode.VarName]; found {
				sym = s
			} else {
				return nil, 0, false, fmt.Errorf("undefined array: %s", baseNode.VarName)
			}
			_, elemSize := is.arrayElemSize(sym)
			byteOff := is.newTemp()
			is.emit(OpMul, byteOff, index, &Operand{Type: "imm", Value: fmt.Sprintf("%d", elemSize)})
			if !strings.Contains(sym.Type, "*") && sym.ArraySize > 0 {
				// True local/global array: address from its base.
				var baseAddr *Operand
				if sym.IsGlobal {
					baseAddr = is.newTemp()
					is.emit(OpLoadAddr, baseAddr, &Operand{Type: "var", Value: baseNode.VarName, IsGlobal: true}, nil)
				} else {
					baseAddr = is.newTemp()
					is.emit(OpLoadAddr, baseAddr, &Operand{Type: "var", Value: baseNode.VarName, Offset: sym.Offset}, nil)
				}
				finalAddr := is.newTemp()
				is.emit(OpAdd, finalAddr, baseAddr, byteOff)
				return finalAddr, elemSize, true, nil
			}
			// Pointer variable: load value, add byte offset.
			baseAddr, berr := is.selectExpression(baseNode)
			if berr != nil {
				return nil, 0, false, berr
			}
			finalAddr := is.newTemp()
			is.emit(OpAdd, finalAddr, baseAddr, byteOff)
			return finalAddr, elemSize, true, nil
		}
		// Complex base: evaluate, derive stride from pointer type.
		baseAddr, berr := is.selectExpression(baseNode)
		if berr != nil {
			return nil, 0, false, berr
		}
		elemSize := is.elemSizeForPtr(baseAddr.DataType)
		byteOff := is.newTemp()
		is.emit(OpMul, byteOff, index, &Operand{Type: "imm", Value: fmt.Sprintf("%d", elemSize)})
		finalAddr := is.newTemp()
		is.emit(OpAdd, finalAddr, baseAddr, byteOff)
		return finalAddr, elemSize, true, nil
	}
	return nil, 0, false, nil
}

func (is *InstructionSelector) emit(op OpCode, dst, src1, src2 *Operand) {
	// Stamp declared widths onto frame operands: any var/mem operand with
	// no explicit Size that addresses the current frame (negative offset)
	// gets the declared slot size, so the emitter uses movl/movw/movb for
	// sub-8-byte locals instead of 8-byte movq (which read/wrote neighbors).
	for _, op := range []*Operand{dst, src1, src2} {
		if op == nil || op.Size != 0 {
			continue
		}
		if op.IsGlobal {
			// Globals: size from the symbol table (skip zero-size junk).
			if (op.Type == "var" || op.Type == "mem") && op.Value != "" {
				if sym, ok := is.globalVars[op.Value]; ok && sym.Size > 0 && sym.Size < 8 {
					op.Size = sym.Size
				}
			}
			continue
		}
		if (op.Type == "var" || op.Type == "mem") && op.Offset < 0 {
			if sz, ok := is.slotSizes[op.Offset]; ok && sz > 0 {
				op.Size = sz
			}
		}
	}
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
	if s, ok := cSizeOf(typ); ok {
		return s
	}
	return 8
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
	// Leading qualifiers don't affect identity/size; strip them so
	// "const struct Foo" resolves like "struct Foo".
	typ = strings.TrimSpace(typ)
	for _, q := range []string{"const ", "volatile ", "static ", "extern ", "register "} {
		if strings.HasPrefix(typ, q) {
			typ = strings.TrimSpace(strings.TrimPrefix(typ, q))
		}
	}
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
		// static and plain-inline definitions get internal linkage (no
		// .globl): otherwise headers like raymath.h multiply-define them
		// at link time against their library copies. extern (incl. extern
		// inline, which requests an instance) stays global.
		if funcHasWord(node.ReturnType, "static") ||
			(funcHasWord(node.ReturnType, "inline") && !funcHasWord(node.ReturnType, "extern")) {
			is.funcIsLocal[node.Name] = true
		}
		
		// Skip external function declarations (no body)
		if node.Children == nil || len(node.Children) == 0 {
			// External function - just track it (no code generation)
			return nil
		}
		
		is.currentFunc = node.Name
		is.localVars = make(map[string]*Symbol)
		is.allLocalVars = make(map[string]*Symbol)
		is.stackOffset = 0
		is.varCounter = 0  // Reset counter for each function
		is.slotSizes = make(map[int]int)
		
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
			// Width-correct slot: int params occupy 4 bytes (see slotSizes).
			paramSize := 8
			if paramType != "" {
				if s := is.getTypeSize(paramType); s > 0 {
					paramSize = s
				}
			}
			is.slotSizes[is.stackOffset] = paramSize
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
			} else {
				// Stack parameter per SysV: 16(%rbp) + 8k (ret addr + saved rbp).
				prmOp := &Operand{Type: "prm", Offset: 16 + (regIdx-len(argRegs))*8}
				paramOp := &Operand{Type: "mem", Offset: is.stackOffset}
				is.emit(OpLoad, paramOp, prmOp, nil)
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
		} else if s := is.getTypeSize(dataType); s > 0 {
			// Scalar/typedef/pointer: real width (int -> 4, not 8).
			// Packed 4-byte locals keep neighbors out of 8-byte movs
			// now that accesses are width-correct (see slotSizes).
			varSize = s
		}
		
		if node.ArraySize > 0 {
			varSize = node.ArraySize * varSize  // Array: count * element size
		}
		
		if node.IsGlobal {
			isStatic := strings.HasPrefix(strings.TrimSpace(node.DataType), "static")
			is.globalVars[node.VarName] = &Symbol{
				Name:      node.VarName,
				IsGlobal:  true,
				IsStatic:  isStatic,
				Size:      varSize,
				ArraySize: node.ArraySize,
				Type:      dataType,
			}
			// Keep constant initializers (e.g. = 5): the emitter places
			// them in .data instead of zeroed .comm.
			if len(node.Children) > 0 && node.ArraySize == 0 {
				is.globalVars[node.VarName].Init = node.Children[0]
			}
		} else {
			is.stackOffset -= varSize
			varOffset := is.stackOffset  // Save the variable's offset
			
			// Create a unique key for this variable instance
			is.varCounter++
			uniqueKey := fmt.Sprintf("%s#%d", node.VarName, is.varCounter)
			
			sym := &Symbol{
				Name:      node.VarName,  // Keep original name
				Offset:    varOffset,
				Size:      varSize,
				ArraySize: node.ArraySize,
				Type:      dataType,
			}
			
			// Store in both maps:
			// - allLocalVars keeps ALL variable instances (prevents offset reuse)
			// - localVars tracks current binding (for lookups)
			is.allLocalVars[uniqueKey] = sym
			is.localVars[node.VarName] = sym
			is.slotSizes[varOffset] = varSize
			
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

					varOp := &Operand{Type: "var", Value: node.VarName, Offset: varOffset, Size: varSize}
					is.emit(OpStore, varOp, result, nil)
				}
			}

			// Array initialization: T arr[N] = {e0, e1, ...} where elements
			// may be nested struct literals (raymath-style tables).
			if len(node.Children) > 0 && node.ArraySize > 0 {
				initExpr := node.Children[0]
				if initExpr.Type == NodeCompoundLiteral {
					elemSize := varSize / node.ArraySize
					if elemSize <= 0 {
						elemSize = 8
					}
					for i, elem := range initExpr.Children {
						if i >= node.ArraySize {
							break
						}
						// Value FIRST so no address temp spans its codegen
						// (emitter scratch would clobber a shared register).
						if elem.Type == NodeCompoundLiteral {
							srcAddr, serr := is.selectExpression(elem)
							if serr != nil {
								return serr
							}
							// Recompute the element address AFTER the value:
							// short-lived and consumed immediately.
							baseAddr := is.newTemp()
							is.emit(OpLoadAddr, baseAddr, &Operand{Type: "var", Value: node.VarName, Offset: varOffset}, nil)
							elemAddr := is.newTemp()
							if i > 0 {
								offOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", i*elemSize)}
								is.emit(OpAdd, elemAddr, baseAddr, offOp)
							} else {
								is.emit(OpMov, elemAddr, baseAddr, nil)
							}
							// Copy elemSize bytes, width-aware tail.
							for off := 0; off < elemSize; {
								chunk := 8
								if elemSize-off < 8 {
									chunk = elemSize - off
								}
								srcTemp := is.newTemp()
								srcOp := &Operand{Type: "ptr", IndexTemp: srcAddr, Size: chunk}
								dstOp := &Operand{Type: "ptr", IndexTemp: elemAddr, Size: chunk}
								if off > 0 {
									offOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", off)}
									srcA := is.newTemp()
									is.emit(OpAdd, srcA, srcAddr, offOp)
									srcOp.IndexTemp = srcA
									dstA := is.newTemp()
									is.emit(OpAdd, dstA, elemAddr, offOp)
									dstOp.IndexTemp = dstA
								}
								is.emit(OpLoad, srcTemp, srcOp, nil)
								is.emit(OpStore, dstOp, srcTemp, nil)
								off += chunk
							}
						} else {
							v, verr := is.selectExpression(elem)
							if verr != nil {
								return verr
							}
							baseAddr := is.newTemp()
							is.emit(OpLoadAddr, baseAddr, &Operand{Type: "var", Value: node.VarName, Offset: varOffset}, nil)
							elemAddr := is.newTemp()
							if i > 0 {
								offOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", i*elemSize)}
								is.emit(OpAdd, elemAddr, baseAddr, offOp)
							} else {
								is.emit(OpMov, elemAddr, baseAddr, nil)
							}
							storeOp := &Operand{Type: "ptr", IndexTemp: elemAddr, Size: elemSize}
							if elemSize > 8 {
								storeOp.Size = 0
							}
							is.emit(OpStore, storeOp, v, nil)
						}
					}
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
		// Explicit slots from the parser (no positional guessing: omitted
		// init/cond/incr used to shift children and misclassify, e.g. a bare
		// `i = 0` init made the condition run as the body forever).
		var init, cond, incr, body *ASTNode
		if node.ForBody != nil || node.ForInit != nil || node.ForCond != nil || node.ForIncr != nil {
			init, cond, incr, body = node.ForInit, node.ForCond, node.ForIncr, node.ForBody
		} else {
		// Legacy positional shape (not produced by the current parser).
		idx := 0
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
		op := &Operand{Type: "imm", Value: node.Value}
		if node.DataType != "" {
			op.DataType = node.DataType
		}
		return op, nil
		
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
			if sym.ArraySize > 0 {
				// Array decays to pointer-to-first-element (C semantics).
				// Loading the "value" would read element bytes as an address.
				addr := is.newTemp()
				addr.DataType = sym.Type + "*"
				is.emit(OpLoadAddr, addr, &Operand{Type: "var", Value: node.VarName, Offset: sym.Offset}, nil)
				return addr, nil
			}
			temp := is.newTemp()
			temp.DataType = sym.Type
			varOp := &Operand{Type: "var", Value: node.VarName, Offset: sym.Offset}
			is.emit(OpLoad, temp, varOp, nil)
			return temp, nil
		} else if sym, ok := is.globalVars[node.VarName]; ok {
			if sym.ArraySize > 0 {
				addr := is.newTemp()
			addr.DataType = sym.Type + "*"
			is.emit(OpLoadAddr, addr, &Operand{Type: "var", Value: node.VarName, IsGlobal: true}, nil)
			return addr, nil
			}
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

		// Propagate type: if either operand is float/double, result is float/double
		if left.DataType == "double" || right.DataType == "double" {
			result.DataType = "double"
		} else if left.DataType == "float" || right.DataType == "float" {
			result.DataType = "float"
		} else if (node.Operator == "/" || node.Operator == "%") &&
			!strings.Contains(left.DataType, "*") && !strings.Contains(right.DataType, "*") {
			// Integer division: full C usual conversions (LP64).
			// Width AND signedness decide the instruction (div vs idiv,
			// 32 vs 64 bit). Operands are rewritten to the canonical type
			// so the emitter only needs spelling checks. Single-use temps
			// make this safe (no other instruction reads them).
			lU := is.isUnsignedIntType(left.DataType)
			rU := is.isUnsignedIntType(right.DataType)
			l64 := is.is64BitIntType(left.DataType)
			r64 := is.is64BitIntType(right.DataType)
			var divT string
			if l64 || r64 {
				if lU || rU {
					divT = "unsigned long"
				} else {
					divT = "long"
				}
			} else if lU || rU {
				if lU {
					divT = left.DataType
				} else {
					divT = right.DataType
				}
			} else if left.DataType != "" {
				divT = left.DataType
			} else {
				divT = right.DataType
			}
			result.DataType = divT
			// Canonicalize sides for 64-bit ops (32-bit movl/divl would
			// truncate). 32-bit sides are already spelled correctly.
			if l64 || r64 {
				left.DataType = divT
				right.DataType = divT
			}
		} else if !strings.Contains(left.DataType, "*") && !strings.Contains(right.DataType, "*") &&
			(is.isUnsignedIntType(left.DataType) || is.isUnsignedIntType(right.DataType)) {
			// Other operators: unsigned wins for the result type only.
			if is.isUnsignedIntType(left.DataType) {
				result.DataType = left.DataType
			} else {
				result.DataType = right.DataType
			}
		} else if left.DataType != "" {
			result.DataType = left.DataType
		} else if right.DataType != "" {
			result.DataType = right.DataType
		}
		
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
		// For increment/decrement, we need to modify the variable directly
		if node.Operator == "++" || node.Operator == "--" || 
		   node.Operator == "++_post" || node.Operator == "--_post" {
			// Check if operand is a simple identifier
			if node.Children[0].Type == NodeIdentifier {
				varName := node.Children[0].VarName
				var varOp *Operand
				var varType string

				if sym, ok := is.localVars[varName]; ok {
					varOp = &Operand{Type: "var", Value: varName, Offset: sym.Offset}
					varType = sym.Type
				} else if sym, ok := is.globalVars[varName]; ok {
					varOp = &Operand{Type: "var", Value: varName, IsGlobal: true}
					varType = sym.Type
				} else {
					return nil, fmt.Errorf("undefined variable: %s", varName)
				}

				// Load current value
				currentVal := is.newTemp()
				currentVal.DataType = varType
				is.emit(OpLoad, currentVal, varOp, nil)

				// Compute new value
				one := &Operand{Type: "imm", Value: "1"}
				newVal := is.newTemp()
				newVal.DataType = varType
				
				if node.Operator == "++" || node.Operator == "++_post" {
					is.emit(OpAdd, newVal, currentVal, one)
				} else {
					is.emit(OpSub, newVal, currentVal, one)
				}
				
				// Store new value back to variable
				is.emit(OpStore, varOp, newVal, nil)
				
				// Return appropriate value
				if node.Operator == "++_post" || node.Operator == "--_post" {
					// Post-increment: return old value
					return currentVal, nil
				} else {
					// Pre-increment: return new value
					return newVal, nil
				}
			}
			// Fallthrough for complex expressions
		}

		// Complex-lvalue ++/-- (ptr->m, s.m, *p, a[i]): address it, load,
		// add/sub, store back. The old fallthrough incremented a temp and
		// dropped it (e.g. arr->length++ was a no-op). Direct/fast, no pass.
		if (node.Operator == "++" || node.Operator == "--" ||
			node.Operator == "++_post" || node.Operator == "--_post") &&
			len(node.Children) > 0 {
			if addrOp, size, ok, lerr := is.lvalueAddrSize(node.Children[0]); lerr != nil {
				return nil, lerr
			} else if ok {
				cur := is.newTemp()
				is.emit(OpLoad, cur, &Operand{Type: "ptr", IndexTemp: addrOp, Size: size}, nil)
				one := &Operand{Type: "imm", Value: "1"}
				newV := is.newTemp()
				if node.Operator == "++" || node.Operator == "++_post" {
					is.emit(OpAdd, newV, cur, one)
				} else {
					is.emit(OpSub, newV, cur, one)
				}
				is.emit(OpStore, &Operand{Type: "ptr", IndexTemp: addrOp, Size: size}, newV, nil)
				if node.Operator == "++" || node.Operator == "--" {
					return newV, nil
				}
				return cur, nil
			}
		}
		
		// Fast path for & (address-of): materialize to a temp via leaq
		// so downstream never sees symbolic "addr" operands. No opts, just direct.
		if node.Operator == "&" {
			child := node.Children[0]
		// &*p == p
			if child.Type == NodeUnaryOp && child.Operator == "*" && len(child.Children) > 0 {
				return is.selectExpression(child.Children[0])
		}
			// &ident
			if child.Type == NodeIdentifier {
				varName := child.VarName
				// &func == function address (for callbacks).
				if _, isFunc := is.functions[varName]; isFunc {
						return &Operand{Type: "label", Value: varName}, nil
				}
				addrResult := is.newTemp()
				if sym, ok := is.localVars[varName]; ok {
					is.emit(OpLoadAddr, addrResult, &Operand{Type: "var", Value: varName, Offset: sym.Offset}, nil)
					addrResult.DataType = sym.Type + "*"
					return addrResult, nil
				} else if sym, ok := is.globalVars[varName]; ok {
					is.emit(OpLoadAddr, addrResult, &Operand{Type: "var", Value: varName, IsGlobal: true}, nil)
					addrResult.DataType = sym.Type + "*"
					return addrResult, nil
				}
				return nil, fmt.Errorf("undefined variable: %s", varName)
		}
		// &member / &array-element: share the lvalue address helper.
		if child.Type == NodeMemberAccess || child.Type == NodeArrayAccess {
			if addrOp, _, ok, lerr := is.lvalueAddrSize(child); lerr != nil {
				return nil, lerr
			} else if ok {
				return addrOp, nil
			}
		}
			return nil, fmt.Errorf("& operator on complex expression not yet supported (type=%d)", child.Type)
		}

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
			// Pre-increment (fallback for complex expressions)
			one := &Operand{Type: "imm", Value: "1"}
			is.emit(OpAdd, operand, operand, one)
			is.emit(OpMov, result, operand, nil)
		case "--":
			// Pre-decrement (fallback for complex expressions)
			one := &Operand{Type: "imm", Value: "1"}
			is.emit(OpSub, operand, operand, one)
			is.emit(OpMov, result, operand, nil)
		case "++_post":
			// Post-increment (fallback for complex expressions)
			is.emit(OpMov, result, operand, nil)
			one := &Operand{Type: "imm", Value: "1"}
			is.emit(OpAdd, operand, operand, one)
		case "--_post":
			// Post-decrement (fallback for complex expressions)
			is.emit(OpMov, result, operand, nil)
			one := &Operand{Type: "imm", Value: "1"}
			is.emit(OpSub, operand, operand, one)
		case "&":
			// Handled by fast path above; should never reach here.
			return nil, fmt.Errorf("& operator should have been handled by fast path")
		case "*":
			// Dereference operator - load from pointer, width from pointee
			// type (a char* deref must be movzbl, not 8-byte movq, or
			// string loops read past NUL into garbage).
			derefSize := 0
			if operand.DataType != "" && strings.HasSuffix(operand.DataType, "*") {
				derefSize = is.elemSizeForPtr(operand.DataType)
			}
			ptrOp := &Operand{Type: "ptr", Value: operand.Value, IndexTemp: operand}
			if derefSize > 0 && derefSize < 8 {
				ptrOp.Size = derefSize
			}
			is.emit(OpLoad, result, ptrOp, nil)
			
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
			
			// Determine element type and size from the declaration
			// (true array -> element type; pointer -> pointee type).
			var arrSym *Symbol
			if sym, ok := is.localVars[varName]; ok {
				s := sym
				arrSym = s
			} else if sym, ok := is.globalVars[varName]; ok {
				s := sym
				arrSym = s
			}
			elementType, elementSize := is.arrayElemSize(arrSym)
			
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
					Size:      elementSize,
					DataType:  elementType,
				}
				is.emit(OpLoad, result, ptrOp, nil)
				return result, nil
			}
			
			// Struct elements are address-yielding lvalues (a value load
			// would truncate to 8 bytes and member access would treat
			// those bytes as a pointer).
			if is.structDefFor(elementType) != nil {
				baseAddr := is.newTemp()
				if isGlobal {
					is.emit(OpLoadAddr, baseAddr, &Operand{Type: "var", Value: varName, IsGlobal: true}, nil)
				} else {
					is.emit(OpLoadAddr, baseAddr, &Operand{Type: "var", Value: varName, Offset: baseOffset}, nil)
				}
				finalAddr := is.newTemp()
				finalAddr.DataType = elementType + "*"
				is.emit(OpAdd, finalAddr, baseAddr, byteOffset)
				return finalAddr, nil
			}

			// Use the optimized array access path for actual arrays
			result := is.newTemp()
			result.DataType = elementType
			arrayOp := &Operand{
				Type:      "array",
				Value:     varName,
				Offset:    baseOffset,
				IsGlobal:  isGlobal,
				IndexTemp: byteOffset,
				Size:      elementSize,
				DataType:  elementType,
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

			// Element size comes from the pointer type (e.g. enum* -> 4).
			// Hardcoding 8 here overflowed realloc'd buffers (heap corruption).
			elemSize := is.elemSizeForPtr(baseAddr.DataType)
			elementSizeOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", elemSize)}
			byteOffset := is.newTemp()
			is.emit(OpMul, byteOffset, index, elementSizeOp)

			// Add base address + offset
			finalAddr := is.newTemp()
			is.emit(OpAdd, finalAddr, baseAddr, byteOffset)

			elemType := strings.TrimSuffix(strings.TrimSpace(is.resolveType(baseAddr.DataType)), "*")
			if is.structDefFor(elemType) != nil {
				// Struct elements yield their address (see true-array path).
				finalAddr.DataType = elemType + "*"
				return finalAddr, nil
			}

			// Load from the computed address
			result := is.newTemp()
			result.DataType = elemType
			ptrOp := &Operand{
				Type:      "ptr",
				IndexTemp: finalAddr,
				Size:      elemSize,
				DataType:  result.DataType,
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
		for _, q := range []string{"const ", "volatile ", "static ", "extern "} {
			structName = strings.TrimSpace(strings.TrimPrefix(structName, q))
		}

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
		memberType := "" // NEW: track member type
		for _, member := range structDef.Members {
			if member.Name == memberName {
				memberOffset = member.Offset
				memberSize = member.Size
				memberType = member.Type // NEW: get member type
				break
			}
		}
		
		if memberOffset == -1 {
			return nil, fmt.Errorf("struct %s has no member %s", structName, memberName)
		}
		
		// Load member value
		result := is.newTemp()
		result.DataType = memberType // NEW: set result DataType
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
			memberOp := &Operand{Type: "ptr", IndexTemp: ptrTemp, Size: memberSize, DataType: memberType} // NEW: set DataType
			is.emit(OpLoad, result, memberOp, nil)
		} else {
			// struct.member: direct access
			// This only works for simple variable bases
			if baseTemp.Type == "var" {
				finalOffset := baseTemp.Offset + memberOffset
				memberOp := &Operand{Type: "var", Value: baseTemp.Value, Offset: finalOffset, IsGlobal: baseTemp.IsGlobal, Size: memberSize, DataType: memberType}
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
				memberOp := &Operand{Type: "ptr", IndexTemp: ptrTemp, Size: memberSize, DataType: memberType}
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
					
					memberOp := &Operand{Type: "ptr", IndexTemp: ptrTemp, Size: memberSize}
					is.emit(OpStore, memberOp, assignValue, nil)
				} else {
					if baseTemp.Type == "var" {
						finalOffset := baseTemp.Offset + memberOffset
						memberOp := &Operand{Type: "var", Value: baseTemp.Value, Offset: finalOffset, IsGlobal: baseTemp.IsGlobal, Size: memberSize}
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

			// Stride comes from the declaration (see arrayElemSize).
			// For complex bases the symbol is unknown here; that branch
			// recomputes from the pointer type after evaluating base.
			stElemSize := 8
			if baseNode.Type == NodeIdentifier {
				if sym, ok := is.localVars[baseNode.VarName]; ok {
					_, stElemSize = is.arrayElemSize(sym)
				} else if sym, ok := is.globalVars[baseNode.VarName]; ok {
					_, stElemSize = is.arrayElemSize(sym)
				}
			}
			// Calculate byte offset: index * elementSize (emitted per
			// branch below, since complex bases need their own stride).
			elementSize := &Operand{Type: "imm", Value: fmt.Sprintf("%d", stElemSize)}
			
			// Check if base is a simple identifier (local/global array)
			if baseNode.Type == NodeIdentifier {
				varName := baseNode.VarName
				var baseOffset int
				var isGlobal bool
				var stSym *Symbol

				if sym, ok := is.localVars[varName]; ok {
					baseOffset = sym.Offset
					isGlobal = false
					stSym = sym
				} else if sym, ok := is.globalVars[varName]; ok {
					baseOffset = 0
					isGlobal = true
					stSym = sym
				} else {
					return nil, fmt.Errorf("undefined array: %s", varName)
				}

				stElemType, stElemSize2 := is.arrayElemSize(stSym)
				stElemSize = stElemSize2
				elementSize = &Operand{Type: "imm", Value: fmt.Sprintf("%d", stElemSize)}
				if stSym.ArraySize == 0 {
					// Pointer variable: store through its VALUE, not its slot.
					baseAddr, err := is.selectExpression(baseNode)
					if err != nil {
						return nil, err
					}
					byteOffset := is.newTemp()
					is.emit(OpMul, byteOffset, index, elementSize)
					finalAddr := is.newTemp()
					is.emit(OpAdd, finalAddr, baseAddr, byteOffset)
					ptrOp := &Operand{
						Type:      "ptr",
						IndexTemp: finalAddr,
						Size:      stElemSize,
						DataType:  stElemType,
					}
					is.emit(OpStore, ptrOp, value, nil)
					return value, nil
				}

				// True array: optimized array access path
				byteOffset := is.newTemp()
				is.emit(OpMul, byteOffset, index, elementSize)
				arrayOp := &Operand{
					Type:      "array",
					Value:     varName,
					Offset:    baseOffset,
					IsGlobal:  isGlobal,
					IndexTemp: byteOffset,
					Size:      stElemSize,
				}
				is.emit(OpStore, arrayOp, value, nil)
			} else {
				// Base is a complex expression (member access, pointer, etc.)
				// Evaluate it to get the pointer/array address
				baseAddr, err := is.selectExpression(baseNode)
				if err != nil {
					return nil, err
				}

				// Recompute stride from the pointer type (the shared
				// byteOffset used the default 8; member pointers need
				// real sizes, e.g. enum* -> 4).
				stElemSize = is.elemSizeForPtr(baseAddr.DataType)
				byteOffset := is.newTemp()
				is.emit(OpMul, byteOffset, index, &Operand{Type: "imm", Value: fmt.Sprintf("%d", stElemSize)})

				// Add base address + offset
				finalAddr := is.newTemp()
				is.emit(OpAdd, finalAddr, baseAddr, byteOffset)

				// Store to the computed address
				ptrOp := &Operand{
					Type:      "ptr",
					IndexTemp: finalAddr,
					Size:      stElemSize,
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
				memberOp := &Operand{Type: "ptr", IndexTemp: ptrTemp, Size: memberSize}
				is.emit(OpStore, memberOp, value, nil)
			} else {
				// struct.member: direct access
				// Need to compute address of base, add member offset, and store
				
				if baseTemp.Type == "var" {
					// Simple variable
					finalOffset := baseTemp.Offset + memberOffset
					memberOp := &Operand{Type: "var", Value: baseTemp.Value, Offset: finalOffset, IsGlobal: baseTemp.IsGlobal, Size: memberSize}
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
			// Evaluate RHS first, then the pointer: no live pointer temp spans
			// the RHS codegen, whose emitter scratch regs (%rax/%r11) would
			// clobber it. Deliberately dumb/fast ordering, no analysis pass.
			value, err := is.selectExpression(node.Children[1])
			if err != nil {
				return nil, err
			}

			// Get the pointer expression (used immediately below)
			ptrExpr, err := is.selectExpression(node.Children[0].Children[0])
			if err != nil {
				return nil, err
			}
			
			// Check if we're assigning a struct
			// Only if RHS is a direct compound literal (not inside a statement expression)
			isStructCopy := false
			var structSize int
			
			// Check if RHS is a compound literal
			if node.Children[1].Type == NodeCompoundLiteral {
				isStructCopy = true
				structType := node.Children[1].DataType
				structSize = is.getTypeSize(structType)
			}
			
			if isStructCopy && structSize > 0 {
				// Generate a memory copy: copy structSize bytes from value (src addr) to ptrExpr (dst addr)
				if structSize <= 32 {
					// Small struct: copy field by field (8 bytes at a time)
					for offset := 0; offset < structSize; offset += 8 {
						remaining := structSize - offset
						copySize := 8
						if remaining < 8 {
							copySize = remaining
						}
						
						// Load from source: *(value + offset)
						srcTemp := is.newTemp()
						if offset > 0 {
							offsetOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", offset)}
							srcAddr := is.newTemp()
							is.emit(OpAdd, srcAddr, value, offsetOp)
							srcOp := &Operand{Type: "ptr", IndexTemp: srcAddr, Size: copySize}
							is.emit(OpLoad, srcTemp, srcOp, nil)
						} else {
							srcOp := &Operand{Type: "ptr", IndexTemp: value, Size: copySize}
							is.emit(OpLoad, srcTemp, srcOp, nil)
						}
						
						// Store to dest: *(ptrExpr + offset)
						if offset > 0 {
							offsetOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", offset)}
							dstAddr := is.newTemp()
							is.emit(OpAdd, dstAddr, ptrExpr, offsetOp)
							dstOp := &Operand{Type: "ptr", IndexTemp: dstAddr, Size: copySize}
							is.emit(OpStore, dstOp, srcTemp, nil)
						} else {
							dstOp := &Operand{Type: "ptr", IndexTemp: ptrExpr, Size: copySize}
							is.emit(OpStore, dstOp, srcTemp, nil)
						}
					}
				} else {
					// Large struct: call memcpy
					// memcpy(ptrExpr, value, structSize)
					sizeOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", structSize)}
					is.emit(OpMov, &Operand{Type: "reg", Value: "rdi"}, ptrExpr, nil)
					is.emit(OpMov, &Operand{Type: "reg", Value: "rsi"}, value, nil)
					is.emit(OpMov, &Operand{Type: "reg", Value: "rdx"}, sizeOp, nil)
					is.emit(OpCall, &Operand{Type: "label", Value: "memcpy"}, nil, nil)
				}
				
				return value, nil
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
		// Indirect calls (cb.fn(), (*fp)()) have no declared signature.
		// Evaluated late (just before emission below) so the pointer temp
		// never spans arg codegen. See below.
		
		// Evaluate arguments directly into stack slots (dumb but airtight):
		// each arg temp dies at its store, so later arg evals (emitter scratch
		// %rax/%r11) and the SetArg moves (fixed %rdi/%rsi/...) can never
		// clobber an already-evaluated arg. No analysis pass needed.
		argSlots := []*Operand{}
		for _, argNode := range node.Children {
			arg, err := is.selectExpression(argNode)
			if err != nil {
				return nil, err
			}
			is.stackOffset -= 8
			slot := &Operand{Type: "mem", Offset: is.stackOffset, DataType: arg.DataType}
			// Keep float-literal detection working: slots are mem, so record
			// the original immediate spelling on the slot value (unused for mem).
			if arg.Type == "imm" {
				slot.Value = arg.Value
			}
			is.emit(OpStore, slot, arg, nil)
			argSlots = append(argSlots, slot)
		}
		args := argSlots
		
		// Check if we need to allocate space for a large struct return
		var retSlot *Operand
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
		
		// Prepare arguments for call
		// Use OpSetArg which bypasses register allocation
		intRegIdx := argStartIdx
		floatRegIdx := 0
		intRegs := []string{"rdi", "rsi", "rdx", "rcx", "r8", "r9"}
		floatRegs := []string{"xmm0", "xmm1", "xmm2", "xmm3", "xmm4", "xmm5", "xmm6", "xmm7"}
		
		// Overflow args (past 6 int / 8 float regs) go on the stack per SysV.
		// Collect them first so one aligned alloc covers all.
		type stackArg struct {
			slot *Operand
			idx  int
		}
		var stackArgs []stackArg
		for _, arg := range args {
			// Determine if this argument is a float (slots carry the
			// original immediate spelling in Value for this check)
			isFloat := arg.DataType == "float" || arg.DataType == "double" ||
				((arg.Type == "mem" || arg.Type == "imm") && strings.Contains(arg.Value, "."))

			if isFloat {
				if floatRegIdx < len(floatRegs) {
					regOp := &Operand{Type: "freg", Value: floatRegs[floatRegIdx]}
					floatRegIdx++
					is.emit(OpSetArg, regOp, arg, nil)
				} else {
					stackArgs = append(stackArgs, stackArg{slot: arg, idx: len(stackArgs)})
				}
			} else {
				if intRegIdx < len(intRegs) {
					regOp := &Operand{Type: "reg", Value: intRegs[intRegIdx]}
					intRegIdx++
					is.emit(OpSetArg, regOp, arg, nil)
				} else {
					stackArgs = append(stackArgs, stackArg{slot: arg, idx: len(stackArgs)})
				}
			}
		}
		var stackBytes int
		if len(stackArgs) > 0 {
			// 16-aligned so the call below keeps rsp aligned. Dumb, fast.
			stackBytes = (len(stackArgs)*8 + 15) &^ 15
			is.emit(OpStackAlloc, &Operand{Type: "imm", Value: fmt.Sprintf("%d", stackBytes)}, nil, nil)
			for _, sa := range stackArgs {
				sarg := &Operand{Type: "sarg", Offset: sa.idx * 8, DataType: sa.slot.DataType}
				is.emit(OpStore, sarg, sa.slot, nil)
			}
		}
		
		// NOW emit the hidden pointer load (after args are in place)
		if retSlot != nil {
			is.emit(OpLoadAddr, &Operand{Type: "reg", Value: "rdi"}, retSlot, nil)
		}
		
		// SysV varargs need %al = number of vector regs used. Emit it here
		// (before the indirect target is evaluated below): %rax is
		// call-clobbered anyway, so writing it early is always safe,
		// while writing it at call time would kill a target held in %rax.
		is.emit(OpSetAl, &Operand{Type: "imm", Value: fmt.Sprintf("%d", floatRegIdx)}, nil, nil)

		// Call function (direct by label, or indirect through a register).
		// The indirect target is evaluated here — after all arg/setarg code —
		// so its temp is live for exactly one instruction.
		result := is.newTemp()
		var funcOp *Operand
		if node.Target != nil {
			top, terr := is.selectExpression(node.Target)
			if terr != nil {
				return nil, terr
			}
			funcOp = top
		} else {
			funcOp = &Operand{Type: "label", Value: node.Name}
		}
		is.emit(OpCall, result, funcOp, &Operand{Type: "imm", Value: fmt.Sprintf("%d", len(args))})
		if stackBytes > 0 {
			is.emit(OpStackFree, &Operand{Type: "imm", Value: fmt.Sprintf("%d", stackBytes)}, nil, nil)
		}
		
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
			} else if returnType != "" {
				// Ordinary typed return (e.g. unsigned int hash()): carry
				// the type so conversions (unsigned div) apply at use sites.
				// Struct returns keep legacy untyped behavior (their member
				// access on register-held values is a separate feature).
				rt := is.resolveType(returnType)
				isStructRet := strings.Contains(rt, "struct") || strings.Contains(rt, "union")
				if !isStructRet {
					result.DataType = returnType
				}
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
		
	case NodeOffsetof:
		// __builtin_offsetof(Type, member[.sub]): fold to an integer constant.
		structType := is.resolveType(node.DataType)
		total := 0
		for _, field := range strings.Split(node.Value, ".") {
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
			structDef, found := is.structs[structName]
			if !found {
				return nil, fmt.Errorf("__builtin_offsetof: undefined struct '%s'", structName)
			}
			matched := false
			for _, m := range structDef.Members {
				if m.Name == field {
				total += m.Offset
					structType = m.Type
					matched = true
					break
				}
			}
			if !matched {
				return nil, fmt.Errorf("__builtin_offsetof: struct %s has no member %s", structName, field)
			}
		}
		op := &Operand{Type: "imm", Value: fmt.Sprintf("%d", total), DataType: "unsigned long"}
		return op, nil

	case NodeCast:
		// Type cast: (Type)expr, with real conversions across float/int.
		// A bitwise passthrough is wrong (float bits != double/int values).
		if len(node.Children) < 1 {
			return nil, fmt.Errorf("cast needs operand")
		}
		result, err := is.selectExpression(node.Children[0])
		if err != nil {
			return nil, err
		}
		srcT := result.DataType
		dstT := node.DataType
		if mode := cvtMode(srcT, dstT); mode != "" {
			conv := is.newTemp()
			conv.DataType = dstT
			is.emit(OpCvt, conv, result, &Operand{Type: "imm", Value: mode})
			return conv, nil
		}
		// Same-kind cast (or unknown): pass through with new type.
		result.DataType = node.DataType
		return result, nil
		
	default:
		return nil, fmt.Errorf("unknown expression type: %d", node.Type)
	}
}
