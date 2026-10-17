package main

import (
	"fmt"
	"os"
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
	OpSetArg     // Special opcode for setting up function arguments - bypasses register allocator
	OpStackAlloc // Dst=imm byte count: subq from rsp for stack call args
	OpStackFree  // Dst=imm byte count: addq back after call
	OpCvt        // Numeric conversion: Src2=imm mode (f2d,d2f,i2f,i2d,f2i,d2i)
	OpSetAl      // Dst=imm count: mov to %eax for varargs %al (before call)
)

type Operand struct {
	Type      string // "reg", "imm", "var", "label", "mem", "array"
	Value     string
	Offset    int
	IsGlobal  bool
	Size      int
	IndexTemp *Operand // For array indexing, holds the calculated offset
	DataType  string   // Track the C type (e.g., "int", "struct Foo*")
	SourcePtr *Operand // For dereferenced values, track the original pointer
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
	varCounter   int // Counter to make variable names unique
	// Loop/switch break+continue targets (innermost last). breakStack
	// top is the switch end when inside a switch, else the loop end.
	breakStack    []string
	continueStack []string
	// funcDefined tracks functions with bodies in this TU (rax-based
	// float/struct returns). Merely-declared (external) functions use
	// SysV returns (floats in xmm0) and must be read differently.
	funcDefined map[string]bool
	// largeRetHint carries the expected large-struct type for INDIRECT calls
	// (unknown return type) so they still pass the hidden pointer (e.g.
	// r.load_font.fn(...) assigned to Font). "" = off. Save/restore around
	// nested selections (a struct call can nest inside another).
	largeRetHint string

	// Symbol tables
	localVars    map[string]*Symbol // Current active binding for each variable name
	allLocalVars map[string]*Symbol // All local variables (with unique keys)
	globalVars   map[string]*Symbol
	functions    map[string]*FunctionSignature // Track function signatures
	funcIsLocal  map[string]bool               // static/inline-only functions: no .globl
	labelMap     map[string]string             // source label -> asm label (per function)
	stringLits   map[string]string
	structs      map[string]*StructDef // Struct definitions from parser
	typedefs     map[string]string     // Typedef aliases from parser
	enums        map[string]int        // Enum constants from parser

	// slotSizes maps frame offsets (negative rbp offsets) to their declared
	// byte size for the CURRENT function. emit() stamps it onto var/mem
	// operands so loads/stores use width-correct movs (movl for int).
	// Without this, 8-byte movq on packed 4-byte locals read/wrote
	// neighboring variables (infinite loops, corruption). Reset per function.
	slotSizes map[int]int
	// slotTypes maps the same offsets to declared type strings so emit()
	// can insert implicit conversions (float 2.5 stored to an int slot
	// must truncate to 2, not store float bits). Unregistered offsets
	// (arg slots, spill temps) stay bitwise.
	slotTypes map[int]string

	stackOffset int
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
		slotTypes:    make(map[int]string),
		labelMap:     make(map[string]string),
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
	case "struct __pthread_mutex":
		return 40, true // glibc pthread_mutex_t
	case "struct __pthread_cond":
		return 48, true // glibc pthread_cond_t
	case "struct __pthread_attr":
		return 56, true // glibc pthread_attr_t
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
// Any integer↔float/double crossing converts (char/short/int/long);
// pointers, same-kind pairs, and unknowns pass through bitwise as before.
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
	// Every integer type is a conversion source/target: char/short (they
	// arrive 32-bit sign/zero-extended) as well as int and long (the
	// emitter picks the q-form for 8-byte values). Missing the narrow
	// ones made `float f = shortvar;` store raw integer bits.
	if s, ok := cSizeOf(base); ok && s >= 1 && s <= 8 {
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
		if elem == "void" {
			return 1 // GNU byte semantics for void* arithmetic
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

// memberArrayInfo returns element type/size for a MemberAccess node when
// the member is an array, or ok=false. Single source for array-ness.
func (is *InstructionSelector) memberArrayInfo(target *ASTNode) (elemType string, elemSize int, ok bool) {
	if target.Type != NodeMemberAccess || len(target.Children) < 1 {
		return "", 0, false
	}
	baseNode := target.Children[0]
	var structType string
	if baseNode.Type == NodeIdentifier {
		varName := baseNode.VarName
		if sym, found := is.localVars[varName]; found {
			structType = sym.Type
		} else if sym, found := is.globalVars[varName]; found {
			structType = sym.Type
		} else {
			return "", 0, false
		}
	} else {
		// Complex base (array element, nested member): recover the
		// struct type from declarations (e.g. data->meshes[i]).
		structType = is.structTypeOfBase(baseNode)
		if structType == "" {
			return "", 0, false
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
	def, found := is.structs[structName]
	if !found {
		return "", 0, false
	}
	if os.Getenv("SEADBG") != "" {
		fmt.Fprintf(os.Stderr, "memberArrayInfo struct=%q member=%q nmembers=%d\n", structName, target.MemberName, len(def.Members))
	}
	for _, m := range def.Members {
		if os.Getenv("SEADBG") != "" {
			fmt.Fprintf(os.Stderr, "  member %q type=%q arrayLen=%d size=%d\n", m.Name, m.Type, m.ArrayLen, m.Size)
		}
		if m.Name == target.MemberName && m.ArrayLen > 0 {
			et := m.Type
			sz := is.getTypeSize(et)
			if sz <= 0 {
				sz = 8
			}
			return et, sz, true
		}
	}
	return "", 0, false
}

// flattenMultiDim handles a[i][j]... nests on multi-dim arrays. It walks
// the access chain to an identifier root, evaluates each index once
// (source order), and returns the element address computation parts:
// root address operand, total byte offset operand, element type/size, and
// whether this access is the last level (value) or yields a row address.
// ok=false means "not a multi-dim nest" (caller uses legacy paths).
func (is *InstructionSelector) flattenMultiDim(node *ASTNode) (rootAddr, totalOff *Operand, elemType string, elemSize int, isLast bool, ok bool) {
	type levelIdx struct {
		idx   *ASTNode
		level int
	}
	var chain []levelIdx
	cur := node
	for cur != nil && cur.Type == NodeArrayAccess && len(cur.Children) == 2 {
		chain = append(chain, levelIdx{idx: cur.Children[1]})
		cur = cur.Children[0]
	}
	// chain[0] is currently outermost; reverse so index 0 is root-most.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	for k := range chain {
		chain[k].level = k
	}
	// Root must be an identifier or a struct member, both with 2+ dims.
	var dims []int
	if cur.Type == NodeIdentifier {
		var sym *Symbol
		if s, found := is.localVars[cur.VarName]; found {
			sym = s
		} else if s, found := is.globalVars[cur.VarName]; found {
			sym = s
		} else {
			return nil, nil, "", 0, false, false
		}
		if len(sym.Dims) < 2 {
			return nil, nil, "", 0, false, false
		}
		dims = sym.Dims
		if sym.ArraySize > 0 {
			elemType = sym.Type
		} else {
			elemType = strings.TrimSuffix(strings.TrimSpace(sym.Type), "*")
		}
		var err error
		if sym.ArraySize > 0 {
			var baseOff int
			var isG bool
			if sym.IsGlobal {
				baseOff, isG = 0, true
			} else {
				baseOff, isG = sym.Offset, false
			}
			rootAddr = is.newTemp()
			is.emit(OpLoadAddr, rootAddr, &Operand{Type: "var", Value: cur.VarName, Offset: baseOff, IsGlobal: isG}, nil)
		} else {
			rootAddr, err = is.selectExpression(cur)
			if err != nil {
				return nil, nil, "", 0, false, false
			}
		}
	} else if cur.Type == NodeMemberAccess && len(cur.Children) >= 1 {
		// Struct member arrays: m.nodew[i][j], arr[k].nodew[i][j],
		// p->nodew[i][j]. Member dims come from the struct definition;
		// the row address comes from the member lvalue's address. Interior
		// levels must yield the row address — the legacy path would load
		// a scalar element and index through its bits.
		baseT := is.structTypeOf(cur.Children[0])
		if baseT == "" {
			return nil, nil, "", 0, false, false
		}
		def, _, found := is.lookupMember(baseT, cur.MemberName)
		if !found {
			return nil, nil, "", 0, false, false
		}
		for _, m := range def.Members {
			if m.Name == cur.MemberName {
				dims = m.Dims
				elemType = m.Type
				break
			}
		}
		if len(dims) < 2 {
			return nil, nil, "", 0, false, false
		}
		addr, _, aok, aerr := is.lvalueAddrSize(cur)
		if aerr != nil || !aok {
			return nil, nil, "", 0, false, false
		}
		rootAddr = addr
	} else {
		return nil, nil, "", 0, false, false
	}
	elemSize = is.getTypeSize(elemType)
	if elemSize <= 0 {
		elemSize = 8
	}
	// Stride per level: elemSize * product of later dims.
	totalOff = nil
	for _, li := range chain {
		stride := elemSize
		for _, d := range dims[li.level+1:] {
			stride *= d
		}
		idxVal, ierr := is.selectExpression(li.idx)
		if ierr != nil {
			return nil, nil, "", 0, false, false
		}
		term := is.newTemp()
		is.emit(OpMul, term, idxVal, &Operand{Type: "imm", Value: fmt.Sprintf("%d", stride)})
		if totalOff == nil {
			totalOff = term
		} else {
			acc := is.newTemp()
			is.emit(OpAdd, acc, totalOff, term)
			totalOff = acc
		}
	}
	isLast = len(chain) == len(dims)
	return rootAddr, totalOff, elemType, elemSize, isLast, true
}

// sizeofStatic computes sizeof for an expression node without evaluating
// it: identifiers (arrays give totals), member access, *p dereference,
// else legacy DataType sizing (4 fallback, as before).
func (is *InstructionSelector) sizeofStatic(sub *ASTNode) int {
	if sub.Type == NodeIdentifier {
		if sym, found := is.localVars[sub.VarName]; found {
			if sym.ArraySize > 0 {
				return sym.Size
			}
			if s := is.getTypeSize(sym.Type); s > 0 {
				return s
			}
		} else if sym, found := is.globalVars[sub.VarName]; found {
			if sym.ArraySize > 0 {
				return sym.Size
			}
			if s := is.getTypeSize(sym.Type); s > 0 {
				return s
			}
		} else if _, found := is.functions[sub.VarName]; found {
			return 8 // function designator decays to pointer
		}
	}
	if sub.Type == NodeMemberAccess && len(sub.Children) > 0 {
		if _, size, ok := is.memberTypeSize(sub); ok {
			return size
		}
	}
	if sub.Type == NodeArrayAccess && len(sub.Children) >= 2 {
		baseNode := sub.Children[0]
		if baseNode.Type == NodeIdentifier {
			if sym, found := is.localVars[baseNode.VarName]; found {
				et, es := is.arrayElemSize(sym)
				_ = et
				if es > 0 {
					return es
				}
			} else if sym, found := is.globalVars[baseNode.VarName]; found {
				et, es := is.arrayElemSize(sym)
				_ = et
				if es > 0 {
					return es
				}
			}
		} else {
			// Complex base: infer element size from pointer type.
			if baseNode.DataType != "" {
				if es := is.elemSizeForPtr(baseNode.DataType); es > 0 {
					return es
				}
			}
		}
	}
	if sub.Type == NodeUnaryOp && sub.Operator == "*" && len(sub.Children) > 0 {
		if inner := sub.Children[0]; inner.Type == NodeIdentifier {
			if sym, found := is.localVars[inner.VarName]; found {
				return is.elemSizeForPtr(sym.Type)
			}
			if sym, found := is.globalVars[inner.VarName]; found {
				return is.elemSizeForPtr(sym.Type)
			}
		}
	}
	if sub.DataType != "" {
		if s := is.getTypeSize(sub.DataType); s > 0 {
			return s
		}
	}
	return 4
}

// memberTypeSize returns the declared (type, size) of a member access.
func (is *InstructionSelector) memberTypeSize(target *ASTNode) (string, int, bool) {
	if target.Type != NodeMemberAccess || len(target.Children) < 1 {
		return "", 0, false
	}
	baseNode := target.Children[0]
	if baseNode.Type != NodeIdentifier {
		return "", 0, false
	}
	var structType string
	if sym, found := is.localVars[baseNode.VarName]; found {
		structType = sym.Type
	} else if sym, found := is.globalVars[baseNode.VarName]; found {
		structType = sym.Type
	} else {
		return "", 0, false
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
	def, found := is.structs[structName]
	if !found {
		return "", 0, false
	}
	for _, m := range def.Members {
		if m.Name == target.MemberName {
			return m.Type, m.Size, true
		}
	}
	return "", 0, false
}

// structTypeOf resolves the struct (or element) type of an lvalue-ish
// expression: identifier, member access (any depth), or array access.
// Returns "" if unknown. AST strictly shrinks, so recursion terminates.
func (is *InstructionSelector) structTypeOf(node *ASTNode) string {
	switch node.Type {
	case NodeUnaryOp:
		// &E has the pointer-to type of E (e.g. (&p[0])->y0).
		if node.Operator == "&" && len(node.Children) > 0 {
			if inner := is.structTypeOf(node.Children[0]); inner != "" {
				return inner + "*"
			}
		}
		return ""
	case NodeIdentifier:
		if sym, ok := is.localVars[node.VarName]; ok {
			return sym.Type
		}
		if sym, ok := is.globalVars[node.VarName]; ok {
			return sym.Type
		}
		return ""
	case NodeMemberAccess:
		if len(node.Children) < 1 {
			return ""
		}
		baseT := is.resolveType(is.structTypeOf(node.Children[0]))
		if baseT == "" {
			return ""
		}
		if def, mtype, ok := is.lookupMember(baseT, node.MemberName); ok {
			_ = def
			return mtype
		}
		return ""
	case NodeArrayAccess:
		if len(node.Children) < 1 {
			return ""
		}
		inner := node.Children[0]
		if inner.Type == NodeIdentifier {
			name := inner.VarName
			if sym, ok := is.localVars[name]; ok {
				if et, _ := is.arrayElemSize(sym); et != "" {
					return et
				}
			}
			if sym, ok := is.globalVars[name]; ok {
				if et, _ := is.arrayElemSize(sym); et != "" {
					return et
				}
			}
			return ""
		}
		arrT := is.resolveType(is.structTypeOf(inner))
		if arrT == "" {
			return ""
		}
		// Element type: strip trailing [N] dims, else one * level.
		t := strings.TrimSpace(arrT)
		stripped := t
		for strings.HasSuffix(stripped, "]") {
			if idx := strings.LastIndex(stripped, "["); idx >= 0 {
				stripped = strings.TrimSpace(stripped[:idx])
			} else {
				break
			}
		}
		if stripped != t {
			return stripped
		}
		if strings.HasSuffix(stripped, "*") {
			return strings.TrimSpace(strings.TrimSuffix(stripped, "*"))
		}
		return stripped
	}
	return ""
}

// lookupMember finds a member's declared type in a (possibly qualified or
// pointer) struct type. Returns (def, memberType, true) if found.
func (is *InstructionSelector) lookupMember(structType, member string) (*StructDef, string, bool) {
	t := is.resolveType(structType)
	for len(t) > 0 && t[len(t)-1] == '*' {
		t = t[:len(t)-1]
	}
	t = strings.TrimSpace(t)
	for _, q := range []string{"const ", "volatile ", "static ", "extern "} {
		t = strings.TrimSpace(strings.TrimPrefix(t, q))
	}
	if len(t) > 7 && t[:7] == "struct " {
		t = t[7:]
	} else if len(t) > 6 && t[:6] == "union " {
		t = t[6:]
	}
	t = strings.TrimSpace(t)
	if def, ok := is.structs[t]; ok {
		for _, m := range def.Members {
			if m.Name == member {
				return def, m.Type, true
			}
		}
	}
	return nil, "", false
}

// memberAccessType resolves the declared type of a member access with an
// identifier root (nesting/array aware). "" if unknown.
func (is *InstructionSelector) memberAccessType(node *ASTNode) string {
	if node.Type != NodeMemberAccess || len(node.Children) < 1 {
		return ""
	}
	base := node.Children[0]
	var structName string
	if base.Type == NodeIdentifier {
		if sym, ok := is.localVars[base.VarName]; ok {
			structName = sym.Type
		} else if sym, ok := is.globalVars[base.VarName]; ok {
			structName = sym.Type
		}
	} else {
		structName = is.structTypeOfBase(base)
	}
	structName = is.resolveType(structName)
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
	if def, ok := is.structs[structName]; ok {
		for _, m := range def.Members {
			if m.Name == node.MemberName {
				return m.Type
			}
		}
	}
	return ""
}

// structTypeOfBase recovers the struct type of a computed member-access
// base. Delegates to the unified structTypeOf resolver.
func (is *InstructionSelector) structTypeOfBase(baseNode *ASTNode) string {
	return is.structTypeOf(baseNode)
}

// arrayElemHomeType guesses an array element type from the base AST
// without evaluating it (for store conversions ordered before address
// computation). "" if unknown.
func (is *InstructionSelector) arrayElemHomeType(baseNode *ASTNode) string {
	if baseNode.Type == NodeIdentifier {
		if sym, ok := is.localVars[baseNode.VarName]; ok {
			if et, _ := is.arrayElemSize(sym); et != "" {
				return et
			}
		} else if sym, ok := is.globalVars[baseNode.VarName]; ok {
			if et, _ := is.arrayElemSize(sym); et != "" {
				return et
			}
		}
		return ""
	}
	if baseNode.Type == NodeMemberAccess {
		if t, _, ok := is.memberTypeSize(baseNode); ok {
			t = strings.TrimSpace(t)
			// Array member: element type directly; pointer member:
			// one level down.
			if strings.HasSuffix(t, "*") {
				t = strings.TrimSpace(strings.TrimSuffix(t, "*"))
				if t == "" || strings.HasSuffix(t, "*") {
					return ""
				}
			}
			return t
		}
		return ""
	}
	if baseNode.Type == NodeUnaryOp && baseNode.Operator == "*" && len(baseNode.Children) > 0 {
		return is.derefHomeType(baseNode.Children[0])
	}
	return ""
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
		} else if !isPtr {
			// Dot on a complex base needs the base ADDRESS (array
			// element, deref, nested member) — a value load would be
			// meaningless (structs aren't values).
			if addr, _, ok, _ := is.lvalueAddrSize(baseNode); ok {
				baseTemp = addr
				structType = baseNode.DataType
				// Parser often leaves DataType empty on computed bases;
				// recover the struct type from declarations.
				if structType == "" {
					structType = is.structTypeOfBase(baseNode)
				}
			} else {
				return nil, 0, false, nil
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
			// Ident base (address/value per branch above) or complex
			// dot base resolved to its address by lvalueAddrSize, or
			// complex arrow base value (pointer). All are valid here.
			ptrTemp = baseTemp
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
		// Complex base: member arrays decay to address, else evaluate.
		var baseAddr *Operand
		if et, _, ok := is.memberArrayInfo(baseNode); ok {
			addr, _, aok, aerr := is.lvalueAddrSize(baseNode)
			if aerr != nil {
				return nil, 0, false, aerr
			}
			if !aok {
				return nil, 0, false, fmt.Errorf("cannot take address of array base")
			}
			addr.DataType = et + "*"
			baseAddr = addr
		} else {
			var berr error
			baseAddr, berr = is.selectExpression(baseNode)
			if berr != nil {
				return nil, 0, false, berr
			}
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

// derefHomeType guesses the pointee type for *E without evaluating E:
// identifiers via symtab, member access via struct lookup. "" if unknown
// (caller falls back to unconverted stores, as before).
func (is *InstructionSelector) derefHomeType(inner *ASTNode) string {
	trimPtr := func(t string) string {
		rt := is.resolveType(strings.TrimSpace(t))
		if !strings.HasSuffix(rt, "*") {
			return ""
		}
		elem := strings.TrimSpace(strings.TrimSuffix(rt, "*"))
		if elem == "" || strings.HasSuffix(elem, "*") {
			return ""
		}
		return elem
	}
	if inner.Type == NodeIdentifier {
		if sym, ok := is.localVars[inner.VarName]; ok {
			return trimPtr(sym.Type)
		}
		if sym, ok := is.globalVars[inner.VarName]; ok {
			return trimPtr(sym.Type)
		}
	}
	if inner.Type == NodeMemberAccess {
		if t, _, ok := is.memberTypeSize(inner); ok {
			return trimPtr(t)
		}
	}
	return ""
}

// convertForHome wraps a value in an OpCvt when its kind differs from the
// destination type (implicit conversions on stores). Pointers, unknowns,
// and same-kind pairs pass through untouched. Idempotent: after one
// conversion the types agree, so re-application is a no-op.
func (is *InstructionSelector) convertForHome(v *Operand, homeType string) *Operand {
	if v == nil || homeType == "" {
		return v
	}
	// Pointer-to-pointer casts (char*)p, (int*)p: same address, new type.
	// Retype (no code) so later scaling/member logic uses the new element
	// size. Without this, (char*)chunk + 8 scales by 8 (chunk size).
	if strings.Contains(homeType, "*") {
		// Retype unconditionally (emits no code): casts to a pointer type
		// are address passthroughs even when the source is an integer
		// ((Node*)(intptr_t)expr). Later deref/member/scaling logic needs
		// the pointer type to resolve the pointee struct.
		if v.DataType != homeType {
			v.DataType = homeType
		}
		return v
	}
	// Address constants (string literals, function names — Operand Type
	// "label") are address VALUES, not lvalues: integer conversions must
	// not load from the label. (intptr_t)"" used to compile to movslq of
	// the literal's first four bytes (bell's search field then held rodata
	// junk like 'NUR\0' → strcmp ran off unmapped memory).
	if v.Type == "label" {
		return v
	}
	if mode := cvtMode(v.DataType, homeType); mode != "" {
		c := is.newTemp()
		c.DataType = homeType
		is.emit(OpCvt, c, v, &Operand{Type: "imm", Value: mode})
		return c
	}
	// Narrowing int casts (int->char/short): low bits already correct
	// (little-endian); just retype so later widening sign-extends right.
	// Without this, (char)0x80 stays int 128 instead of becoming -128.
	if homeType != "" && v != nil {
		srcR0 := is.resolveType(v.DataType)
		homeR0 := is.resolveType(homeType)
		if !strings.Contains(srcR0, "*") && !strings.Contains(homeR0, "*") {
			if ss, ok := cSizeOf(srcR0); ok && ss > 2 {
				if ds, ok := cSizeOf(homeR0); ok && (ds == 1 || ds == 2) {
					v.DataType = homeType
					return v
				}
			}
		}
	}
	// 8/16->32-bit int widening (value-preserving): signed sign-extends
	// (movsbl/movswl), unsigned zero-extends. Without this, (int)(char)0x80
	// gives 128 instead of -128. Narrower sources promote through int.
	if homeType != "" && v != nil {
		srcR2 := is.resolveType(v.DataType)
		homeR2 := is.resolveType(homeType)
		if !strings.Contains(srcR2, "*") && !strings.Contains(homeR2, "*") {
			if s, ok := cSizeOf(srcR2); ok && (s == 1 || s == 2) {
				if d, ok := cSizeOf(homeR2); ok && d == 4 {
					c := is.newTemp()
					c.DataType = homeType
					// Sub-int unsigned check is textual (isUnsignedIntType
					// requires int rank and misses unsigned char/short).
					if strings.Contains(srcR2, "unsigned") {
						is.emit(OpCvt, c, v, &Operand{Type: "imm", Value: "u82i"})
					} else {
						is.emit(OpCvt, c, v, &Operand{Type: "imm", Value: "s82i"})
					}
					return c
				}
			}
		}
	}
	// 32->64-bit int widening (value-preserving): signed sign-extends,
	// unsigned zero-extends. Without this, int -1 widened to +4294967295
	// (glyph -1 sentinels, negative offsets). Narrower sources promote
	// through int first (handled by their 32-bit loads).
	if homeType != "" && v != nil {
		srcR := is.resolveType(v.DataType)
		homeR := is.resolveType(homeType)
		if !strings.Contains(srcR, "*") && !strings.Contains(homeR, "*") {
			if s, ok := cSizeOf(srcR); ok && s == 4 {
				if d, ok := cSizeOf(homeR); ok && d == 8 {
					c := is.newTemp()
					c.DataType = homeType
					if is.isUnsignedIntType(v.DataType) {
						is.emit(OpCvt, c, v, &Operand{Type: "imm", Value: "u2l"})
					} else {
						is.emit(OpCvt, c, v, &Operand{Type: "imm", Value: "i2l"})
					}
					return c
				}
			}
		}
	}
	return v
}

// storeHomeType returns the declared type of a store destination home
// (frame slot, global, or typed pointer/array element), or "" when unknown
// or pointer-typed (bitwise stores are correct there: addresses are ints).
func (is *InstructionSelector) storeHomeType(dst *Operand) string {
	if dst.Type == "ptr" || dst.Type == "array" {
		if dst.DataType == "" || strings.Contains(dst.DataType, "*") {
			return ""
		}
		return dst.DataType
	}
	if dst.Type != "var" && dst.Type != "mem" {
		return ""
	}
	var home string
	if dst.IsGlobal {
		if dst.Value == "" {
			return ""
		}
		sym, ok := is.globalVars[dst.Value]
		if !ok {
			return ""
		}
		home = sym.Type
	} else {
		if dst.Offset >= 0 {
			return ""
		}
		var ok bool
		home, ok = is.slotTypes[dst.Offset]
		if !ok {
			return ""
		}
	}
	if strings.Contains(home, "*") || home == "" {
		return ""
	}
	return home
}

func (is *InstructionSelector) emit(op OpCode, dst, src1, src2 *Operand) {
	// Stamp declared widths onto frame operands: any var/mem operand with
	// Stamp declared widths onto frame operands: any var/mem operand with
	// no explicit Size that addresses the current frame (negative offset)
	// gets the declared slot size, so the emitter uses movl/movw/movb for
	// sub-8-byte locals instead of 8-byte movq (which read/wrote neighbors).
	for _, op := range []*Operand{dst, src1, src2} {
		if op == nil {
			continue
		}
		// Stamp declared type (for signed loads) alongside width.
		if op.DataType == "" {
			if op.IsGlobal {
				if (op.Type == "var" || op.Type == "mem") && op.Value != "" {
					if sym, ok := is.globalVars[op.Value]; ok {
						op.DataType = sym.Type
					}
				}
			} else if (op.Type == "var" || op.Type == "mem") && op.Offset < 0 {
				if t, ok := is.slotTypes[op.Offset]; ok {
					op.DataType = t
				}
			}
		}
		if op.Size != 0 {
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

	// Check for union types (definitions live in the same table as
	// structs). Missing this made union locals get the 8-byte default
	// slot while sizeof() reported the real size: SDL_Event (128B)
	// locals smashed the frame when SDL wrote a full event into them.
	if len(typ) > 6 && typ[:6] == "union " {
		unionName := strings.TrimSpace(typ[6:])
		if unionDef, ok := is.structs[unionName]; ok {
			return unionDef.Size
		}
		return 8 // Default union size
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

	// Check if it's a struct or union type
	structName := typ
	if len(typ) > 7 && typ[:7] == "struct " {
		structName = typ[7:]
	} else if len(typ) > 6 && typ[:6] == "union " {
		structName = strings.TrimSpace(typ[6:])
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
		// Normalize return type: drop storage-class qualifiers so downstream
		// type checks (float detection etc.) see "float", not "static float".
		rt := strings.TrimSpace(node.ReturnType)
		for _, q := range []string{"static ", "inline ", "extern ", "const ", "volatile ", "register "} {
			for strings.HasPrefix(rt, q) {
				rt = strings.TrimSpace(rt[len(q):])
			}
		}
		is.functions[node.Name] = &FunctionSignature{
			ReturnType: rt,
			ParamTypes: node.ParamTypes,
		}

		if is.funcDefined == nil {
			is.funcDefined = make(map[string]bool)
		}
		// Definitions (have bodies) use sea conventions; bare
		// declarations are external (SysV).
		if len(node.Children) > 0 {
			is.funcDefined[node.Name] = true
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
		// Reserve the callee-saved push area ([rbp-8..rbp-40] for
		// RBX/R12-R15) BEFORE any locals: prologue pushes would
		// otherwise collide with low local slots (clobbered saves).
		// Always reserved (unused space is just padding); the emitter
		// accounts actual pushes in its alignment math.
		is.stackOffset = -40
		is.varCounter = 0 // Reset counter for each function
		is.slotSizes = make(map[int]int)
		is.slotTypes = make(map[int]string)
		is.labelMap = make(map[string]string)

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

		// Allocate parameters. Structs over 8 bytes arrive split (9-16:
		// chunk regs) or indirect (>16: pointer to caller data, copied
		// here into a real local so member paths work unchanged).
		// Allocate parameters. TEMP-FREE prologue: every save is a
		// direct reg->slot move (no allocated temps, which could alias
		// still-live param regs or unsaved callee-saved regs this early).
		// Struct copies run in a second pass, when only stack slots are
		// live (callee-saved temps are covered by usedRegisters saves).
		argRegs := []string{"rdi", "rsi", "rdx", "rcx", "r8", "r9"}
		floatRegs := []string{"xmm0", "xmm1", "xmm2", "xmm3", "xmm4", "xmm5", "xmm6", "xmm7"}
		regIdx := paramRegStartIdx
		floatRegIdx := 0
		// Shared SysV stack-arg slot counter (both classes, in order).
		stackOff := 0
		type pendingCopy struct {
			base    int
			ptrSlot int
			size    int
		}
		var pending []pendingCopy
		for i, param := range node.Params {
			paramType := ""
			if i < len(node.ParamTypes) {
				paramType = node.ParamTypes[i]
			}
			if sz := is.structDefSize(paramType); sz > 8 {
				if sz > 16 {
					// Indirect: save the pointer reg to a slot now;
					// copy in the second pass.
					is.stackOffset -= sz
					// Round DOWN (more negative) to an 8-byte boundary.
					// Go's % keeps the sign of the dividend, so a naive
					// `stackOffset -= stackOffset % 8` rounds UP for
					// negative offsets and overlaps the following slot
					// (e.g. a 44-byte struct at -88 followed by an
					// 8-byte __retptr at -48 overlapped by 4 bytes).
					for is.stackOffset%8 != 0 {
						is.stackOffset--
					}
					base := is.stackOffset
					is.stackOffset -= 8
					ptrSlot := is.stackOffset
					is.slotSizes[base] = sz
					is.slotTypes[base] = paramType
					is.localVars[param] = &Symbol{Name: param, Type: paramType, Offset: base, Size: sz}
					if regIdx < len(argRegs) {
						is.emit(OpStore, &Operand{Type: "mem", Offset: ptrSlot}, &Operand{Type: "reg", Value: argRegs[regIdx]}, nil)
						regIdx++
					} else {
						is.emit(OpLoad, &Operand{Type: "mem", Offset: ptrSlot}, &Operand{Type: "prm", Offset: 16 + stackOff*8}, nil)
						stackOff++
					}
					pending = append(pending, pendingCopy{base: base, ptrSlot: ptrSlot, size: sz})
					continue
				}
				// 9-16 bytes: chunk regs straight into a 16-byte local
				// (no copy needed, no temps).
				sname := is.structBaseName(paramType)
				chunks := is.structChunkClasses(sname)
				for len(chunks) < 2 {
					chunks = append(chunks, "int")
				}
				is.stackOffset -= 16
				base := is.stackOffset
				is.slotSizes[base] = sz
				is.slotTypes[base] = paramType
				is.localVars[param] = &Symbol{Name: param, Type: paramType, Offset: base, Size: sz}
				for ci, cl := range chunks {
					dst := &Operand{Type: "mem", Offset: base + ci*8}
					if cl == "sse" {
						if floatRegIdx < len(floatRegs) {
							is.emit(OpMov, dst, &Operand{Type: "freg", Value: floatRegs[floatRegIdx]}, nil)
							floatRegIdx++
						} else {
							is.emit(OpLoad, dst, &Operand{Type: "prm", Offset: 16 + stackOff*8}, nil)
							stackOff++
						}
					} else {
						if regIdx < len(argRegs) {
							is.emit(OpStore, dst, &Operand{Type: "reg", Value: argRegs[regIdx]}, nil)
							regIdx++
						} else {
							is.emit(OpLoad, dst, &Operand{Type: "prm", Offset: 16 + stackOff*8}, nil)
							stackOff++
						}
					}
				}
				continue
			}
			is.stackOffset -= 8
			// Width-correct slot: int params occupy 4 bytes (see slotSizes).
			paramSize := 8
			if paramType != "" {
				if s := is.getTypeSize(paramType); s > 0 {
					paramSize = s
				}
			}
			is.slotSizes[is.stackOffset] = paramSize
			is.slotTypes[is.stackOffset] = paramType
			is.localVars[param] = &Symbol{
				Name:   param,
				Type:   paramType,
				Offset: is.stackOffset,
				Size:   8,
			}

			// Direct reg->slot saves (no temps). Floats arrive in xmm
			// (SysV); ints/pointers in int regs.
			isFloatParam := paramType == "float" || paramType == "double"
			if isFloatParam {
				if floatRegIdx < len(floatRegs) {
					is.emit(OpMov, &Operand{Type: "mem", Offset: is.stackOffset}, &Operand{Type: "freg", Value: floatRegs[floatRegIdx]}, nil)
					floatRegIdx++
				} else {
					// Stack float: full 8-byte load (bits in low 32).
					is.emit(OpLoad, &Operand{Type: "mem", Offset: is.stackOffset}, &Operand{Type: "prm", Offset: 16 + stackOff*8}, nil)
					stackOff++
				}
			} else if regIdx < len(argRegs) {
				is.emit(OpStore, &Operand{Type: "mem", Offset: is.stackOffset}, &Operand{Type: "reg", Value: argRegs[regIdx]}, nil)
				regIdx++
			} else {
				// Stack parameter per SysV: 16(%rbp) + 8k.
				is.emit(OpLoad, &Operand{Type: "mem", Offset: is.stackOffset}, &Operand{Type: "prm", Offset: 16 + stackOff*8}, nil)
				stackOff++
				regIdx++
			}
		}
		// Second pass: indirect struct copies (temps safe now: all
		// param regs saved; callee-saved temps covered by usedRegs).
		for _, pc := range pending {
			srcPtr := is.newTemp()
			is.emit(OpLoad, srcPtr, &Operand{Type: "mem", Offset: pc.ptrSlot}, nil)
			dstAddr := is.newTemp()
			is.emit(OpLoadAddr, dstAddr, &Operand{Type: "mem", Offset: pc.base}, nil)
			is.emitStructCopy(dstAddr, srcPtr, pc.size)
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
		varSize := 8 // Default for int/pointer
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
				varSize = 8 // Pointer to struct
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
			varSize = node.ArraySize * varSize // Array: count * element size
		}

		if node.IsGlobal {
			isStatic := strings.HasPrefix(strings.TrimSpace(node.DataType), "static")
			is.globalVars[node.VarName] = &Symbol{
				Name:      node.VarName,
				IsGlobal:  true,
				IsStatic:  isStatic,
				Size:      varSize,
				ArraySize: node.ArraySize,
				Dims:      node.Dims,
				Type:      dataType,
			}
			// Keep constant initializers (e.g. = 5, arrays): the
			// emitter places them in .data instead of zeroed .comm.
			if len(node.Children) > 0 {
				is.globalVars[node.VarName].Init = node.Children[0]
			}
		} else {
			is.stackOffset -= varSize
			varOffset := is.stackOffset // Save the variable's offset

			// Create a unique key for this variable instance
			is.varCounter++
			uniqueKey := fmt.Sprintf("%s#%d", node.VarName, is.varCounter)

			sym := &Symbol{
				Name:      node.VarName, // Keep original name
				Offset:    varOffset,
				Size:      varSize,
				ArraySize: node.ArraySize,
				Dims:      node.Dims,
				Type:      dataType,
			}

			// Store in both maps:
			// - allLocalVars keeps ALL variable instances (prevents offset reuse)
			// - localVars tracks current binding (for lookups)
			is.allLocalVars[uniqueKey] = sym
			is.localVars[node.VarName] = sym
			is.slotSizes[varOffset] = varSize
			is.slotTypes[varOffset] = dataType

			// Handle initialization (only for non-arrays for now)
			if len(node.Children) > 0 && node.ArraySize == 0 {
				// Large-struct hint: only when the init IS an unknown
				// call (its value becomes the struct). Nested calls
				// (mallocs inside compounds) must not inherit it.
				savedHint := is.largeRetHint
				initExpr := node.Children[0]
				if sz := is.structDefSize(dataType); sz > 16 && is.unknownStructCall(initExpr) {
					is.largeRetHint = dataType
				}

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
					// Regular initialization (convert to home type first).
					result, err := is.selectExpression(initExpr)
					if err != nil {
						return err
					}
					// Struct-typed var from a struct value (call, ident,
					// member): full copy, not an 8-byte store.
					if sz := is.structDefSize(dataType); sz > 0 {
						if srcAddr := is.structSrcAddr(initExpr, result); srcAddr != nil {
							dstAddr := is.newTemp()
							is.emit(OpLoadAddr, dstAddr, &Operand{Type: "var", Value: node.VarName, Offset: varOffset}, nil)
							is.emitStructCopy(dstAddr, srcAddr, sz)
						} else {
							result = is.convertForHome(result, dataType)
							varOp := &Operand{Type: "var", Value: node.VarName, Offset: varOffset, Size: varSize}
							is.emit(OpStore, varOp, result, nil)
						}
					} else {
						result = is.convertForHome(result, dataType)
						varOp := &Operand{Type: "var", Value: node.VarName, Offset: varOffset, Size: varSize}
						is.emit(OpStore, varOp, result, nil)
					}
				}
				is.largeRetHint = savedHint
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
							v = is.convertForHome(v, node.DataType)
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
			// Large-struct hint: only when returning an unknown call
			// directly (nested calls inside compounds keep known types).
			savedHint := is.largeRetHint
			if funcSig := is.functions[is.currentFunc]; funcSig != nil {
				if sz := is.structDefSize(funcSig.ReturnType); sz > 16 && len(node.Children) > 0 && is.unknownStructCall(node.Children[0]) {
					is.largeRetHint = funcSig.ReturnType
				}
			}
			result, err := is.selectExpression(node.Children[0])
			is.largeRetHint = savedHint
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

					// Source ADDRESS of the returned struct (result itself
					// is a loaded temp for struct values — not the data).
					retChild := node.Children[0]
					srcAddr := is.structSrcAddr(retChild, result)

					// Copy structSize bytes, width-aware tail (a flat 8-byte
					// copy overruns the destination for non-multiples).
					structSize := is.getTypeSize(funcSig.ReturnType)
					if srcAddr != nil && structSize > 0 {
						for offset := 0; offset < structSize; {
							chunk := 8
							if structSize-offset < 8 {
								chunk = structSize - offset
							}
							srcOp := &Operand{Type: "ptr", IndexTemp: srcAddr, Size: chunk}
							dstOp := &Operand{Type: "ptr", IndexTemp: ptrTemp, Size: chunk}
							if offset > 0 {
								offOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", offset)}
								srcA := is.newTemp()
								is.emit(OpAdd, srcA, srcAddr, offOp)
								srcOp.IndexTemp = srcA
								dstA := is.newTemp()
								is.emit(OpAdd, dstA, ptrTemp, offOp)
								dstOp.IndexTemp = dstA
							}
							srcTemp := is.newTemp()
							is.emit(OpLoad, srcTemp, srcOp, nil)
							is.emit(OpStore, dstOp, srcTemp, nil)
							offset += chunk
						}
					}

					// Return the hidden pointer in RAX
					retReg := &Operand{Type: "reg", Value: "rax"}
					is.emit(OpMov, retReg, ptrTemp, nil)
				}
			} else if funcSig != nil && funcSig.ReturnType != "" && is.structDefSize(funcSig.ReturnType) > 0 {
				// Small struct return (<=16, not large): the value must be
				// IN rax[/rdx], not an address. Compound literals (and
				// other address results) load through.
				sz := is.structDefSize(funcSig.ReturnType)
				srcAddr := is.structSrcAddr(node.Children[0], result)
				if srcAddr == nil {
					// Already a value (identifier load): move as-is.
					retReg := &Operand{Type: "reg", Value: "rax"}
					is.emit(OpMov, retReg, result, nil)
				} else if sz <= 8 {
					v := is.newTemp()
					is.emit(OpLoad, v, &Operand{Type: "ptr", IndexTemp: srcAddr, Size: 8}, nil)
					is.emit(OpMov, &Operand{Type: "reg", Value: "rax"}, v, nil)
				} else {
					// 9-16 bytes: rax = low, rdx = high (SysV + our
					// caller-side reader both expect this split).
					lo := is.newTemp()
					is.emit(OpLoad, lo, &Operand{Type: "ptr", IndexTemp: srcAddr, Size: 8}, nil)
					is.emit(OpMov, &Operand{Type: "reg", Value: "rax"}, lo, nil)
					offOp := &Operand{Type: "imm", Value: "8"}
					hiAddr := is.newTemp()
					is.emit(OpAdd, hiAddr, srcAddr, offOp)
					hi := is.newTemp()
					is.emit(OpLoad, hi, &Operand{Type: "ptr", IndexTemp: hiAddr, Size: 8}, nil)
					is.emit(OpMov, &Operand{Type: "reg", Value: "rdx"}, hi, nil)
				}
			} else if funcSig != nil && funcSig.ReturnType != "" {
				// Narrow sub-int returns (short/char) with C semantics:
				// truncate to width, then extend to 64-bit (signed types
				// sign-extend, so int16 -1 stays -1, not 65535).
				rt := is.resolveType(funcSig.ReturnType)
				narrow := 0
				signedRet := true
				if s, ok := cSizeOf(rt); ok && (s == 1 || s == 2) {
					narrow = s
					signedRet = !strings.Contains(rt, "unsigned")
				}
				if narrow > 0 {
					w := is.newTemp()
					w.DataType = funcSig.ReturnType
					// Truncate via 32-bit move (low bits survive).
					is.emit(OpMov, w, result, nil)
					// Then extend to 64-bit with correct signedness.
					ext := is.newTemp()
					ext.DataType = "long"
					if narrow == 1 {
						if signedRet {
							is.emit(OpCvt, ext, w, &Operand{Type: "imm", Value: "sbl2l"})
						} else {
							is.emit(OpCvt, ext, w, &Operand{Type: "imm", Value: "zbl2l"})
						}
					} else {
						if signedRet {
							is.emit(OpCvt, ext, w, &Operand{Type: "imm", Value: "swl2l"})
						} else {
							is.emit(OpCvt, ext, w, &Operand{Type: "imm", Value: "zwl2l"})
						}
					}
					is.emit(OpMov, &Operand{Type: "reg", Value: "rax"}, ext, nil)
				} else {
					// Regular return: move result to RAX
					retReg := &Operand{Type: "reg", Value: "rax"}
					is.emit(OpMov, retReg, result, nil)
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

		is.breakStack = append(is.breakStack, endLabel)
		is.continueStack = append(is.continueStack, startLabel)
		berr := is.selectNode(node.Children[1])
		is.breakStack = is.breakStack[:len(is.breakStack)-1]
		is.continueStack = is.continueStack[:len(is.continueStack)-1]
		if berr != nil {
			return berr
		}

		is.emit(OpJmp, &Operand{Type: "label", Value: startLabel}, nil, nil)
		is.emit(OpLabel, &Operand{Type: "label", Value: endLabel}, nil, nil)

	case NodeDoWhile:
		// do body while (cond): body runs once, then cond gates repeat.
		// continue jumps to the condition check.
		bodyLabel := is.newLabel(".L_do_body")
		condLabel := is.newLabel(".L_do_cond")
		endLabel := is.newLabel(".L_do_end")

		is.emit(OpLabel, &Operand{Type: "label", Value: bodyLabel}, nil, nil)

		is.breakStack = append(is.breakStack, endLabel)
		is.continueStack = append(is.continueStack, condLabel)
		berr := is.selectNode(node.Children[0])
		is.breakStack = is.breakStack[:len(is.breakStack)-1]
		is.continueStack = is.continueStack[:len(is.continueStack)-1]
		if berr != nil {
			return berr
		}

		is.emit(OpLabel, &Operand{Type: "label", Value: condLabel}, nil, nil)
		cond, err := is.selectExpression(node.Children[1])
		if err != nil {
			return err
		}
		is.emit(OpJnz, &Operand{Type: "label", Value: bodyLabel}, cond, nil)
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
		contLabel := is.newLabel(".L_for_cont")

		is.emit(OpLabel, &Operand{Type: "label", Value: startLabel}, nil, nil)

		if cond != nil {
			condResult, err := is.selectExpression(cond)
			if err != nil {
				return err
			}
			is.emit(OpJz, &Operand{Type: "label", Value: endLabel}, condResult, nil)
		}

		is.breakStack = append(is.breakStack, endLabel)
		is.continueStack = append(is.continueStack, contLabel)
		var berr error
		if body != nil {
			berr = is.selectNode(body)
		}
		is.breakStack = is.breakStack[:len(is.breakStack)-1]
		is.continueStack = is.continueStack[:len(is.continueStack)-1]
		if berr != nil {
			return berr
		}

		is.emit(OpLabel, &Operand{Type: "label", Value: contLabel}, nil, nil)
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

		// A break inside the switch (even nested in ifs) exits the
		// switch, not an enclosing loop: push the end label.
		is.breakStack = append(is.breakStack, endLabel)
		defer func() { is.breakStack = is.breakStack[:len(is.breakStack)-1] }()

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
		// Innermost switch/loop end (switch pushes its end too, so a
		// break inside a switch-in-loop exits the switch, per C).
		if len(is.breakStack) > 0 {
			is.emit(OpJmp, &Operand{Type: "label", Value: is.breakStack[len(is.breakStack)-1]}, nil, nil)
		}
		return nil

	case NodeGoto:
		// Unconditional jump to a function-scoped label (forward refs
		// resolve at assemble time; GAS handles them).
		lbl, ok := is.labelMap[node.Value]
		if !ok {
			lbl = is.newLabel(".L_goto")
			is.labelMap[node.Value] = lbl
		}
		is.emit(OpJmp, &Operand{Type: "label", Value: lbl}, nil, nil)
		return nil

	case NodeLabel:
		lbl, ok := is.labelMap[node.Value]
		if !ok {
			lbl = is.newLabel(".L_goto")
			is.labelMap[node.Value] = lbl
		}
		is.emit(OpLabel, &Operand{Type: "label", Value: lbl}, nil, nil)
		return nil

	case NodeContinue:
		if len(is.continueStack) > 0 {
			is.emit(OpJmp, &Operand{Type: "label", Value: is.continueStack[len(is.continueStack)-1]}, nil, nil)
		}
		return nil

	default:
		// Expression as statement
		_, err := is.selectExpression(node)
		return err
	}

	return nil
}

// propagateArithType sets an arithmetic binary-op result type (C usual
// conversions, LP64): double > float > 64-bit/unsigned int rules. Callers
// handle comparisons/logicals (always int) separately.
func propagateArithType(is *InstructionSelector, node *ASTNode, result, left, right *Operand) {
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
		result.DataType = promoteToInt(left.DataType)
	} else if right.DataType != "" {
		result.DataType = promoteToInt(right.DataType)
	}
}

// promoteToInt applies C integer promotion: char/short (signed or
// unsigned) become int. Without this, char*256 truncated to char (0).
func promoteToInt(t string) string {
	t = strings.TrimSpace(t)
	for _, q := range []string{"const ", "volatile ", "static ", "extern ", "register "} {
		t = strings.TrimSpace(strings.TrimPrefix(t, q))
	}
	if s, ok := cSizeOf(t); ok && s < 4 && !strings.Contains(t, "*") {
		return "int"
	}
	return t
}

// selectLogical lowers && / || with jumps so the right side runs only
// when needed (short-circuit). Result is always int 0/1.
func (is *InstructionSelector) selectLogical(node *ASTNode) (*Operand, error) {
	if len(node.Children) < 2 {
		return nil, fmt.Errorf("logical operator needs 2 operands")
	}
	left, err := is.selectExpression(node.Children[0])
	if err != nil {
		return nil, err
	}
	result := is.newTemp()
	result.DataType = "int"
	zero := &Operand{Type: "imm", Value: "0"}
	one := &Operand{Type: "imm", Value: "1"}
	if node.Operator == "&&" {
		falseLbl := is.newLabel(".L_and_false")
		endLbl := is.newLabel(".L_and_end")
		is.emit(OpJz, &Operand{Type: "label", Value: falseLbl}, left, nil)
		right, err := is.selectExpression(node.Children[1])
		if err != nil {
			return nil, err
		}
		is.emit(OpJz, &Operand{Type: "label", Value: falseLbl}, right, nil)
		is.emit(OpMov, result, one, nil)
		is.emit(OpJmp, &Operand{Type: "label", Value: endLbl}, nil, nil)
		is.emit(OpLabel, &Operand{Type: "label", Value: falseLbl}, nil, nil)
		is.emit(OpMov, result, zero, nil)
		is.emit(OpLabel, &Operand{Type: "label", Value: endLbl}, nil, nil)
	} else {
		trueLbl := is.newLabel(".L_or_true")
		endLbl := is.newLabel(".L_or_end")
		is.emit(OpJnz, &Operand{Type: "label", Value: trueLbl}, left, nil)
		right, err := is.selectExpression(node.Children[1])
		if err != nil {
			return nil, err
		}
		is.emit(OpJnz, &Operand{Type: "label", Value: trueLbl}, right, nil)
		is.emit(OpMov, result, zero, nil)
		is.emit(OpJmp, &Operand{Type: "label", Value: endLbl}, nil, nil)
		is.emit(OpLabel, &Operand{Type: "label", Value: trueLbl}, nil, nil)
		is.emit(OpMov, result, one, nil)
		is.emit(OpLabel, &Operand{Type: "label", Value: endLbl}, nil, nil)
	}
	return result, nil
}

// libcFloatFuncs names libc math functions taking only float params
// (headers skipped, so no signatures). Callers pass float bits for these
// (SysV); unknown functions default to double promotion (varargs-safe).
var libcFloatFuncs = map[string]bool{
	"sinf": true, "cosf": true, "tanf": true, "asinf": true,
	"acosf": true, "atanf": true, "atan2f": true, "sinhf": true,
	"coshf": true, "tanhf": true, "asinhf": true, "acoshf": true,
	"atanhf": true, "expf": true, "exp2f": true, "expm1f": true,
	"logf": true, "log10f": true, "log2f": true, "log1pf": true,
	"powf": true, "sqrtf": true, "cbrtf": true, "hypotf": true,
	"erff": true, "erfcf": true, "lgammaf": true, "tgammaf": true,
	"ceilf": true, "floorf": true, "nearbyintf": true, "rintf": true,
	"roundf": true, "truncf": true, "fmodf": true, "remainderf": true,
	"fminf": true, "fmaxf": true, "fdimf": true, "fabsf": true,
	"copysignf": true, "fmaf": true,
}

// libcDoubleRet names libc math functions returning double (headers
// skipped). External double returns live in xmm0 (SysV), not rax.
var libcDoubleRet = map[string]bool{
	"sin": true, "cos": true, "tan": true, "asin": true,
	"acos": true, "atan": true, "atan2": true, "sinh": true,
	"cosh": true, "tanh": true, "exp": true, "exp2": true,
	"log": true, "log10": true, "log2": true, "pow": true,
	"sqrt": true, "cbrt": true, "hypot": true, "erf": true,
	"erfc": true, "ceil": true, "floor": true, "round": true,
	"trunc": true, "fmod": true, "fmin": true, "fmax": true,
	"fabs": true, "fma": true,
}

// unknownStructCall returns true when node is (or wraps) a call whose
// return type is unknown AND which could produce a large struct: direct
// unknown calls and indirect calls, but NOT known-small returns (libc
// float/double, known ints) and NOT calls nested inside other
// expressions (their values don't become the outer struct).
func (is *InstructionSelector) unknownStructCall(node *ASTNode) bool {
	for node != nil {
		if node.Type == NodeCall {
			if sig, ok := is.functions[node.Name]; ok && sig.ReturnType != "" {
				return false
			}
			if libcFloatFuncs[node.Name] || libcDoubleRet[node.Name] {
				return false
			}
			return true
		}
		// Unwrap transparent nodes (parens produce nested exprs/casts).
		if (node.Type == NodeCast || node.Type == NodeUnaryOp) && len(node.Children) > 0 {
			node = node.Children[0]
			continue
		}
		return false
	}
	return false
}

// structBaseName strips pointers and qualifiers plus the struct/union
// keyword, yielding the bare definition name for chunk classification.
func (is *InstructionSelector) structBaseName(paramType string) string {
	t := is.resolveType(paramType)
	for len(t) > 0 && t[len(t)-1] == '*' {
		t = t[:len(t)-1]
	}
	t = strings.TrimSpace(t)
	for _, q := range []string{"const ", "volatile ", "static ", "extern "} {
		t = strings.TrimSpace(strings.TrimPrefix(t, q))
	}
	sname := t
	if len(sname) > 7 && sname[:7] == "struct " {
		sname = sname[7:]
	} else if len(sname) > 6 && sname[:6] == "union " {
		sname = sname[6:]
	}
	return strings.TrimSpace(sname)
}

// structChunkClasses classifies each 8-byte chunk of a struct for SysV
// passing: "int" (contains int/pointer bytes) or "sse" (all float/double).
// Padding-only chunks default to int. Used for 9-16 byte structs (passed
// in two regs) on both caller and callee sides.
func (is *InstructionSelector) structChunkClasses(structName string) []string {
	def, ok := is.structs[structName]
	if !ok {
		return nil
	}
	n := (def.Size + 7) / 8
	if n < 1 {
		n = 1
	}
	classes := make([]string, n)
	for i := range classes {
		classes[i] = "int"
	}
	// votes[i] = 1 if chunk i has int bytes, 2 if only float bytes
	votes := make([]int, n)
	for _, m := range def.Members {
		isFloat := strings.Contains(m.Type, "float") || strings.Contains(m.Type, "double")
		for b := m.Offset; b < m.Offset+m.Size; b++ {
			c := b / 8
			if c < 0 || c >= n {
				continue
			}
			if isFloat {
				if votes[c] == 0 {
					votes[c] = 2
				}
			} else {
				votes[c] = 1
			}
		}
	}
	for i := range classes {
		if votes[i] == 2 {
			classes[i] = "sse"
		}
	}
	return classes
}

// externalSmallStructReturn reads a small (<=16-byte) struct returned by
// an EXTERNAL (gcc-compiled) function using the SysV register convention:
// float-only eightbytes come back in xmm0/xmm1, integer eightbytes in
// rax/rdx (each register file numbered in chunk order). The value is
// spilled to a 16-byte stack slot and returned as a mem operand. Sea-
// defined functions use the internal rax/rdx convention instead and must
// not go through this path.
func (is *InstructionSelector) externalSmallStructReturn(returnType string, structSize int) *Operand {
	// Resolve the struct key structChunkClasses expects (typedef chains,
	// qualifiers, pointers, struct/union prefixes).
	t := is.resolveType(returnType)
	for i := 0; i < 4; i++ {
		nxt := is.resolveType(t)
		if nxt == t {
			break
		}
		t = nxt
	}
	if len(t) == 0 || t[len(t)-1] == '*' {
		return nil
	}
	for _, q := range []string{"const ", "volatile ", "static ", "extern "} {
		t = strings.TrimSpace(strings.TrimPrefix(t, q))
	}
	if len(t) > 7 && t[:7] == "struct " {
		t = strings.TrimSpace(t[7:])
	} else if len(t) > 6 && t[:6] == "union " {
		t = strings.TrimSpace(t[6:])
	}
	chunks := is.structChunkClasses(t)
	if len(chunks) == 0 || len(chunks) > 2 {
		return nil
	}
	// Spill the returned chunks to a slot (rbp-relative negative offset,
	// aligned down like the other return slots).
	is.stackOffset -= 16
	for is.stackOffset%16 != 0 {
		is.stackOffset--
	}
	intRegs := []string{"rax", "rdx"}
	sseRegs := []string{"xmm0", "xmm1"}
	intIdx, sseIdx := 0, 0
	for i, cl := range chunks {
		part := &Operand{Type: "mem", Offset: is.stackOffset + i*8}
		if cl == "sse" {
			is.emit(OpStore, part, &Operand{Type: "freg", Value: sseRegs[sseIdx]}, nil)
			sseIdx++
		} else {
			is.emit(OpStore, part, &Operand{Type: "reg", Value: intRegs[intIdx]}, nil)
			intIdx++
		}
	}
	result := is.newTemp()
	result.Type = "mem"
	result.Offset = is.stackOffset
	result.DataType = returnType
	return result
}

// structArgInfo resolves a call argument's struct type/size for passing.
// Returns (structName, size, true) for structs larger than 8 bytes (which
// need special passing); smaller structs and non-structs return false
// (existing value passing handles them).
func (is *InstructionSelector) structArgInfo(argNode *ASTNode, arg *Operand) (string, int, bool) {
	var typ string
	switch argNode.Type {
	case NodeIdentifier:
		if sym, ok := is.localVars[argNode.VarName]; ok {
			// Arrays decay to pointers (passed as plain values).
			if sym.ArraySize > 0 {
				return "", 0, false
			}
			typ = sym.Type
		} else if sym, ok := is.globalVars[argNode.VarName]; ok {
			if sym.ArraySize > 0 {
				return "", 0, false
			}
			typ = sym.Type
		}
	case NodeMemberAccess:
		typ = is.memberAccessType(argNode)
		// Array members decay to pointers (memberArrayInfo path).
		if et, _, ok := is.memberArrayInfo(argNode); ok && et != "" {
			return "", 0, false
		}
	case NodeCall:
		if arg != nil {
			typ = arg.DataType
		}
	case NodeArrayAccess:
		// Element type (e.g. m.meshes[i] is a Mesh). arg.DataType is
		// often the address type (Mesh*), which would wrongly look like
		// a pointer and be passed as a plain 8-byte value.
		typ = is.structTypeOf(argNode)
	case NodeCompoundLiteral:
		typ = argNode.DataType
	}
	if typ == "" {
		if arg != nil {
			typ = arg.DataType
		}
	}
	if typ == "" {
		return "", 0, false
	}
	size := is.structDefSize(typ)
	if size <= 0 {
		return "", 0, false
	}
	// struct name for chunk classification
	t := is.resolveType(typ)
	for len(t) > 0 && t[len(t)-1] == '*' {
		t = t[:len(t)-1]
	}
	t = strings.TrimSpace(t)
	for _, q := range []string{"const ", "volatile ", "static ", "extern "} {
		t = strings.TrimSpace(strings.TrimPrefix(t, q))
	}
	if len(t) > 7 && t[:7] == "struct " {
		t = t[7:]
	} else if len(t) > 6 && t[:6] == "union " {
		t = t[6:]
	}
	return strings.TrimSpace(t), size, true
}

// emitStructCopy copies size bytes from srcAddr to dstAddr in width-aware
// chunks (a flat 8-byte copy overruns non-multiples and reads/writes past
// the end). Both must be address temps.
//
// The bases are spilled to stack slots first and reloaded per chunk:
// keeping them in allocated temps across the loop is unsafe (the
// allocator reused their registers for chunk temporaries mid-loop,
// shifting later chunks). Reloaded bases + reserved scratch have only
// 2-3 instruction live ranges, which allocate safely.
func (is *InstructionSelector) emitStructCopy(dstAddr, srcAddr *Operand, structSize int) {
	is.stackOffset -= 8
	srcSlot := is.stackOffset
	is.stackOffset -= 8
	dstSlot := is.stackOffset
	is.emit(OpStore, &Operand{Type: "mem", Offset: srcSlot}, srcAddr, nil)
	is.emit(OpStore, &Operand{Type: "mem", Offset: dstSlot}, dstAddr, nil)
	// Reserved scratch (never allocated, no liveness issues).
	rax := &Operand{Type: "reg", Value: "rax"}
	r11 := &Operand{Type: "reg", Value: "r11"}
	for offset := 0; offset < structSize; {
		chunk := 8
		if structSize-offset < 8 {
			chunk = structSize - offset
		}
		// rax = src base + offset; tmp = *rax.
		is.emit(OpLoad, rax, &Operand{Type: "mem", Offset: srcSlot}, nil)
		if offset > 0 {
			is.emit(OpAdd, rax, rax, &Operand{Type: "imm", Value: fmt.Sprintf("%d", offset)})
		}
		tmp := is.newTemp()
		is.emit(OpLoad, tmp, &Operand{Type: "ptr", IndexTemp: rax, Size: chunk}, nil)
		// r11 = dst base + offset; *r11 = tmp.
		is.emit(OpLoad, r11, &Operand{Type: "mem", Offset: dstSlot}, nil)
		if offset > 0 {
			is.emit(OpAdd, r11, r11, &Operand{Type: "imm", Value: fmt.Sprintf("%d", offset)})
		}
		is.emit(OpStore, &Operand{Type: "ptr", IndexTemp: r11, Size: chunk}, tmp, nil)
		offset += chunk
	}
}

// structSrcAddr materializes the address of a struct-valued expression:
// identifiers (LoadAddr), member/array (lvalue helper), compound literals
// (their temp IS the address), or mem/var operands naming storage.
func (is *InstructionSelector) structSrcAddr(node *ASTNode, val *Operand) *Operand {
	if node.Type == NodeBlock {
		// Statement expression: selectExpression already materialized the
		// struct's address as the block's value (see the NodeBlock case).
		if val != nil && is.structDefSize(val.DataType) > 0 {
			return val
		}
		return nil
	}
	if node.Type == NodeIdentifier {
		if sym, ok := is.localVars[node.VarName]; ok {
			a := is.newTemp()
			is.emit(OpLoadAddr, a, &Operand{Type: "var", Value: node.VarName, Offset: sym.Offset}, nil)
			return a
		}
		if _, ok := is.globalVars[node.VarName]; ok {
			a := is.newTemp()
			is.emit(OpLoadAddr, a, &Operand{Type: "var", Value: node.VarName, IsGlobal: true}, nil)
			return a
		}
	}
	if node.Type == NodeCompoundLiteral {
		return val
	}
	// Deref and anything else addressable (*mesh, casts of addresses).
	if addr, _, ok, _ := is.lvalueAddrSize(node); ok {
		return addr
	}
	if val != nil && (val.Type == "mem" || val.Type == "var") {
		a := is.newTemp()
		is.emit(OpLoadAddr, a, &Operand{Type: val.Type, Value: val.Value, Offset: val.Offset, IsGlobal: val.IsGlobal}, nil)
		return a
	}
	return nil
}

// structDefSize returns the byte size when typ names a defined struct/union,
// else 0.
func (is *InstructionSelector) structDefSize(typ string) int {
	t := is.resolveType(typ)
	for len(t) > 0 && t[len(t)-1] == '*' {
		return 0
	}
	t = strings.TrimSpace(t)
	for _, q := range []string{"const ", "volatile ", "static ", "extern "} {
		t = strings.TrimSpace(strings.TrimPrefix(t, q))
	}
	if len(t) > 7 && t[:7] == "struct " {
		t = t[7:]
	} else if len(t) > 6 && t[:6] == "union " {
		t = t[6:]
	} else {
		return 0
	}
	t = strings.TrimSpace(t)
	if def, ok := is.structs[t]; ok {
		return def.Size
	}
	return 0
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
		// Logical ops short-circuit (C sequence point): the right side
		// evaluates only when the left doesn't decide. Evaluating both
		// unconditionally crashed `!e || !e[0]` on NULL e.
		if node.Operator == "&&" || node.Operator == "||" {
			return is.selectLogical(node)
		}
		left, err := is.selectExpression(node.Children[0])
		if err != nil {
			return nil, err
		}

		right, err := is.selectExpression(node.Children[1])
		if err != nil {
			return nil, err
		}

		result := is.newTemp()

		// Pointer arithmetic: widen a 32-bit index to 64-bit
		// (value-preserving) and scale by the element size. Without
		// widening, int -1 zero-extends to +4B; without scaling,
		// int_ptr+3 advances 3 bytes, not 3 ints.
		if node.Operator == "+" || node.Operator == "-" {
			leftPtr := strings.Contains(left.DataType, "*")
			rightPtr := strings.Contains(right.DataType, "*")
			if leftPtr != rightPtr {
				var ptrSide *Operand
				if leftPtr {
					right = is.convertForHome(right, "long")
					ptrSide = left
				} else {
					left = is.convertForHome(left, "long")
					ptrSide = right
				}
				if elemSize := is.elemSizeForPtr(ptrSide.DataType); elemSize > 1 {
					scaled := is.newTemp()
					scaled.DataType = "long"
					if leftPtr {
						is.emit(OpMul, scaled, right, &Operand{Type: "imm", Value: fmt.Sprintf("%d", elemSize)})
						right = scaled
					} else {
						is.emit(OpMul, scaled, left, &Operand{Type: "imm", Value: fmt.Sprintf("%d", elemSize)})
						left = scaled
					}
				}
			}
		}

		// Comparisons and logical ops yield int (C semantics), even for
		// float operands (whose comparison result was mistyped as float,
		// sending 0/1 through float varargs promotion).
		switch node.Operator {
		case "==", "!=", "<", "<=", ">", ">=", "&&", "||":
			result.DataType = "int"
		default:
			propagateArithType(is, node, result, left, right)
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
				if strings.Contains(varType, "*") {
					// Pointer ++/-- advances one ELEMENT, not one byte.
					// (stbtt__edge* e; e++ moved 1 byte and walked the edge
					// array byte-wise, tripping the rasterizer's bounds assert.)
					if elemSize := is.elemSizeForPtr(varType); elemSize > 1 {
						one = &Operand{Type: "imm", Value: fmt.Sprintf("%d", elemSize)}
					}
				}
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
				// Pointer ++/-- on a pointer-valued lvalue advances by
				// the element size, not one byte.
				if strings.Contains(addrOp.DataType, "*") {
					vt := strings.TrimSpace(addrOp.DataType)
					if strings.HasSuffix(vt, "*") {
						vt = vt[:len(vt)-1]
					}
					if strings.Contains(vt, "*") {
						if es := is.elemSizeForPtr(vt); es > 1 {
							one = &Operand{Type: "imm", Value: fmt.Sprintf("%d", es)}
						}
					}
				}
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
					// Stamp the pointer type so `(&p[0])->member` resolves
					// (operand DataType feeds member base-type lookup).
					if t := is.structTypeOf(child); t != "" {
						addrOp.DataType = t + "*"
					}
					return addrOp, nil
				}
			}
			return nil, fmt.Errorf("& operator on complex expression not yet supported (type=%d, in function: %s)", child.Type, is.currentFunc)
		}

		operand, err := is.selectExpression(node.Children[0])
		if err != nil {
			return nil, err
		}

		result := is.newTemp()
		result.DataType = operand.DataType

		switch node.Operator {
		case "-":
			// Float negation flips the sign bit (xor); integer negq on
			// float bits is meaningless (-2.5f became -1.75f).
			if operand.DataType == "float" || operand.DataType == "double" {
				is.emit(OpMov, result, operand, nil)
				if operand.DataType == "float" {
					// 0x80000000 as signed imm32 (-2147483648).
					is.emit(OpXor, result, result, &Operand{Type: "imm", Value: "-2147483648"})
				} else {
					is.emit(OpXor, result, result, &Operand{Type: "imm", Value: "0x8000000000000000"})
				}
			} else {
				is.emit(OpNeg, result, operand, nil)
			}
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
		// Multi-dim nests (a[i][j]) flatten here; legacy paths below.
		if rootAddr, totalOff, elemType, elemSize, isLast, ok := is.flattenMultiDim(node); ok {
			finalAddr := is.newTemp()
			is.emit(OpAdd, finalAddr, rootAddr, totalOff)
			if !isLast {
				// Row address (e.g. passing sorted_names[i] as char*).
				finalAddr.DataType = elemType + "*"
				return finalAddr, nil
			}
			result := is.newTemp()
			result.DataType = elemType
			is.emit(OpLoad, result, &Operand{Type: "ptr", IndexTemp: finalAddr, Size: elemSize, DataType: elemType}, nil)
			return result, nil
		}

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
			// Member arrays decay to their address (an 8-byte value load
			// would read element bytes as a pointer).
			var baseAddr *Operand
			if et, _, ok := is.memberArrayInfo(baseNode); ok {
				addr, _, aok, aerr := is.lvalueAddrSize(baseNode)
				if aerr != nil {
					return nil, aerr
				}
				if !aok {
					return nil, fmt.Errorf("cannot take address of array base")
				}
				addr.DataType = et + "*"
				baseAddr = addr
			} else {
				var berr error
				baseAddr, berr = is.selectExpression(baseNode)
				if berr != nil {
					return nil, berr
				}
			}

			// Element size comes from the pointer type (e.g. enum* -> 4).
			// Hardcoding 8 here overflowed realloc'd buffers (heap corruption).
			if os.Getenv("SEADBG") != "" {
				fmt.Fprintf(os.Stderr, "ARRLOAD baseType=%q elemSize=%d\n", baseAddr.DataType, 0)
			}
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

		// Array members decay to pointers (C semantics): return the
		// member address, not an 8-byte value load.
		if et, _, ok := is.memberArrayInfo(node); ok {
			addr, _, aok, aerr := is.lvalueAddrSize(node)
			if aerr != nil {
				return nil, aerr
			}
			if aok {
				addr.DataType = et + "*"
				return addr, nil
			}
		}

		baseNode := node.Children[0]
		memberName := node.MemberName
		isPtr := node.IsPointer // true for -> operator

		if os.Getenv("SEADEBUG") != "" {
			fmt.Fprintf(os.Stderr, "MEMBERACCESS baseNodeType=%d member=%s func=%s\n", baseNode.Type, memberName, is.currentFunc)
		}

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
		} else if !isPtr {
			// Dot on a complex base (array element, nested member):
			// struct values aren't loadable, so read via the member
			// address (same as the assignment paths).
			if addr, msize, ok, _ := is.lvalueAddrSize(node); ok {
				result := is.newTemp()
				result.DataType = is.memberAccessType(node)
				memberOp := &Operand{Type: "ptr", IndexTemp: addr, Size: msize, DataType: result.DataType}
				is.emit(OpLoad, result, memberOp, nil)
				return result, nil
			}
			// Not addressable (e.g. temp holding a struct pointer from
			// a statement expression): fall through to value logic below.
			baseTempVal, err := is.selectExpression(baseNode)
			if err != nil {
				return nil, err
			}
			baseTemp = baseTempVal
			structType = baseTemp.DataType
			if structType == "" {
				structType = baseNode.DataType
			}
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
				return nil, fmt.Errorf("member access '%s' on expression with unknown type (in function: %s)", memberName, is.currentFunc)
			}
		}

		// Resolve typedef aliases to actual struct types
		structType = is.resolveType(structType)

		// Extract struct name from type (e.g., "struct Point*" -> "Point")
		structName := structType
		origStructType := structType // Save for error reporting

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
				return nil, fmt.Errorf("member access '%s' on expression with no type information (base type: %v, base node type: %d, in function: %s)", memberName, baseTemp, baseNode.Type, is.currentFunc)
			}
			baseDesc := ""
			if baseNode != nil {
				baseDesc = fmt.Sprintf("; base node type=%d var=%q dt=%q", baseNode.Type, baseNode.VarName, baseNode.DataType)
			}
			return nil, fmt.Errorf("undefined struct: '%s' (from type: '%s'%s; member %q; in function: %s)", structName, origStructType, baseDesc, memberName, is.currentFunc)
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
			return nil, fmt.Errorf("struct %s has no member %s (in function: %s)", structName, memberName, is.currentFunc)
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
			} else if baseTemp.Type == "mem" {
				// Stack slot holding a struct value (e.g. a small struct
				// returned by an external function was spilled there):
				// address the slot, then load the member through it.
				addrTemp := is.newTemp()
				is.emit(OpLoadAddr, addrTemp, &Operand{Type: "mem", Offset: baseTemp.Offset}, nil)
				var ptrTemp *Operand
				if memberOffset != 0 {
					offsetOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", memberOffset)}
					ptrWithOffset := is.newTemp()
					is.emit(OpAdd, ptrWithOffset, addrTemp, offsetOp)
					ptrTemp = ptrWithOffset
				} else {
					ptrTemp = addrTemp
				}
				memberOp := &Operand{Type: "ptr", IndexTemp: ptrTemp, Size: memberSize, DataType: memberType}
				is.emit(OpLoad, result, memberOp, nil)
			} else {
				return nil, fmt.Errorf("dot access on non-variable expression not yet supported (in function: %s, member: %s, baseType: %s)",
					is.currentFunc, memberName, baseTemp.Type)
			}
		}

		return result, nil

	case NodeAssignment:
		// Large-struct hint for indirect calls on the RHS (e.g.
		// f = r.load_font.fn(...)). Set from the LHS type; deferred
		// restore covers all exits (selectExpression is recursive).
		{
			savedHint := is.largeRetHint
			defer func() { is.largeRetHint = savedHint }()
			lhsType := ""
			lhs := node.Children[0]
			if lhs.Type == NodeIdentifier {
				if sym, ok := is.localVars[lhs.VarName]; ok {
					lhsType = sym.Type
				} else if sym, ok := is.globalVars[lhs.VarName]; ok {
					lhsType = sym.Type
				}
			} else if lhs.Type == NodeMemberAccess {
				lhsType = is.memberAccessType(lhs)
			}
			if sz := is.structDefSize(lhsType); sz > 16 && is.unknownStructCall(node.Children[1]) {
				is.largeRetHint = lhsType
			}
		}
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
			temp.DataType = oldValue.DataType
			temp.Size = oldValue.Size
			if temp.Size == 0 {
				temp.Size = is.getTypeSize(oldValue.DataType)
			}

			// Pointer arithmetic in compound assignment: scale an
			// integer RHS by the element size (mirrors binary +).
			if (node.Operator == "+=" || node.Operator == "-=") &&
				strings.Contains(oldValue.DataType, "*") &&
				!strings.Contains(rightValue.DataType, "*") {
				if elemSize := is.elemSizeForPtr(oldValue.DataType); elemSize > 1 {
					scaled := is.newTemp()
					scaled.DataType = "long"
					is.emit(OpMul, scaled, rightValue, &Operand{Type: "imm", Value: fmt.Sprintf("%d", elemSize)})
					rightValue = scaled
				}
			}
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
				Type:  NodeNumber,
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

				// Check if base is a simple identifier (local/global array)
				if baseNode.Type == NodeIdentifier {
					varName := baseNode.VarName
					var baseOffset int
					var isGlobal bool
					var stSym *Symbol
					var elemType string
					elemSize := 8

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

					if stSym != nil {
						et, es := is.arrayElemSize(stSym)
						elemType = et
						if es > 0 {
							elemSize = es
						}
					}
					if elemType == "" {
						elemType = oldValue.DataType
					}
					if elemSize == 0 {
						elemSize = 8
					}

					byteOffset := is.newTemp()
					is.emit(OpMul, byteOffset, index, &Operand{Type: "imm", Value: fmt.Sprintf("%d", elemSize)})

					if stSym != nil && stSym.ArraySize == 0 {
						// Pointer variable: store through its value.
						baseAddr := is.newTemp()
						if isGlobal {
							is.emit(OpLoad, baseAddr, &Operand{Type: "var", Value: varName, IsGlobal: true}, nil)
						} else {
							is.emit(OpLoad, baseAddr, &Operand{Type: "var", Value: varName, Offset: baseOffset}, nil)
						}
						finalAddr := is.newTemp()
						is.emit(OpAdd, finalAddr, baseAddr, byteOffset)
						ptrOp := &Operand{
							Type:      "ptr",
							IndexTemp: finalAddr,
							Size:      elemSize,
							DataType:  elemType,
						}
						is.emit(OpStore, ptrOp, assignValue, nil)
					} else {
						arrayOp := &Operand{
							Type:      "array",
							Value:     varName,
							Offset:    baseOffset,
							IsGlobal:  isGlobal,
							IndexTemp: byteOffset,
							Size:      elemSize,
							DataType:  elemType,
						}
						is.emit(OpStore, arrayOp, assignValue, nil)
					}
				} else {
					baseAddr, err := is.selectExpression(baseNode)
					if err != nil {
						return nil, err
					}

					elemSize := is.elemSizeForPtr(baseAddr.DataType)
					if elemSize == 0 {
						elemSize = 8
					}
					elemType := strings.TrimSpace(strings.TrimSuffix(is.resolveType(baseAddr.DataType), "*"))
					if elemType == "" {
						elemType = oldValue.DataType
					}

					byteOffset := is.newTemp()
					is.emit(OpMul, byteOffset, index, &Operand{Type: "imm", Value: fmt.Sprintf("%d", elemSize)})

					finalAddr := is.newTemp()
					is.emit(OpAdd, finalAddr, baseAddr, byteOffset)

					ptrOp := &Operand{
						Type:      "ptr",
						IndexTemp: finalAddr,
						Size:      elemSize,
						DataType:  elemType,
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
					return nil, fmt.Errorf("undefined struct: %s (in function: %s)", structName, is.currentFunc)
				}

				memberOffset := -1
				memberSize := 8 // Default
				memberType := ""
				for _, member := range structDef.Members {
					if member.Name == memberName {
						memberOffset = member.Offset
						memberSize = member.Size
						memberType = member.Type
						break
					}
				}

				if memberOffset == -1 {
					return nil, fmt.Errorf("struct %s has no member %s (in function: %s)", structName, memberName, is.currentFunc)
				}

				// Struct-typed member from a struct value (nested call
				// result, struct var): full copy, not an 8-byte store.
				// (A single store truncates (drops tail) or misaligns.)
				if sz := is.structDefSize(memberType); sz > 0 {
					if srcAddr := is.structSrcAddr(memberNode.Children[0], assignValue); srcAddr != nil {
						// NOTE: Children[0] is the BASE, not the member!
						// The value's address comes from the RHS instead.
						_ = srcAddr
					}
					if rhsAddr := is.structSrcAddr(node.Children[1], assignValue); rhsAddr != nil {
						var dstAddr *Operand
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
							dstAddr = ptrTemp
						} else if baseTemp.Type == "var" {
							dstAddr = is.newTemp()
							is.emit(OpLoadAddr, dstAddr, &Operand{Type: "var", Value: baseTemp.Value, Offset: baseTemp.Offset + memberOffset, IsGlobal: baseTemp.IsGlobal}, nil)
						} else {
							if addr, _, ok, aerr := is.lvalueAddrSize(memberNode); aerr != nil {
								return nil, aerr
							} else if !ok {
								return nil, fmt.Errorf("dot access on complex expression for assignment not yet supported (in function: %s)", is.currentFunc)
							} else {
								dstAddr = addr
							}
						}
						is.emitStructCopy(dstAddr, rhsAddr, sz)
						return assignValue, nil
					}
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
					}
					// Complex base (e.g. arr[i].member): store via the
					// member address (lvalueAddrSize recurses into the base).
					addr, _, ok, aerr := is.lvalueAddrSize(memberNode)
					if aerr != nil {
						return nil, aerr
					}
					if !ok {
						return nil, fmt.Errorf("dot access on complex expression for assignment not yet supported (in function: %s)", is.currentFunc)
					}
					memberOp := &Operand{Type: "ptr", IndexTemp: addr, Size: memberSize}
					is.emit(OpStore, memberOp, assignValue, nil)
					return assignValue, nil
				}

				return assignValue, nil
			}

			// Handle dereference compound assignment
			if node.Children[0].Type == NodeUnaryOp && node.Children[0].Operator == "*" {
				ptrExpr, err := is.selectExpression(node.Children[0].Children[0])
				if err != nil {
					return nil, err
				}

				ptrOp := &Operand{Type: "ptr", IndexTemp: ptrExpr, Size: temp.Size, DataType: temp.DataType}
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

			// Value first, converted to element type immediately: nothing
			// else is live yet, so conversion scratch is always safe.
			// (Computing the address first and converting later lets the
			// conversion clobber a live address temp -> segfault.)
			value, err := is.selectExpression(node.Children[1])
			if err != nil {
				return nil, err
			}
			value = is.convertForHome(value, is.arrayElemHomeType(baseNode))

			// Struct-typed element: copy the whole struct, not an 8-byte
			// scalar. The scalar store below truncates: an element-typed
			// RHS yields the element ADDRESS (stored as 8 bytes, so the
			// target read back a pointer) and a variable RHS yields only
			// its first chunk. stbtt__sort_edges lost x1/y1/invert here.
			if sz := is.structDefSize(is.arrayElemHomeType(baseNode)); sz > 0 {
				structSrc := value
				if structSrc == nil || !strings.HasSuffix(strings.TrimSpace(structSrc.DataType), "*") {
					structSrc = is.structSrcAddr(node.Children[1], value)
				}
				if structSrc != nil {
					idx, ierr := is.selectExpression(arrayNode.Children[1])
					if ierr != nil {
						return nil, ierr
					}
					var dstAddr *Operand
					if baseNode.Type == NodeIdentifier {
						varName := baseNode.VarName
						var baseOffset int
						var isGlobal bool
						var stSym *Symbol
						if sym, ok := is.localVars[varName]; ok {
							baseOffset = sym.Offset
							stSym = sym
						} else if sym, ok := is.globalVars[varName]; ok {
							isGlobal = true
							stSym = sym
						} else {
							return nil, fmt.Errorf("undefined array: %s", varName)
						}
						es := sz
						if _, s := is.arrayElemSize(stSym); s > 0 {
							es = s
						}
						byteOffset := is.newTemp()
						is.emit(OpMul, byteOffset, idx, &Operand{Type: "imm", Value: fmt.Sprintf("%d", es)})
						if stSym.ArraySize == 0 {
							// Pointer variable: base is its value.
							bv, berr := is.selectExpression(baseNode)
							if berr != nil {
								return nil, berr
							}
							finalAddr := is.newTemp()
							is.emit(OpAdd, finalAddr, bv, byteOffset)
							dstAddr = finalAddr
						} else {
							baseAddr := is.newTemp()
							is.emit(OpLoadAddr, baseAddr, &Operand{Type: "var", Value: varName, Offset: baseOffset, IsGlobal: isGlobal}, nil)
							finalAddr := is.newTemp()
							is.emit(OpAdd, finalAddr, baseAddr, byteOffset)
							dstAddr = finalAddr
						}
					} else {
						// Complex base (member array, pointer expr):
						// same address derivation as the scalar branch.
						var baseAddr *Operand
						if et, _, ok := is.memberArrayInfo(baseNode); ok {
							addr, _, aok, aerr := is.lvalueAddrSize(baseNode)
							if aerr != nil {
								return nil, aerr
							}
							if !aok {
								return nil, fmt.Errorf("cannot take address of array base")
							}
							addr.DataType = et + "*"
							baseAddr = addr
						} else {
							var berr error
							baseAddr, berr = is.selectExpression(baseNode)
							if berr != nil {
								return nil, berr
							}
						}
						es := is.elemSizeForPtr(baseAddr.DataType)
						if es == 0 {
							es = sz
						}
						byteOffset := is.newTemp()
						is.emit(OpMul, byteOffset, idx, &Operand{Type: "imm", Value: fmt.Sprintf("%d", es)})
						finalAddr := is.newTemp()
						is.emit(OpAdd, finalAddr, baseAddr, byteOffset)
						dstAddr = finalAddr
					}
					is.emitStructCopy(dstAddr, structSrc, sz)
					return value, nil
				}
			}

			// Get index
			index, err := is.selectExpression(arrayNode.Children[1])
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
				// Member arrays decay to their address (an 8-byte value load
				// would read element bytes as a pointer).
				var baseAddr *Operand
				if et, _, ok := is.memberArrayInfo(baseNode); ok {
					addr, _, aok, aerr := is.lvalueAddrSize(baseNode)
					if aerr != nil {
						return nil, aerr
					}
					if !aok {
						return nil, fmt.Errorf("cannot take address of array base")
					}
					addr.DataType = et + "*"
					baseAddr = addr
				} else {
					var berr error
					baseAddr, berr = is.selectExpression(baseNode)
					if berr != nil {
						return nil, berr
					}
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
				stElemType := strings.TrimSuffix(strings.TrimSpace(is.resolveType(baseAddr.DataType)), "*")
				ptrOp := &Operand{
					Type:      "ptr",
					IndexTemp: finalAddr,
					Size:      stElemSize,
					DataType:  stElemType,
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
				return nil, fmt.Errorf("undefined struct: %s (in function: %s)", structName, is.currentFunc)
			}

			// Find member offset, size and type
			memberOffset := -1
			memberSize := 8 // Default
			memberTypeAssign := ""
			for _, member := range structDef.Members {
				if member.Name == memberName {
					memberOffset = member.Offset
					memberSize = member.Size
					memberTypeAssign = member.Type
					break
				}
			}

			if memberOffset == -1 {
				return nil, fmt.Errorf("struct %s has no member %s (in function: %s)", structName, memberName, is.currentFunc)
			}

			// Get value to store (convert to home type immediately: the
			// address computation below must not span value codegen, and
			// the conversion's scratch must not span the address).
			value, err := is.selectExpression(node.Children[1])
			if err != nil {
				return nil, err
			}
			value = is.convertForHome(value, memberTypeAssign)

			// Struct-typed member from a struct value: full copy.
			if sz := is.structDefSize(memberTypeAssign); sz > 0 {
				if rhsAddr := is.structSrcAddr(node.Children[1], value); rhsAddr != nil {
					var dstAddr *Operand
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
						dstAddr = ptrTemp
					} else if baseTemp.Type == "var" {
						dstAddr = is.newTemp()
						is.emit(OpLoadAddr, dstAddr, &Operand{Type: "var", Value: baseTemp.Value, Offset: baseTemp.Offset + memberOffset, IsGlobal: baseTemp.IsGlobal}, nil)
					} else {
						if addr, _, ok, aerr := is.lvalueAddrSize(memberNode); aerr != nil {
							return nil, aerr
						} else if !ok {
							return nil, fmt.Errorf("dot access on complex expression for assignment not yet supported (in function: %s)", is.currentFunc)
						} else {
							dstAddr = addr
						}
					}
					is.emitStructCopy(dstAddr, rhsAddr, sz)
					return value, nil
				}
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
				}
				// Complex base (e.g. arr[i].member): store via the
				// member address (lvalueAddrSize recurses into the base).
				addr, _, ok, aerr := is.lvalueAddrSize(memberNode)
				if aerr != nil {
					return nil, aerr
				}
				if !ok {
					return nil, fmt.Errorf("dot access on complex expression for assignment not yet supported (in function: %s)", is.currentFunc)
				}
				memberOp := &Operand{Type: "ptr", IndexTemp: addr, Size: memberSize}
				is.emit(OpStore, memberOp, value, nil)
				return value, nil
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
			// Convert to pointee type BEFORE the pointer is materialized
			// (conversion scratch must not span a live address temp).
			value = is.convertForHome(value, is.derefHomeType(node.Children[0].Children[0]))

			// Get the pointer expression (used immediately below)
			ptrExpr, err := is.selectExpression(node.Children[0].Children[0])
			if err != nil {
				return nil, err
			}

			// Check if we're assigning a struct: a direct compound
			// literal (not inside a statement expression), or any
			// addressable struct source through a struct-typed pointee.
			// Otherwise the scalar store at the end truncates to 8 bytes
			// (*dst = t kept only the first chunk and the tail went stale).
			isStructCopy := false
			var structSize int
			var structSrc *Operand

			// Check if RHS is a compound literal
			if node.Children[1].Type == NodeCompoundLiteral {
				isStructCopy = true
				structType := node.Children[1].DataType
				structSize = is.getTypeSize(structType)
				structSrc = value
			} else if sz := is.structDefSize(is.derefHomeType(node.Children[0].Children[0])); sz > 0 {
				structSize = sz
				if value != nil && strings.HasSuffix(strings.TrimSpace(value.DataType), "*") {
					// Value already is the struct's address (e.g. a
					// large-struct call result).
					structSrc = value
					isStructCopy = true
				} else if src := is.structSrcAddr(node.Children[1], value); src != nil {
					structSrc = src
					isStructCopy = true
				}
			}

			if isStructCopy && structSize > 0 && structSrc != nil {
				// Generate a memory copy: copy structSize bytes from structSrc (src addr) to ptrExpr (dst addr)
				if structSize <= 32 {
					// Small struct: copy field by field (8 bytes at a time)
					for offset := 0; offset < structSize; offset += 8 {
						remaining := structSize - offset
						copySize := 8
						if remaining < 8 {
							copySize = remaining
						}

						// Load from source: *(structSrc + offset)
						srcTemp := is.newTemp()
						if offset > 0 {
							offsetOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", offset)}
							srcAddr := is.newTemp()
							is.emit(OpAdd, srcAddr, structSrc, offsetOp)
							srcOp := &Operand{Type: "ptr", IndexTemp: srcAddr, Size: copySize}
							is.emit(OpLoad, srcTemp, srcOp, nil)
						} else {
							srcOp := &Operand{Type: "ptr", IndexTemp: structSrc, Size: copySize}
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
					// memcpy(ptrExpr, structSrc, structSize)
					sizeOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", structSize)}
					is.emit(OpMov, &Operand{Type: "reg", Value: "rdi"}, ptrExpr, nil)
					is.emit(OpMov, &Operand{Type: "reg", Value: "rsi"}, structSrc, nil)
					is.emit(OpMov, &Operand{Type: "reg", Value: "rdx"}, sizeOp, nil)
					// Target goes in src1 (dst=nil): emitCall reads Src1.
					is.emit(OpCall, nil, &Operand{Type: "label", Value: "memcpy"}, nil)
				}

				return value, nil
			}

			// Store to pointer (width-correct: an 8-byte mov to an
			// int* clobbers the neighbor; size from pointee type).
			derefHome := strings.TrimSuffix(strings.TrimSpace(is.resolveType(ptrExpr.DataType)), "*")
			derefSize := 0
			if s, ok := cSizeOf(strings.TrimSpace(derefHome)); ok && s > 0 {
				derefSize = s
			} else if s := is.getTypeSize(derefHome); s > 0 {
				derefSize = s
			}
			ptrOp := &Operand{Type: "ptr", IndexTemp: ptrExpr, DataType: derefHome, Size: derefSize}
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
			// Struct-typed var from a struct value: full copy, not an
			// 8-byte store (e.g. r = pick_renderer()).
			if sz := is.structDefSize(sym.Type); sz > 0 {
				if srcAddr := is.structSrcAddr(node.Children[1], value); srcAddr != nil {
					dstAddr := is.newTemp()
					is.emit(OpLoadAddr, dstAddr, &Operand{Type: "var", Value: varName, Offset: sym.Offset}, nil)
					is.emitStructCopy(dstAddr, srcAddr, sz)
					return value, nil
				}
			}
			value = is.convertForHome(value, sym.Type)
			varOp := &Operand{Type: "var", Value: varName, Offset: sym.Offset}
			is.emit(OpStore, varOp, value, nil)
		} else if gsym, ok := is.globalVars[varName]; ok {
			if sz := is.structDefSize(gsym.Type); sz > 0 {
				if srcAddr := is.structSrcAddr(node.Children[1], value); srcAddr != nil {
					dstAddr := is.newTemp()
					is.emit(OpLoadAddr, dstAddr, &Operand{Type: "var", Value: varName, IsGlobal: true}, nil)
					is.emitStructCopy(dstAddr, srcAddr, sz)
					return value, nil
				}
			}
			value = is.convertForHome(value, gsym.Type)
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

		// External float/double returns (libc table; declared functions
		// resolve via the map above). Sea-defined functions use rax
		// convention (handled by normal path); externals use xmm0.
		extRet := returnType
		if extRet == "" {
			if libcFloatFuncs[node.Name] {
				extRet = "float"
			} else if libcDoubleRet[node.Name] {
				extRet = "double"
			}
		}
		extFloatRet := (extRet == "float" || extRet == "double") && !is.funcDefined[node.Name] && node.Target == nil
		// Indirect call in a large-struct context (assignment/init/return
		// expecting one): assume that return (hidden pointer). Always
		// clear on entry so the hint never leaks inward: it targets the
		// outermost unknown-return call only (inner mallocs etc. must
		// not inherit it; known-return calls don't need it).
		if returnType == "" && is.largeRetHint != "" {
			returnType = is.largeRetHint
		}
		is.largeRetHint = ""
		// Indirect calls (cb.fn(), (*fp)()) have no declared signature.
		// Evaluated late (just before emission below) so the pointer temp
		// never spans arg codegen. See below.

		// Evaluate arguments directly into stack slots (dumb but airtight):
		// each arg temp dies at its store, so later arg evals (emitter scratch
		// %rax/%r11) and the SetArg moves (fixed %rdi/%rsi/...) can never
		// clobber an already-evaluated arg. No analysis pass needed.
		type callArg struct {
			slot     *Operand // normal arg (nil for struct)
			isStruct bool
			chunks   []string // struct chunk classes, or ["mem"] if >16
			srcAddr  *Operand // struct source address temp
			ssize    int
		}
		var callArgs []callArg
		// Expected param types for C conversions (int->float etc.).
		// Converted HERE (not in SetArg): conversions use xmm0 scratch
		// which would clobber earlier args already sitting in xmm regs.
		var paramTypes []string
		if sig, ok := is.functions[node.Name]; ok {
			paramTypes = sig.ParamTypes
		}
		for ai, argNode := range node.Children {
			// Large-struct hint when the corresponding param expects
			// one (direct calls with signatures): an indirect call
			// used as this arg still passes the hidden pointer.
			savedHint := is.largeRetHint
			if ai < len(paramTypes) {
				if sz := is.structDefSize(paramTypes[ai]); sz > 16 && is.unknownStructCall(argNode) {
					is.largeRetHint = paramTypes[ai]
				}
			}
			arg, err := is.selectExpression(argNode)
			is.largeRetHint = savedHint
			if err != nil {
				return nil, err
			}
			// Structs over 8 bytes need special passing (chunked regs
			// for 9-16, hidden pointer for larger). Detect AFTER eval
			// so calls/compound literals materialize first.
			sname, ssize, sIsStruct := is.structArgInfo(argNode, arg)
			// Extern (gcc-compiled) DIRECT calls use the SysV ABI.
			// Indirect calls (through function pointers) target
			// sea-compiled trampolines and keep sea's internal ABI.
			// A call is extern only when it names a known function that
			// has no body in this TU; a call through a variable is
			// indirect even though node.Target is still nil here.
			isExtern := node.Target == nil && is.functions[node.Name] != nil && !is.funcDefined[node.Name]
			// Small structs (<= 8 bytes, e.g. Vector2/Color) use sea's
			// internal value convention for sea-to-sea calls, but an
			// extern call needs proper SysV classification (a float
			// struct like Vector2 must go in an xmm register, not an
			// integer one). So only take the struct path for extern
			// calls or structs larger than 8 bytes.
			if sIsStruct && (ssize > 8 || isExtern) {
				srcAddr := is.structSrcAddr(argNode, arg)
				if srcAddr == nil && ssize <= 8 {
					// Small struct value with no address (e.g. a
					// register-returned struct like b2MakeRot()):
					// spill it to a stack temp so the chunk loader can
					// read it back.
					is.stackOffset -= 8
					tmpOff := is.stackOffset
					sp := is.newTemp()
					sp.DataType = "long"
					is.emit(OpMov, sp, arg, nil)
					is.emit(OpStore, &Operand{Type: "mem", Offset: tmpOff}, sp, nil)
					srcAddr = is.newTemp()
					is.emit(OpLoadAddr, srcAddr, &Operand{Type: "mem", Offset: tmpOff}, nil)
				}
				if srcAddr == nil {
					return nil, fmt.Errorf("cannot pass struct argument (in function: %s)", is.currentFunc)
				}
				var chunks []string
				if isExtern && ssize > 16 {
					// SysV: pass big struct by value on the stack.
					chunks = []string{"sysv"}
				} else if ssize > 16 {
					// sea internal ABI: pass a pointer to the struct.
					chunks = []string{"mem"}
				} else {
					chunks = is.structChunkClasses(sname)
					if len(chunks) == 0 {
						chunks = []string{"int", "int"}
					}
				}
				callArgs = append(callArgs, callArg{isStruct: true, chunks: chunks, srcAddr: srcAddr, ssize: ssize})
				continue
			}
			// C conversion to the expected param type (int->float,
			// float->double...). Unknown/varargs keep arg as-is.
			if ai < len(paramTypes) && paramTypes[ai] != "" {
				arg = is.convertForHome(arg, paramTypes[ai])
			} else if len(paramTypes) == 0 && libcFloatFuncs[node.Name] {
				arg = is.convertForHome(arg, "float")
			}
			is.stackOffset -= 8
			slot := &Operand{Type: "mem", Offset: is.stackOffset, DataType: arg.DataType}
			// Keep float-literal detection working: slots are mem, so record
			// the original immediate spelling on the slot value (unused for mem).
			if arg.Type == "imm" {
				slot.Value = arg.Value
			}
			is.emit(OpStore, slot, arg, nil)
			callArgs = append(callArgs, callArg{slot: slot})
		}
		args := callArgs

		// Check if we need to allocate space for a large struct return
		var retSlot *Operand
		argStartIdx := 0


		if returnType != "" && is.isLargeStruct(returnType) {
			// Allocate space for return value on stack
			structSize := is.getTypeSize(returnType)
			is.stackOffset -= structSize
			// Align DOWN (more negative): Go's % keeps sign, so
			// `-120 - (-120%16)` would round UP and shrink the slot.
			for is.stackOffset%16 != 0 {
				is.stackOffset--
			}

			retSlotOffset := is.stackOffset
			retSlot = &Operand{
				Type:     "mem", // Use "mem" type to prevent register allocation
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
		// Expected param types (declared above in the slot loop) drive
		// float routing below.
		for ai, ca := range args {
			// Struct over 8 bytes: chunked regs (9-16) or pointer (>16).
			if ca.isStruct {
				if len(ca.chunks) == 1 && ca.chunks[0] == "sysv" {
					// SysV: pass big struct by value on the stack
					// (consecutive 8-byte slots). Spill the source
					// address to a stack slot first (like emitStructCopy)
					// so the chunk loop can't clobber it.
					nslots := (ca.ssize + 7) / 8
					is.stackOffset -= 8
					srcSlot := is.stackOffset
					is.emit(OpStore, &Operand{Type: "mem", Offset: srcSlot}, ca.srcAddr, nil)
					for k := 0; k < nslots; k++ {
						is.stackOffset -= 8
						st := &Operand{Type: "mem", Offset: is.stackOffset, DataType: "long"}
						srcBase := is.newTemp()
						is.emit(OpLoad, srcBase, &Operand{Type: "mem", Offset: srcSlot}, nil)
						if k > 0 {
							offOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", k*8)}
							tmp := is.newTemp()
							is.emit(OpAdd, tmp, srcBase, offOp)
							srcBase = tmp
						}
						val := is.newTemp()
						val.DataType = "long"
						is.emit(OpLoad, val, &Operand{Type: "ptr", IndexTemp: srcBase, Size: 8}, nil)
						is.emit(OpStore, st, val, nil)
						stackArgs = append(stackArgs, stackArg{slot: st, idx: len(stackArgs)})
					}
					continue
				}
				if len(ca.chunks) == 1 && ca.chunks[0] == "mem" {
					if intRegIdx < len(intRegs) {
						regOp := &Operand{Type: "reg", Value: intRegs[intRegIdx]}
						intRegIdx++
						is.emit(OpSetArg, regOp, ca.srcAddr, nil)
					} else {
						is.stackOffset -= 8
						st := &Operand{Type: "mem", Offset: is.stackOffset, DataType: "long*"}
						is.emit(OpStore, st, ca.srcAddr, nil)
						stackArgs = append(stackArgs, stackArg{slot: st, idx: len(stackArgs)})
					}
					continue
				}
				for i, cl := range ca.chunks {
					cAddr := ca.srcAddr
					if i > 0 {
						offOp := &Operand{Type: "imm", Value: fmt.Sprintf("%d", i*8)}
						cA := is.newTemp()
						is.emit(OpAdd, cA, ca.srcAddr, offOp)
						cAddr = cA
					}
					cVal := is.newTemp()
					cVal.DataType = "long"
					if cl == "sse" {
						cVal.DataType = "double"
					}
					is.emit(OpLoad, cVal, &Operand{Type: "ptr", IndexTemp: cAddr, Size: 8}, nil)
					if cl == "sse" {
						if floatRegIdx < len(floatRegs) {
							regOp := &Operand{Type: "freg", Value: floatRegs[floatRegIdx]}
							floatRegIdx++
							is.emit(OpSetArg, regOp, cVal, nil)
						} else {
							// Stack overflow: share the stack area in order.
							is.stackOffset -= 8
							st := &Operand{Type: "mem", Offset: is.stackOffset, DataType: cVal.DataType}
							is.emit(OpStore, st, cVal, nil)
							stackArgs = append(stackArgs, stackArg{slot: st, idx: len(stackArgs)})
						}
					} else {
						if intRegIdx < len(intRegs) {
							regOp := &Operand{Type: "reg", Value: intRegs[intRegIdx]}
							intRegIdx++
							is.emit(OpSetArg, regOp, cVal, nil)
						} else {
							is.stackOffset -= 8
							st := &Operand{Type: "mem", Offset: is.stackOffset, DataType: cVal.DataType}
							is.emit(OpStore, st, cVal, nil)
							stackArgs = append(stackArgs, stackArg{slot: st, idx: len(stackArgs)})
						}
					}
				}
				continue
			}
			arg := ca.slot
			// (Converted to the expected param type in the slot loop.
			// Converting here would use xmm0 scratch and clobber
			// earlier args already sitting in xmm regs.)
			// Determine if this argument is a float (slots carry the
			// original immediate spelling in Value for this check)
			isFloat := arg.DataType == "float" || arg.DataType == "double" ||
				((arg.Type == "mem" || arg.Type == "imm") && isFloatSpelling(arg.Value))

			// SysV float routing: fixed float params take bits; double
			// (or unknown/varargs) params take promoted doubles.
			expectFloat := false
			if ai < len(paramTypes) && paramTypes[ai] == "float" {
				expectFloat = true
			} else if len(paramTypes) == 0 && libcFloatFuncs[node.Name] && arg.DataType == "float" {
				expectFloat = true
			}
			if isFloat && expectFloat && arg.DataType != "double" {
				// Pass float bits (no promotion). Temps/regs holding
				// bits pass through retyped (SetArg movq preserves
				// low 32); immediates and memory materialize 32-bit.
				var bitSrc *Operand
				if arg.Type == "temp" || arg.Type == "reg" {
					bitSrc = &Operand{Type: arg.Type, Value: arg.Value, Offset: arg.Offset, DataType: "int"}
				} else {
					bitTemp := is.newTemp()
					bitTemp.DataType = "int"
					if arg.Type == "imm" {
						is.emit(OpMov, bitTemp, arg, nil)
					} else {
						ldSrc := &Operand{Type: arg.Type, Value: arg.Value, Offset: arg.Offset, IsGlobal: arg.IsGlobal, Size: 4, DataType: "float"}
						is.emit(OpLoad, bitTemp, ldSrc, nil)
					}
					bitSrc = bitTemp
				}
				if floatRegIdx < len(floatRegs) {
					regOp := &Operand{Type: "freg", Value: floatRegs[floatRegIdx]}
					floatRegIdx++
					is.emit(OpSetArg, regOp, bitSrc, nil)
				} else {
					// Stack float: 8-byte slot, bits in low 32 (callee
					// movss-loads; upper garbage ignored both sides).
					is.stackOffset -= 8
					st := &Operand{Type: "mem", Offset: is.stackOffset, DataType: "int"}
					is.emit(OpStore, st, bitSrc, nil)
					stackArgs = append(stackArgs, stackArg{slot: st, idx: len(stackArgs)})
				}
				continue
			}

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
		// Plain `fp()` parses without a Target; when the name is a
		// variable (fn-pointer param/local, callback field) rather than
		// a known function, call indirectly through it.
		callTarget := node.Target
		if callTarget == nil && node.Name != "" {
			if _, isFunc := is.functions[node.Name]; !isFunc {
				if _, isVar := is.localVars[node.Name]; isVar {
					callTarget = &ASTNode{Type: NodeIdentifier, VarName: node.Name}
				} else if _, isVar := is.globalVars[node.Name]; isVar {
					callTarget = &ASTNode{Type: NodeIdentifier, VarName: node.Name}
				}
			}
		}
		if callTarget != nil {
			top, terr := is.selectExpression(callTarget)
			if terr != nil {
				return nil, terr
			}
			funcOp = top
		} else {
			funcOp = &Operand{Type: "label", Value: node.Name}
		}
		// If we used a return slot, the struct is already there (the
		// callee filled it via the hidden pointer; rax just echoes the
		// pointer). Emit the call with no Dst so emitCall doesn't move
		// rax over the struct.
		if retSlot != nil {
			is.emit(OpCall, nil, funcOp, &Operand{Type: "imm", Value: fmt.Sprintf("%d", len(args))})
		} else if extFloatRet {
			// External float/double return lives in xmm0 (SysV), not
			// rax. Move bits out (float stays float bits for the
			// float model; double moves whole).
			is.emit(OpCall, nil, funcOp, &Operand{Type: "imm", Value: fmt.Sprintf("%d", len(args))})
			xmm := &Operand{Type: "freg", Value: "xmm0"}
			result.DataType = extRet
			is.emit(OpMov, result, xmm, nil)
		} else {
			is.emit(OpCall, result, funcOp, &Operand{Type: "imm", Value: fmt.Sprintf("%d", len(args))})
		}
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
			// External (gcc ABI) small-struct returns: float-only 8-byte
			// chunks come back in xmm0/xmm1, integer chunks in rax/rdx.
			// The rax/rdx-everywhere convention only holds for functions
			// with a body in this TU (our callees write it that way).
			if structSize > 0 && structSize <= 16 && callTarget == nil && node.Name != "" && !is.funcDefined[node.Name] {
				if extRes := is.externalSmallStructReturn(returnType, structSize); extRes != nil {
					return extRes, nil
				}
			}
			if structSize > 8 && structSize <= 16 {
				// Struct is returned in RAX (first 8 bytes) + RDX (next 8 bytes)
				// We need to save both registers to memory
				is.stackOffset -= 16 // Allocate space for full struct
				// Align DOWN (more negative); Go's % keeps sign.
				for is.stackOffset%16 != 0 {
					is.stackOffset--
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
				// Always carry the return type so member access and
				// sizeof can resolve structs/pointers from call results.
				result.DataType = returnType
			}
		}

		return result, nil

	case NodeComma:
		// Comma operator: evaluate left-to-right, yield the last.
		// (C sequence point; each side effect completes in order.)
		var last *Operand
		for _, child := range node.Children {
			v, err := is.selectExpression(child)
			if err != nil {
				return nil, err
			}
			last = v
		}
		if last == nil {
			return &Operand{Type: "imm", Value: "0"}, nil
		}
		return last, nil

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
			return nil, fmt.Errorf("undefined struct: %s (in function: %s)", structName, is.currentFunc)
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

			// Find field offset, size and type
			var fieldOffset int
			var fieldSize int
			var fieldType string
			if fieldName == "" {
				// Positional - use index
				if i < len(structDef.Members) {
					fieldOffset = structDef.Members[i].Offset
					fieldSize = structDef.Members[i].Size
					fieldType = structDef.Members[i].Type
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
						fieldType = member.Type
						found = true
						break
					}
				}
				if !found {
					return nil, fmt.Errorf("struct %s has no member %s (in function: %s)", structName, fieldName, is.currentFunc)
				}
			}

			// Convert the initializer to the field's type (e.g. an int
			// literal initializing a double field must become a double,
			// not raw integer bits).
			if value != nil && fieldType != "" {
				value = is.convertForHome(value, fieldType)
			}

			// Struct-typed field from a struct value (nested compound,
			// struct var): copy bytes, not the address/value temp.
			if sz := is.structDefSize(fieldType); sz > 0 {
				if srcAddr := is.structSrcAddr(node.Children[i], value); srcAddr != nil {
					dstAddr := is.newTemp()
					is.emit(OpLoadAddr, dstAddr, &Operand{Type: "var", Value: tempName, Offset: baseOffset + fieldOffset}, nil)
					is.emitStructCopy(dstAddr, srcAddr, sz)
					continue
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
		var lastExprNode *ASTNode

		for _, stmt := range node.Children {
			if stmt.Type == NodeExprStmt && len(stmt.Children) > 0 {
				// Expression statement - evaluate it
				val, err := is.selectExpression(stmt.Children[0])
				if err != nil {
					return nil, err
				}
				lastValue = val
				lastExprNode = stmt.Children[0]
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

		// Struct-valued statement expression: hand back the struct's
		// ADDRESS (sea's struct-value convention). Callers such as member
		// access and struct copies then dereference a real pointer instead
		// of treating the first 8 loaded bytes as one.
		if lastExprNode != nil && is.structDefSize(lastValue.DataType) > 0 {
			if addr := is.structSrcAddr(lastExprNode, lastValue); addr != nil {
				addr.DataType = lastValue.DataType
				return addr, nil
			}
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

	case NodeSizeof:
		// sizeof(expr): scope-correct sizes (arrays yield totals, not
		// element sizes). Never evaluates the operand (no side effects).
		if len(node.Children) < 1 {
			return nil, fmt.Errorf("sizeof needs operand")
		}
		sub := node.Children[0]
		sz := is.sizeofStatic(sub)
		return &Operand{Type: "imm", Value: fmt.Sprintf("%d", sz), DataType: "unsigned long"}, nil

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
		dstT := node.DataType
		// Route through convertForHome (cvtMode + 32->64 widening) so
		// (long)int_minus_one sign-extends instead of becoming +4B.
		// Pointers pass through bitwise (convertForHome skips them).
		return is.convertForHome(result, dstT), nil

	default:
		return nil, fmt.Errorf("unknown expression type: %d", node.Type)
	}
}
