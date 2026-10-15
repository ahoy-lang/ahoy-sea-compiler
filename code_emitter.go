package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// emitImmToReg64 moves an integer immediate into a 64-bit register, using
// movabs when it doesn't fit a sign-extended 32-bit immediate. Plain movq
// with a wide constant either fails to assemble or (via movl) silently
// truncates — both wrong for UL-sized constants.
func (ce *CodeEmitter) emitImmToReg64(value, dstReg string) {
	if v, err := strconv.ParseUint(value, 0, 64); err == nil && v > 0x7FFFFFFF {
		ce.output.WriteString(fmt.Sprintf("    movabs $%s, %s\n", value, dstReg))
	} else {
		ce.output.WriteString(fmt.Sprintf("    movq $%s, %s\n", value, dstReg))
	}
}

// Code emitter - generates x86-64 assembly from IR
type CodeEmitter struct {
	output       strings.Builder
	dataSection  strings.Builder
	bssSection   strings.Builder
	rodataSection strings.Builder
	
	instructions []*IRInstruction
	stringLits   map[string]string
	globalVars   map[string]*Symbol
	structDefs   map[string]*StructDef
	localFuncs   map[string]bool // static/inline-only: emit without .globl
	floatLits    map[string]string  // label -> double literal value
	float32Lits  map[string]string  // label -> single-precision literal value
	
	currentFunc   string
	stackSize     int
	usedRegisters []int
	
	labelCounter  int
	floatCounter  int
}

func NewCodeEmitter(instructions []*IRInstruction, stringLits map[string]string, globalVars map[string]*Symbol, structDefs map[string]*StructDef, localFuncs map[string]bool) *CodeEmitter {
	return &CodeEmitter{
		instructions:  instructions,
		stringLits:    stringLits,
		globalVars:    globalVars,
		structDefs:    structDefs,
		localFuncs:    localFuncs,
		floatLits:     make(map[string]string),
		float32Lits:   make(map[string]string),
	}
}

func (ce *CodeEmitter) Emit() string {
	ce.emitBssSection()
	ce.emitTextSection()
	// Emit data section last, after we've discovered all float literals
	ce.emitDataSection()
	
	return ce.buildOutput()
}

func (ce *CodeEmitter) emitDataSection() {
	if len(ce.stringLits) == 0 && len(ce.floatLits) == 0 && len(ce.float32Lits) == 0 {
		return
	}
	
	ce.rodataSection.WriteString("    .section .rodata\n")
	
	// Emit string literals
	for label, str := range ce.stringLits {
		ce.rodataSection.WriteString(fmt.Sprintf("%s:\n", label))
		ce.rodataSection.WriteString(fmt.Sprintf("    .string \"%s\"\n", escapeString(str)))
	}
	
	// Emit float literals
	for label, value := range ce.floatLits {
		ce.rodataSection.WriteString(fmt.Sprintf("    .align 8\n"))
		ce.rodataSection.WriteString(fmt.Sprintf("%s:\n", label))
		ce.rodataSection.WriteString(fmt.Sprintf("    .double %s\n", value))
	}

	// Emit single-precision literals
	for label, value := range ce.float32Lits {
		ce.rodataSection.WriteString(fmt.Sprintf("    .align 4\n"))
		ce.rodataSection.WriteString(fmt.Sprintf("%s:\n", label))
		ce.rodataSection.WriteString(fmt.Sprintf("    .float %s\n", value))
	}
}

func (ce *CodeEmitter) emitBssSection() {
	if len(ce.globalVars) == 0 {
		return
	}

	ce.bssSection.WriteString("    .bss\n")
	for name, sym := range ce.globalVars {
		// Skip external symbols (libc provides these)
		if sym.IsExternal {
			continue
		}
		// Initialized globals go to .data (BSS would zero them).
		if sym.Init != nil {
			if sym.Init.Type == NodeCompoundLiteral && sym.ArraySize > 0 {
				if ce.emitDataArray(name, sym, sym.Init) {
					continue
				}
			} else if sym.ArraySize == 0 {
				if sym.Init.Type == NodeCompoundLiteral {
					if ce.emitDataStruct(name, sym, sym.Init) {
						continue
					}
				} else if ce.emitDataGlobal(name, sym) {
					continue
				}
			}
		}
		ce.bssSection.WriteString(fmt.Sprintf("    .comm %s,%d,%d\n", name, sym.Size, sym.Size))
	}
}

// emitDataStruct emits a global struct initializer to .data, member by
// member with padding. Returns false (caller falls back to .comm) when any
// field isn't a foldable constant.
func (ce *CodeEmitter) emitDataStruct(name string, sym *Symbol, node *ASTNode) bool {
	structName := strings.TrimSpace(node.DataType)
	structName = strings.TrimPrefix(structName, "struct ")
	structName = strings.TrimPrefix(structName, "union ")
	structName = strings.TrimSpace(structName)
	def, ok := ce.structDefs[structName]
	if !ok {
		return false
	}
	// Map initializers: positional in order, designated by name.
	desVals := map[string]*ASTNode{}
	posIdx := 0
	for i, child := range node.Children {
		fname := ""
		if i < len(node.InitFields) {
			fname = node.InitFields[i]
		}
		if fname == "" {
			if posIdx >= len(def.Members) {
				return false
			}
			desVals[def.Members[posIdx].Name] = child
			posIdx++
		} else {
			desVals[fname] = child
		}
	}
	ce.emitDataHeader(name, sym, def.Size)
	off := 0
	for _, m := range def.Members {
		if m.Offset > off {
			ce.dataSection.WriteString(fmt.Sprintf("    .zero %d\n", m.Offset-off))
			off = m.Offset
		}
		val, present := desVals[m.Name]
		if !present {
			ce.dataSection.WriteString(fmt.Sprintf("    .zero %d\n", m.Size))
			off += m.Size
			continue
		}
		if !ce.emitDataMemberValue(m, val) {
			return false
		}
		off += m.Size
	}
	structSize := def.Size
	if sym.Size > structSize {
		structSize = sym.Size
	}
	if structSize > off {
		ce.dataSection.WriteString(fmt.Sprintf("    .zero %d\n", structSize-off))
	}
	return true
}

// emitDataMemberValue emits one member initializer; false = not foldable.
func (ce *CodeEmitter) emitDataMemberValue(m StructMember, val *ASTNode) bool {
	isFloatMember := m.Type == "float" || m.Type == "double"
	v, kind := ce.constInitValue(val, nil)
	if kind == "" {
		return false
	}
	if kind == "string" {
		if m.Size != 8 {
			return false
		}
		ce.dataSection.WriteString(fmt.Sprintf("    .quad %s\n", v))
		return true
	}
	if isFloatMember {
		if kind == "double" && m.Size == 4 {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				ce.dataSection.WriteString(fmt.Sprintf("    .long %d\n", math.Float32bits(float32(f))))
				return true
			}
			return false
		}
		if kind == "int" {
			v = v + ".0"
		}
		if m.Size == 4 {
			ce.dataSection.WriteString(fmt.Sprintf("    .float %s\n", v))
		} else {
			ce.dataSection.WriteString(fmt.Sprintf("    .double %s\n", v))
		}
		return true
	}
	if kind == "double" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			v = fmt.Sprintf("%d", int64(f))
		} else {
			return false
		}
	}
	switch m.Size {
	case 8:
		ce.dataSection.WriteString(fmt.Sprintf("    .quad %s\n", v))
	case 4:
		ce.dataSection.WriteString(fmt.Sprintf("    .long %s\n", v))
	case 2:
		ce.dataSection.WriteString(fmt.Sprintf("    .value %s\n", v))
	case 1:
		ce.dataSection.WriteString(fmt.Sprintf("    .byte %s\n", v))
	default:
		return false
	}
	return true
}

// emitDataHeader writes linkage + label for a .data global.
func (ce *CodeEmitter) emitDataHeader(name string, sym *Symbol, structSize int) {
	if sym.IsStatic {
		ce.dataSection.WriteString(fmt.Sprintf("    .local %s\n", name))
	} else {
		ce.dataSection.WriteString(fmt.Sprintf("    .globl %s\n", name))
	}
	align := sym.Size
	if structSize > 0 {
		align = structSize
	}
	if align > 8 {
		align = 8
	}
	if align < 1 {
		align = 1
	}
	ce.dataSection.WriteString(fmt.Sprintf("    .align %d\n", align))
	ce.dataSection.WriteString(fmt.Sprintf("%s:\n", name))
}

// emitDataGlobal emits one initialized global to .data. Returns false for
// non-constant initializers (caller falls back to .comm like before).
func (ce *CodeEmitter) emitDataGlobal(name string, sym *Symbol) bool {
	val, kind := ce.constInitValue(sym.Init, sym)
	if kind == "" {
		return false
	}
	if sym.IsStatic {
		ce.dataSection.WriteString(fmt.Sprintf("    .local %s\n", name))
	} else {
		ce.dataSection.WriteString(fmt.Sprintf("    .globl %s\n", name))
	}
	align := sym.Size
	if align > 8 {
		align = 8
	}
	if align < 1 {
		align = 1
	}
	ce.dataSection.WriteString(fmt.Sprintf("    .align %d\n", align))
	ce.dataSection.WriteString(fmt.Sprintf("%s:\n", name))
	switch kind {
	case "string":
		// char* init: address of (possibly newly interned) literal.
		ce.dataSection.WriteString(fmt.Sprintf("    .quad %s\n", val))
	case "double":
		if sym.Size == 4 {
			ce.dataSection.WriteString(fmt.Sprintf("    .float %s\n", val))
		} else {
			ce.dataSection.WriteString(fmt.Sprintf("    .double %s\n", val))
		}
	default: // int
		switch sym.Size {
		case 8:
			ce.dataSection.WriteString(fmt.Sprintf("    .quad %s\n", val))
		case 4:
			ce.dataSection.WriteString(fmt.Sprintf("    .long %s\n", val))
		case 2:
			ce.dataSection.WriteString(fmt.Sprintf("    .value %s\n", val))
		case 1:
			ce.dataSection.WriteString(fmt.Sprintf("    .byte %s\n", val))
		default:
			ce.dataSection.WriteString(fmt.Sprintf("    .quad %s\n", val))
		}
	}
	return true
}

// emitDataArray emits an initialized global array to .data, element by
// element with zero-fill for the remainder. Returns false (caller falls
// back to .comm) when any element isn't a constant.
func (ce *CodeEmitter) emitDataArray(name string, sym *Symbol, node *ASTNode) bool {
	elemSize := 8
	if sym.ArraySize > 0 {
		elemSize = sym.Size / sym.ArraySize
	}
	if elemSize <= 0 {
		elemSize = 8
	}
	type elemVal struct {
		val  string
		kind string
	}
	var vals []elemVal
	for _, e := range node.Children {
		v, k := ce.constInitValue(e, nil)
		if k == "" {
			return false
		}
		vals = append(vals, elemVal{v, k})
	}
	if sym.IsStatic {
		ce.dataSection.WriteString(fmt.Sprintf("    .local %s\n", name))
	} else {
		ce.dataSection.WriteString(fmt.Sprintf("    .globl %s\n", name))
	}
	align := elemSize
	if align > 8 {
		align = 8
	}
	if align < 1 {
		align = 1
	}
	ce.dataSection.WriteString(fmt.Sprintf("    .align %d\n", align))
	ce.dataSection.WriteString(fmt.Sprintf("%s:\n", name))
	emitOne := func(val, kind string) {
		switch kind {
		case "string":
			ce.dataSection.WriteString(fmt.Sprintf("    .quad %s\n", val))
		case "double":
			if elemSize == 4 {
				ce.dataSection.WriteString(fmt.Sprintf("    .float %s\n", val))
			} else {
				ce.dataSection.WriteString(fmt.Sprintf("    .double %s\n", val))
			}
		default:
			switch elemSize {
			case 4:
				ce.dataSection.WriteString(fmt.Sprintf("    .long %s\n", val))
			case 2:
				ce.dataSection.WriteString(fmt.Sprintf("    .value %s\n", val))
			case 1:
				ce.dataSection.WriteString(fmt.Sprintf("    .byte %s\n", val))
			default:
				ce.dataSection.WriteString(fmt.Sprintf("    .quad %s\n", val))
			}
		}
	}
	for _, e := range vals {
		emitOne(e.val, e.kind)
	}
	if rest := sym.ArraySize - len(vals); rest > 0 {
		ce.dataSection.WriteString(fmt.Sprintf("    .zero %d\n", rest*elemSize))
	}
	return true
}

// constInitValue folds a global initializer to assembly text: (value, kind)
// with kind in {"int", "double", "string"}, or ("", "") if not constant.
func (ce *CodeEmitter) constInitValue(node *ASTNode, sym *Symbol) (string, string) {
	if node == nil {
		return "", ""
	}
	switch node.Type {
	case NodeNumber:
		if node.DataType == "float" || node.DataType == "double" || isFloatSpelling(node.Value) {
			return node.Value, "double"
		}
		return node.Value, "int"
	case NodeString:
		// Reuse an identical literal, else intern a new one.
		for label, s := range ce.stringLits {
			if s == node.Value {
				return label, "string"
			}
		}
		label := fmt.Sprintf(".str_g%d", len(ce.stringLits))
		ce.stringLits[label] = node.Value
		return label, "string"
	case NodeUnaryOp:
		if (node.Operator == "-" || node.Operator == "+") && len(node.Children) == 1 {
			v, k := ce.constInitValue(node.Children[0], sym)
			if k == "int" || k == "double" {
				if node.Operator == "-" {
					return "-" + v, k
				}
				return v, k
			}
		}
	}
	return "", ""
}

func (ce *CodeEmitter) emitTextSection() {
	ce.output.WriteString("    .text\n")
	
	debug := false  // Disable debug
	i := 0
	for i < len(ce.instructions) {
		instr := ce.instructions[i]
		
		if debug {
			fmt.Printf("DEBUG emitTextSection: i=%d, op=%d, label=%v\n", i, instr.Op, instr.Dst)
		}
		
		if instr.Op == OpLabel {
			// Check if this is a function start
			if ce.isFunctionLabel(instr.Dst.Value) {
				if debug {
					fmt.Printf("  -> Emitting function %s, i before=%d\n", instr.Dst.Value, i)
				}
				ce.emitFunction(instr.Dst.Value, &i)
				if debug {
					fmt.Printf("  -> After emitFunction, i=%d\n", i)
				}
				// Don't increment i here - emitFunction already advanced it
				continue
			} else {
				ce.emitLabel(instr.Dst.Value)
			}
		}
		
		i++
	}
}

func (ce *CodeEmitter) isFunctionLabel(label string) bool {
	// Function labels don't start with .
	return !strings.HasPrefix(label, ".")
}

func (ce *CodeEmitter) emitFunction(name string, startIdx *int) {
	ce.currentFunc = name
	
	// Emit function header (local linkage for static/inline-only defs so
	// headers can't multiply-define them at link time).
	if !ce.localFuncs[name] {
		ce.output.WriteString(fmt.Sprintf("\n    .globl %s\n", name))
	}
	ce.output.WriteString(fmt.Sprintf("    .type %s, @function\n", name))
	ce.output.WriteString(fmt.Sprintf("%s:\n", name))
	
	// Prologue
	ce.output.WriteString("    pushq %rbp\n")
	ce.output.WriteString("    movq %rsp, %rbp\n")

	// Save callee-saved registers FIRST so the frame math accounts for
	// them: pushes change rsp, and the sub below must keep 16-alignment
	// (a push without padding misaligned every call -> printf crashed).
	savedRegs := ce.savedRegList()
	for _, reg := range savedRegs {
		ce.output.WriteString(fmt.Sprintf("    pushq %%%s\n", reg))
	}

	// Calculate stack size needed (skip the label instruction itself).
	// Include push bytes so (pushes + sub) stays 16-aligned.
	ce.stackSize = ce.calculateStackSize(*startIdx + 1)
	pushBytes := len(savedRegs) * 8
	total := ce.stackSize + pushBytes
	// Align to 16 bytes
	total = (total + 15) & ^15
	ce.stackSize = total - pushBytes
	if ce.stackSize > 0 {
		ce.output.WriteString(fmt.Sprintf("    subq $%d, %%rsp\n", ce.stackSize))
	}
	
	// Process function body
	*startIdx++
	for *startIdx < len(ce.instructions) {
		instr := ce.instructions[*startIdx]
		
		if instr.Op == OpLabel && ce.isFunctionLabel(instr.Dst.Value) {
			// Next function
			*startIdx--
			break
		}
		
		if instr.Op == OpRet {
			// Return mid-function (e.g. early return in a loop): emit the
			// epilogue but KEEP GOING. Breaking here dropped everything
			// after the first return (loop-back edges, malloc paths,
			// whole function tails) and left jumps dangling into the
			// next function. Unreachable straight-line code after a
			// return costs a few bytes and is always correct.
			ce.emitReturn()
			*startIdx++
			continue
		}
		
		ce.emitInstruction(instr)
		*startIdx++
	}
	
	ce.output.WriteString(fmt.Sprintf("    .size %s, .-%s\n", name, name))
}

func (ce *CodeEmitter) calculateStackSize(startIdx int) int {
	maxOffset := 0
	
	for i := startIdx; i < len(ce.instructions); i++ {
		instr := ce.instructions[i]
		
		if instr.Op == OpLabel && ce.isFunctionLabel(instr.Dst.Value) {
			break
		}
		
		operands := []*Operand{instr.Dst, instr.Src1, instr.Src2}
		for _, op := range operands {
			if op != nil && (op.Type == "mem" || op.Type == "var") && op.Offset < 0 {
				offset := -op.Offset
				if offset > maxOffset {
					maxOffset = offset
				}
			}
		}
	}
	
	return maxOffset
}

// savedRegList returns the callee-saved register names in use (for
// prologue pushes; restores mirror it).
func (ce *CodeEmitter) savedRegList() []string {
	calleeSaved := []int{RBX, R12, R13, R14, R15}
	var out []string
	for _, reg := range calleeSaved {
		for _, usedReg := range ce.usedRegisters {
			if reg == usedReg {
				out = append(out, regNames[reg])
			}
		}
	}
	return out
}

func (ce *CodeEmitter) emitRegisterSaves() {
	calleeSaved := []int{RBX, R12, R13, R14, R15}
	
	for _, reg := range calleeSaved {
		for _, usedReg := range ce.usedRegisters {
			if reg == usedReg {
				ce.output.WriteString(fmt.Sprintf("    pushq %%%s\n", regNames[reg]))
			}
		}
	}
}

func (ce *CodeEmitter) emitRegisterRestores() {
	calleeSaved := []int{R15, R14, R13, R12, RBX}
	
	for _, reg := range calleeSaved {
		for _, usedReg := range ce.usedRegisters {
			if reg == usedReg {
				ce.output.WriteString(fmt.Sprintf("    popq %%%s\n", regNames[reg]))
			}
		}
	}
}

func (ce *CodeEmitter) emitReturn() {
	// Pops must read the push area (just below rbp), not wherever rsp
	// wandered (locals/calls). Reset first, then pop, then tear down.
	n := len(ce.savedRegList())
	if n > 0 {
		ce.output.WriteString(fmt.Sprintf("    leaq -%d(%%rbp), %%rsp\n", n*8))
	}
	ce.emitRegisterRestores()
	ce.output.WriteString("    movq %rbp, %rsp\n")
	ce.output.WriteString("    popq %rbp\n")
	ce.output.WriteString("    ret\n")
}

func (ce *CodeEmitter) emitLabel(label string) {
	ce.output.WriteString(fmt.Sprintf("%s:\n", label))
}

func (ce *CodeEmitter) emitInstruction(instr *IRInstruction) {
	switch instr.Op {
	case OpNop:
		ce.output.WriteString("    nop\n")
		
	case OpMov:
		ce.emitMov(instr.Dst, instr.Src1)
	
	case OpMovFloat:
		ce.emitMovFloat(instr.Dst, instr.Src1)
		
	case OpAdd:
		ce.emitBinaryOp("addq", instr.Dst, instr.Src1, instr.Src2)
		
	case OpSub:
		ce.emitBinaryOp("subq", instr.Dst, instr.Src1, instr.Src2)
		
	case OpMul:
		ce.emitMul(instr.Dst, instr.Src1, instr.Src2)
		
	case OpDiv:
		ce.emitDiv(instr.Dst, instr.Src1, instr.Src2)
		
	case OpMod:
		ce.emitMod(instr.Dst, instr.Src1, instr.Src2)
		
	case OpNeg:
		ce.emitMov(instr.Dst, instr.Src1)
		ce.output.WriteString(fmt.Sprintf("    negq %s\n", ce.formatOperand(instr.Dst)))
		
	case OpAnd:
		ce.emitBinaryOp("andq", instr.Dst, instr.Src1, instr.Src2)
		
	case OpOr:
		ce.emitBinaryOp("orq", instr.Dst, instr.Src1, instr.Src2)
		
	case OpXor:
		ce.emitBinaryOp("xorq", instr.Dst, instr.Src1, instr.Src2)
		
	case OpNot:
		ce.emitMov(instr.Dst, instr.Src1)
		dstStr := ce.formatOperand(instr.Dst)
		if strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")") {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", dstStr))
			ce.output.WriteString("    testq %rax, %rax\n")
			ce.output.WriteString("    sete %al\n")
			ce.output.WriteString("    movzbq %al, %rax\n")
			ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
		} else {
			ce.output.WriteString(fmt.Sprintf("    testq %s, %s\n", dstStr, dstStr))
			ce.output.WriteString("    sete %al\n")
			ce.output.WriteString(fmt.Sprintf("    movzbq %%al, %s\n", dstStr))
		}
		
	case OpShl:
		ce.emitShift("salq", instr.Dst, instr.Src1, instr.Src2)
		
	case OpShr:
		ce.emitShift("sarq", instr.Dst, instr.Src1, instr.Src2)
		
	case OpEq:
		ce.emitComparison("sete", instr.Dst, instr.Src1, instr.Src2)
		
	case OpNe:
		ce.emitComparison("setne", instr.Dst, instr.Src1, instr.Src2)
		
	case OpLt:
		ce.emitComparison("setl", instr.Dst, instr.Src1, instr.Src2)
		
	case OpLe:
		ce.emitComparison("setle", instr.Dst, instr.Src1, instr.Src2)
		
	case OpGt:
		ce.emitComparison("setg", instr.Dst, instr.Src1, instr.Src2)
		
	case OpGe:
		ce.emitComparison("setge", instr.Dst, instr.Src1, instr.Src2)
		
	case OpLoad:
		ce.emitLoad(instr.Dst, instr.Src1)
		
	case OpLoadAddr:
		// Load address of variable/memory location. Dst may be spilled
		// to memory (linear-scan); lea needs a reg dst, so bounce via
		// scratch in that case.
		dstStr := ce.formatOperand(instr.Dst)
		src1 := instr.Src1
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		leaDst := dstStr
		if dstIsMem {
			// RAX is emitter scratch (never holds temps).
			leaDst = "%rax"
		}
		
		if src1.Type == "var" {
			if src1.IsGlobal {
				ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %s\n", src1.Value, leaDst))
			} else {
				ce.output.WriteString(fmt.Sprintf("    leaq %d(%%rbp), %s\n", src1.Offset, leaDst))
			}
		} else if src1.Type == "mem" {
			ce.output.WriteString(fmt.Sprintf("    leaq %d(%%rbp), %s\n", src1.Offset, leaDst))
		} else if src1.Type == "temp" {
			// Temp might have been allocated on stack - check allocator mapping
			// For now, try to use formatOperand but strip the dereference
			srcStr := ce.formatOperand(src1)
			// If srcStr is like offset(%rbp), use leaq with it
			if strings.Contains(srcStr, "(%rbp)") {
				ce.output.WriteString(fmt.Sprintf("    leaq %s, %s\n", srcStr, leaDst))
			} else {
				// Shouldn't happen - fallback to moving the value
				ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcStr, leaDst))
			}
		} else {
			// Fallback
			ce.output.WriteString(fmt.Sprintf("    leaq %s, %s\n", ce.formatOperand(src1), leaDst))
		}
		if dstIsMem {
			ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
		}
		
	case OpStore:
		ce.emitStore(instr.Dst, instr.Src1)
		
	case OpCall:
		ce.emitCall(instr)
		
	case OpJmp:
		ce.output.WriteString(fmt.Sprintf("    jmp %s\n", instr.Dst.Value))
		
	case OpJz:
		src1Str := ce.formatOperand(instr.Src1)
		if instr.Src1.Type == "imm" {
			// test doesn't take two immediates: materialize first.
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", src1Str))
			ce.output.WriteString("    testq %rax, %rax\n")
		} else if strings.Contains(src1Str, "(") && strings.Contains(src1Str, ")") {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", src1Str))
			ce.output.WriteString("    testq %rax, %rax\n")
		} else {
			ce.output.WriteString(fmt.Sprintf("    testq %s, %s\n", src1Str, src1Str))
		}
		ce.output.WriteString(fmt.Sprintf("    jz %s\n", instr.Dst.Value))
		
	case OpJnz:
		src1Str := ce.formatOperand(instr.Src1)
		if instr.Src1.Type == "imm" {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", src1Str))
			ce.output.WriteString("    testq %rax, %rax\n")
		} else if strings.Contains(src1Str, "(") && strings.Contains(src1Str, ")") {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", src1Str))
			ce.output.WriteString("    testq %rax, %rax\n")
		} else {
			ce.output.WriteString(fmt.Sprintf("    testq %s, %s\n", src1Str, src1Str))
		}
		ce.output.WriteString(fmt.Sprintf("    jnz %s\n", instr.Dst.Value))
		
	case OpLabel:
		ce.emitLabel(instr.Dst.Value)
		
	case OpParam:
		// Parameters handled in OpCall
		
	case OpPush:
		ce.output.WriteString(fmt.Sprintf("    pushq %s\n", ce.formatOperand(instr.Src1)))
		
	case OpPop:
		ce.output.WriteString(fmt.Sprintf("    popq %s\n", ce.formatOperand(instr.Dst)))
		
	case OpSetArg:
		// Special handling for setting up function arguments
		// This bypasses the register allocator to avoid conflicts
		ce.emitSetArg(instr)

	case OpStackAlloc:
		ce.output.WriteString(fmt.Sprintf("    subq $%s, %%rsp\n", instr.Dst.Value))

	case OpStackFree:
		ce.output.WriteString(fmt.Sprintf("    addq $%s, %%rsp\n", instr.Dst.Value))

	case OpSetAl:
		ce.output.WriteString(fmt.Sprintf("    mov $%s, %%eax\n", instr.Dst.Value))

	case OpCvt:
		ce.emitCvt(instr.Dst, instr.Src1, instr.Src2.Value)

	default:
		// Never silently drop instructions (a missing case once deleted
		// whole function tails without a sound).
		panic(fmt.Sprintf("code emitter: unhandled opcode %d", instr.Op))
	}
}

// emitArrayLoadFromBase emits the final indexed load; %rdx holds the base,
// %r11 the byte offset. Width follows Size (0/8 = 8 bytes, unchanged path).
func (ce *CodeEmitter) emitArrayLoadFromBase(dst *Operand, dstReg string, size int, indexReg string) {
	idx := strings.TrimPrefix(indexReg, "%")
	switch size {
	case 4:
		dstStr32 := ce.formatOperand32(dst)
		ce.output.WriteString(fmt.Sprintf("    movl (%%rdx, %%%s, 1), %s\n", idx, dstStr32))
	case 2:
		dstStr32 := ce.formatOperand32(dst)
		ce.output.WriteString(fmt.Sprintf("    movzwl (%%rdx, %%%s, 1), %s\n", idx, dstStr32))
	case 1:
		dstStr32 := ce.formatOperand32(dst)
		ce.output.WriteString(fmt.Sprintf("    movzbl (%%rdx, %%%s, 1), %s\n", idx, dstStr32))
	default:
		ce.output.WriteString(fmt.Sprintf("    movq (%%rdx, %%%s, 1), %s\n", idx, dstReg))
	}
}

// emitArrayStoreToBase emits the final indexed store; value is in %rax.
func (ce *CodeEmitter) emitArrayStoreToBase(size int, indexReg string) {
	idx := strings.TrimPrefix(indexReg, "%")
	switch size {
	case 4:
		ce.output.WriteString("    movl %eax, (%rdx, %" + idx + ", 1)\n")
	case 2:
		ce.output.WriteString("    movw %ax, (%rdx, %" + idx + ", 1)\n")
	case 1:
		ce.output.WriteString("    movb %al, (%rdx, %" + idx + ", 1)\n")
	default:
		ce.output.WriteString("    movq %rax, (%rdx, %" + idx + ", 1)\n")
	}
}

// emitFloatCompare lowers a float comparison via ucomiss (both sides to
// double in xmm0/xmm1). Ints convert (signed 32-bit; 64-bit via rax).
func (ce *CodeEmitter) emitFloatCompare(setcc string, dst, src1, src2 *Operand) {
	ce.loadDoubleToXmm(src1, "%xmm0")
	ce.loadDoubleToXmm(src2, "%xmm1")
	// Double compare (operands are doubles; ucomiss would read only
	// the low 32 bits). Flag semantics match ucomiss for the cc map.
	ce.output.WriteString("    ucomisd %xmm1, %xmm0\n")
	// Map signed cc to unsigned (ucomiss uses CF/ZF) + NaN handling.
	cc := setcc
	fix := ""
	switch setcc {
	case "sete":
		cc = "sete"
		fix = "andnp"
	case "setne":
		cc = "setne"
		fix = "orp"
	case "setl":
		cc = "setb"
		fix = "andnp"
	case "setle":
		cc = "setbe"
		fix = "andnp"
	case "setg":
		cc = "seta"
	case "setge":
		cc = "setae"
	}
	ce.output.WriteString(fmt.Sprintf("    %s %%al\n", cc))
	if fix == "andnp" {
		ce.output.WriteString("    setnp %dl\n")
		ce.output.WriteString("    andb %dl, %al\n")
	} else if fix == "orp" {
		ce.output.WriteString("    setp %dl\n")
		ce.output.WriteString("    orb %dl, %al\n")
	}
	dstStr := ce.formatOperand(dst)
	dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
	if dstIsMem {
		ce.output.WriteString("    movzbq %al, %rax\n")
		ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
	} else {
		ce.output.WriteString(fmt.Sprintf("    movzbq %%al, %s\n", dstStr))
	}
}

// loadDoubleToXmm loads any int/float operand to xmm as a double.
func (ce *CodeEmitter) loadDoubleToXmm(op *Operand, xmm string) {
	if op.DataType == "float" {
		ce.emitFloatLoadToXmm(op, xmm)
		return
	}
	if op.DataType == "double" {
		s := ce.formatOperand(op)
		if op.Type == "imm" {
			label := ce.getFloatLabel(op.Value)
			ce.output.WriteString(fmt.Sprintf("    movsd %s(%%rip), %s\n", label, xmm))
		} else if strings.Contains(s, "(") && strings.Contains(s, ")") {
			ce.output.WriteString(fmt.Sprintf("    movsd %s, %s\n", s, xmm))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", s, xmm))
		}
		return
	}
	if op.Type == "imm" && isFloatSpelling(op.Value) {
		label := ce.getFloatLabel(op.Value)
		ce.output.WriteString(fmt.Sprintf("    movsd %s(%%rip), %s\n", label, xmm))
		return
	}
	// Integer to double (signed). 64-bit longs use the q form.
	// Immediates materialize through eax first (cvtsi2sd takes no imm).
	if op.Type == "imm" {
		ce.output.WriteString(fmt.Sprintf("    movq $%s, %%rax\n", op.Value))
		if op.DataType == "long" || op.DataType == "unsigned long" || op.DataType == "long long" {
			ce.output.WriteString(fmt.Sprintf("    cvtsi2sdq %%rax, %s\n", xmm))
		} else {
			ce.output.WriteString(fmt.Sprintf("    cvtsi2sd %%eax, %s\n", xmm))
		}
		return
	}
	is64 := op.DataType == "long" || op.DataType == "unsigned long" || op.DataType == "long long"
	s := ce.formatOperand(op)
	if strings.Contains(s, "(") && strings.Contains(s, ")") {
		if is64 {
			ce.output.WriteString(fmt.Sprintf("    cvtsi2sdq %s, %s\n", s, xmm))
		} else {
			ce.output.WriteString(fmt.Sprintf("    cvtsi2sdl %s, %s\n", s, xmm))
		}
	} else if is64 {
		ce.output.WriteString(fmt.Sprintf("    cvtsi2sdq %s, %s\n", s, xmm))
	} else {
		ce.output.WriteString(fmt.Sprintf("    cvtsi2sd %s, %s\n", ce.formatOperand32(op), xmm))
	}
}

// isInt32Type reports types compared as 32-bit: signed int plus
// char/short (which C promotes to int). Unsigned uses different condition
// codes (pre-existing gap); 64-bit uses cmpq. The 32-bit compare examines
// low 32 only, so zero-extended uppers from movl loads are ignored.
func isInt32Type(t string) bool {
	t = strings.TrimSpace(t)
	if t == "" || strings.Contains(t, "*") {
		return false
	}
	for _, q := range []string{"const ", "volatile ", "static ", "extern ", "register "} {
		t = strings.TrimSpace(strings.TrimPrefix(t, q))
	}
	if strings.Contains(t, "unsigned") {
		return false
	}
	if s, ok := cSizeOf(t); ok {
		return s == 4 || s == 2 || s == 1
	}
	return t == "int"
}

// isFloatSpelling reports whether an immediate spells a float: a dot,
// or a decimal exponent (1e30). Hex integers (0x1e5, whose E is a digit)
// are excluded.
func isFloatSpelling(v string) bool {
	if strings.Contains(v, ".") {
		return true
	}
	if strings.HasPrefix(v, "0x") || strings.HasPrefix(v, "0X") {
		return false
	}
	return strings.ContainsAny(v, "eE")
}

func (ce *CodeEmitter) emitMov(dst, src *Operand) {
	if dst.Type == "reg" && src.Type == "reg" && dst.Value == src.Value {
		return // No-op
	}

	// freg source (xmm param regs): bitwise move out (float params lower
	// into locals; movsd preserves the bit pattern, no conversion).
	if src.Type == "freg" {
		srcStr := "%" + src.Value
		dstStr := ce.formatOperand(dst)
		if strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")") {
			ce.output.WriteString(fmt.Sprintf("    movsd %s, %s\n", srcStr, dstStr))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcStr, dstStr))
		}
		return
	}

	dstStr := ce.formatOperand(dst)
	srcStr := ce.formatOperand(src)
	
	// Handle floating point immediate values (width-correct: float literals
	// materialize as single-precision bits, doubles via .rodata).
	if src.Type == "imm" && isFloatSpelling(src.Value) {
		if src.DataType == "float" {
			bits := floatBits32(src.Value)
			dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
			if dstIsMem {
				ce.output.WriteString(fmt.Sprintf("    movl $%d, %%eax\n", bits))
				ce.output.WriteString(fmt.Sprintf("    movl %%eax, %s\n", dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movl $%d, %s\n", bits, ce.formatOperand32(dst)))
			}
			return
		}
		// It's a double literal - store in .rodata and load address
		label, exists := ce.floatLits[src.Value]
		if !exists {
			ce.floatCounter++
			label = fmt.Sprintf(".FC%d", ce.floatCounter)
			ce.floatLits[label] = src.Value
		}
		// Load the float constant as a 64-bit integer from .rodata
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		if dstIsMem {
			ce.output.WriteString(fmt.Sprintf("    movq %s(%%rip), %%rax\n", label))
			ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movq %s(%%rip), %s\n", label, dstStr))
		}
		return
	}
	
	// Handle label (string literals, addresses) - use leaq
	if src.Type == "label" {
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		if dstIsMem {
			ce.output.WriteString(fmt.Sprintf("    leaq %s, %%rax\n", ce.globalRef(src)))
			ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
		} else {
			ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %s\n", src.Value, dstStr))
		}
		return
	}
	
	// Determine if we need 32-bit mov based on DataType (only for integer types)
	use32Bit := false
	if (src.DataType == "int" || src.DataType == "unsigned int" || src.DataType == "unsigned" || 
	    dst.DataType == "int" || dst.DataType == "unsigned int" || dst.DataType == "unsigned" ||
	    strings.HasPrefix(src.DataType, "enum ") || strings.HasPrefix(dst.DataType, "enum ")) &&
	   src.Type != "imm" && !isFloatSpelling(src.Value) {
		// Use 32-bit mov for integer types when loading from memory
		// But not for immediate integer values (those should use movl anyway)
		if src.Type == "mem" || src.Type == "var" || src.Type == "ptr" {
			use32Bit = true
		}
	}
	
	if use32Bit {
		// Use 32-bit load with sign extension
		dstStr32 := ce.formatOperand32(dst)
		srcStr32 := ce.formatOperand32(src)
		
		// Check for memory-to-memory
		srcIsMem := strings.Contains(srcStr, "(") && strings.Contains(srcStr, ")")
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		
		if srcIsMem && dstIsMem {
			ce.output.WriteString(fmt.Sprintf("    movl %s, %%eax\n", srcStr32))
			ce.output.WriteString(fmt.Sprintf("    movl %%eax, %s\n", dstStr32))
		} else {
			// Use movslq for sign-extending 32-bit to 64-bit when loading to register
			if !dstIsMem && srcIsMem {
				ce.output.WriteString(fmt.Sprintf("    movslq %s, %s\n", srcStr32, dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movl %s, %s\n", srcStr32, dstStr32))
			}
		}
		return
	}
	
	// Handle immediate to memory (movabs for wide constants)
	if dst.Type == "mem" && src.Type == "imm" {
		if !isFloatSpelling(src.Value) {
			ce.emitImmToReg64(src.Value, "%rax")
		} else {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", srcStr))
		}
		ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
		return
	}
	
	// Check for memory-to-memory by pattern (both have (%reg) syntax)
	srcIsMem := strings.Contains(srcStr, "(") && strings.Contains(srcStr, ")")
	dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
	
	if srcIsMem && dstIsMem {
		// Memory to memory through register
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", srcStr))
		ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
		return
	}

	// Wide integer immediates need movabs (plain movq can't encode them).
	if src.Type == "imm" && !isFloatSpelling(src.Value) {
		if dstIsMem {
			ce.emitImmToReg64(src.Value, "%rax")
			ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
		} else {
			ce.emitImmToReg64(src.Value, dstStr)
		}
		return
	}

	ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcStr, dstStr))
}

func (ce *CodeEmitter) emitMovFloat(dst, src *Operand) {
	// Move floating point values using XMM registers and movsd
	dstStr := ce.formatOperand(dst)
	srcStr := ce.formatOperand(src)
	
	// Handle floating point immediate values
	if src.Type == "imm" {
		label := ce.getFloatLabel(src.Value)
		
		// Load the float constant using movsd
		if dst.Type == "freg" {
			ce.output.WriteString(fmt.Sprintf("    movsd %s(%%rip), %%%s\n", label, dst.Value))
		} else {
			// Load to temp XMM register first, then move to destination
			ce.output.WriteString(fmt.Sprintf("    movsd %s(%%rip), %%xmm0\n", label))
			ce.output.WriteString(fmt.Sprintf("    movsd %%xmm0, %s\n", dstStr))
		}
		return
	}
	
	// Source is in memory or temp register, destination is XMM register
	if dst.Type == "freg" {
		if src.Type == "temp" || src.Type == "reg" {
			// Move from GPR to XMM (use movq for bit pattern transfer)
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%%s\n", srcStr, dst.Value))
		} else {
			// Load from memory to XMM
			ce.output.WriteString(fmt.Sprintf("    movsd %s, %%%s\n", srcStr, dst.Value))
		}
		return
	}
	
	// If not a float register destination, fall back to regular mov
	ce.emitMov(dst, src)
}
func (ce *CodeEmitter) emitBinaryOp(op string, dst, src1, src2 *Operand) {
	// Check if this is a float operation
	isFloat := (dst.DataType == "float" || dst.DataType == "double" ||
	            src1.DataType == "float" || src1.DataType == "double" ||
	            src2.DataType == "float" || src2.DataType == "double")
	
	if isFloat {
		// Float operation using SSE instructions (computed in double;
		// single-precision inputs convert up, float results convert back).
		floatOp := ""
		switch op {
		case "addq":
			floatOp = "addsd"
		case "subq":
			floatOp = "subsd"
		default:
			// Fallback to integer operation for unsupported float ops
			isFloat = false
		}

		if isFloat {
			ce.emitFloatLoadToXmm(src1, "%xmm0")

			// Apply operation with src2 (via xmm1 to keep forms uniform).
			ce.emitFloatLoadToXmm(src2, "%xmm1")
			ce.output.WriteString(fmt.Sprintf("    %s %%xmm1, %%xmm0\n", floatOp))

			// Store result (converts back for float destinations).
			ce.emitFloatStoreFromXmm("%xmm0", dst)
			return
		}
	}
	
	// Integer operation
	// Move src1 to dst
	ce.emitMov(dst, src1)
	
	// Apply operation - handle float immediates, then oversized ints
	src2Str := ce.loadFloatIfNeeded(src2, "%r10")
	if src2Str == ce.formatOperand(src2) {
		src2Str = ce.loadIntImmIfNeeded(src2, "%r10")
	}
	dstStr := ce.formatOperand(dst)
	
	if dst.Type == "mem" && src2.Type == "mem" {
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", src2Str))
		ce.output.WriteString(fmt.Sprintf("    %s %%rax, %s\n", op, dstStr))
	} else {
		ce.output.WriteString(fmt.Sprintf("    %s %s, %s\n", op, src2Str, dstStr))
	}
}

func (ce *CodeEmitter) emitMul(dst, src1, src2 *Operand) {
	// Check if this is a float operation
	isFloat := (dst.DataType == "float" || dst.DataType == "double" ||
	            src1.DataType == "float" || src1.DataType == "double" ||
	            src2.DataType == "float" || src2.DataType == "double")
	
	if isFloat {
		// Float multiplication (double precision, float converts both ways).
		ce.emitFloatLoadToXmm(src1, "%xmm0")
		ce.emitFloatLoadToXmm(src2, "%xmm1")
		ce.output.WriteString("    mulsd %xmm1, %xmm0\n")
		ce.emitFloatStoreFromXmm("%xmm0", dst)
		return
	}
	
	// Integer multiplication
	ce.emitMov(dst, src1)
	
	src2Str := ce.loadFloatIfNeeded(src2, "%r10")
	dstStr := ce.formatOperand(dst)
	dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
	
	if dstIsMem {
		// imul doesn't support memory destination - use rax
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", dstStr))
		ce.output.WriteString(fmt.Sprintf("    imulq %s, %%rax\n", src2Str))
		ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
	} else {
		ce.output.WriteString(fmt.Sprintf("    imulq %s, %s\n", src2Str, dstStr))
	}
}

func (ce *CodeEmitter) emitDiv(dst, src1, src2 *Operand) {
	// Check if this is a float operation
	isFloat := (dst.DataType == "float" || dst.DataType == "double" ||
	            src1.DataType == "float" || src1.DataType == "double" ||
	            src2.DataType == "float" || src2.DataType == "double")
	
	if isFloat {
		// Float division (double precision, float converts both ways).
		ce.emitFloatLoadToXmm(src1, "%xmm0")
		ce.emitFloatLoadToXmm(src2, "%xmm1")
		ce.output.WriteString("    divsd %xmm1, %xmm0\n")
		ce.emitFloatStoreFromXmm("%xmm0", dst)
		return
	}
	
	// Division requires RAX and RDX
	// Unsigned (either side unsigned per C conversions) uses div, not idiv.
	// The selector normalizes the result DataType to an unsigned spelling,
	// so a prefix check here is sufficient (no typedef table in emitter).
	isUnsigned := isUnsignedPrefix(dst.DataType) || isUnsignedPrefix(src1.DataType) || isUnsignedPrefix(src2.DataType)
	divOp, extOp := "idivl", "cdq"
	divOp64, extOp64 := "idivq", "cqto"
	if isUnsigned {
		divOp, extOp = "divl", "xorl %edx, %edx"
		divOp64, extOp64 = "divq", "xorq %rdx, %rdx"
	}
	// Check if we're working with 32-bit integers
	use32Bit := (src2.DataType == "int" || src2.DataType == "unsigned int" || src2.DataType == "unsigned" ||
		strings.HasPrefix(src2.DataType, "enum "))

	if use32Bit {
		// 32-bit division
		ce.output.WriteString(fmt.Sprintf("    movl %s, %%eax\n", ce.formatOperand32(src1)))
		ce.output.WriteString("    " + extOp + "\n") // sign- or zero-extend EAX to EDX:EAX

		if src2.Type == "imm" {
			ce.output.WriteString(fmt.Sprintf("    movl $%s, %%r11d\n", src2.Value))
			ce.output.WriteString("    " + divOp + " %r11d\n")
		} else {
			ce.output.WriteString(fmt.Sprintf("    "+divOp+" %s\n", ce.formatOperand32(src2)))
		}

		ce.output.WriteString(fmt.Sprintf("    movl %%eax, %s\n", ce.formatOperand32(dst)))
	} else {
		// 64-bit division (original code)
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", ce.formatOperand(src1)))
		ce.output.WriteString("    " + extOp64 + "\n")

		if src2.Type == "imm" {
			src2Str := ce.loadFloatIfNeeded(src2, "%r11")
			if src2Str == "%r11" {
				ce.output.WriteString("    " + divOp64 + " %r11\n")
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %%r11\n", src2Str))
				ce.output.WriteString("    " + divOp64 + " %r11\n")
			}
		} else {
			ce.output.WriteString(fmt.Sprintf("    "+divOp64+" %s\n", ce.formatOperand(src2)))
		}

		ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", ce.formatOperand(dst)))
	}
}

func (ce *CodeEmitter) emitMod(dst, src1, src2 *Operand) {
	// Modulo - result in RDX
	isUnsigned := isUnsignedPrefix(dst.DataType) || isUnsignedPrefix(src1.DataType) || isUnsignedPrefix(src2.DataType)
	divOp, extOp := "idivl", "cdq"
	divOp64, extOp64 := "idivq", "cqto"
	if isUnsigned {
		divOp, extOp = "divl", "xorl %edx, %edx"
		divOp64, extOp64 = "divq", "xorq %rdx, %rdx"
	}
	// Check if we're working with 32-bit integers
	use32Bit := (src2.DataType == "int" || src2.DataType == "unsigned int" || src2.DataType == "unsigned" ||
		strings.HasPrefix(src2.DataType, "enum "))

	if use32Bit {
		// 32-bit division
		ce.output.WriteString(fmt.Sprintf("    movl %s, %%eax\n", ce.formatOperand32(src1)))
		ce.output.WriteString("    " + extOp + "\n")

		if src2.Type == "imm" {
			ce.output.WriteString(fmt.Sprintf("    movl $%s, %%r11d\n", src2.Value))
			ce.output.WriteString("    " + divOp + " %r11d\n")
		} else {
			ce.output.WriteString(fmt.Sprintf("    "+divOp+" %s\n", ce.formatOperand32(src2)))
		}

		ce.output.WriteString(fmt.Sprintf("    movl %%edx, %s\n", ce.formatOperand32(dst)))
	} else {
		// 64-bit division (original code)
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", ce.formatOperand(src1)))
		ce.output.WriteString("    " + extOp64 + "\n")

		if src2.Type == "imm" {
			src2Str := ce.loadFloatIfNeeded(src2, "%r11")
			if src2Str == "%r11" {
				ce.output.WriteString("    " + divOp64 + " %r11\n")
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %%r11\n", src2Str))
				ce.output.WriteString("    " + divOp64 + " %r11\n")
			}
		} else {
			ce.output.WriteString(fmt.Sprintf("    "+divOp64+" %s\n", ce.formatOperand(src2)))
		}

		ce.output.WriteString(fmt.Sprintf("    movq %%rdx, %s\n", ce.formatOperand(dst)))
	}
}

// isUnsignedPrefix reports whether a (possibly unresolved) type spelling is
// unsigned. The selector resolves aliases into the result DataType, so the
// emitter only needs the spelling check.

// isUnsignedPrefix reports whether a (possibly unresolved) type spelling is
// unsigned. The selector resolves aliases into the result DataType, so the
// emitter only needs the spelling check.
func isUnsignedPrefix(t string) bool {
	t = strings.TrimSpace(t)
	for _, q := range []string{"const ", "volatile ", "register ", "static "} {
		t = strings.TrimSpace(strings.TrimPrefix(t, q))
	}
	for _, w := range strings.Fields(t) {
		if w == "unsigned" {
			return true
		}
	}
	return false
}

func (ce *CodeEmitter) emitShift(op string, dst, src1, src2 *Operand) {
	ce.emitMov(dst, src1)
	
	// Shift amount must be in CL
	if src2.Type != "imm" {
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rcx\n", ce.formatOperand(src2)))
		ce.output.WriteString(fmt.Sprintf("    %s %%cl, %s\n", op, ce.formatOperand(dst)))
	} else {
		// Handle float immediates
		src2Str := ce.loadFloatIfNeeded(src2, "%rcx")
		if src2Str == "%rcx" {
			ce.output.WriteString(fmt.Sprintf("    %s %%cl, %s\n", op, ce.formatOperand(dst)))
		} else {
			ce.output.WriteString(fmt.Sprintf("    %s %s, %s\n", op, src2Str, ce.formatOperand(dst)))
		}
	}
}

// floatOperand reports whether an operand is float/double (by type or
// immediate spelling) for comparison routing.
func floatOperand(op *Operand) bool {
	if op == nil {
		return false
	}
	if op.DataType == "float" || op.DataType == "double" {
		return true
	}
	if op.Type == "imm" && isFloatSpelling(op.Value) {
		return true
	}
	return false
}

func (ce *CodeEmitter) emitComparison(setcc string, dst, src1, src2 *Operand) {
	// Any float side -> ucomiss in double (integer bit-cmp is meaningless
	// for mixed int/float or negatives). NaN fixes: </<=/== clear on
	// unordered (andnp); != sets on unordered (or-p); >/>= are exact.
	if floatOperand(src1) || floatOperand(src2) {
		ce.emitFloatCompare(setcc, dst, src1, src2)
		return
	}
	// 32-bit signed comparison uses cmpl (examines low 32 only, so
	// zero-extended upper garbage from movl loads is ignored). cmpq
	// would misread int -1 (0xFFFFFFFF) as +4B. Both sides must be
	// 32-bit int (mixed widths promote to 64-bit elsewhere).
	cmpOp := "cmpq"
	if isInt32Type(src1.DataType) && isInt32Type(src2.DataType) {
		cmpOp = "cmpl"
	}
	src1Str := ce.formatOperand(src1)
	src2Str := ce.formatOperand(src2)
	if cmpOp == "cmpl" {
		src1Str = ce.formatOperand32(src1)
		src2Str = ce.formatOperand32(src2)
	}
	
	// Handle float immediates - they need to be in .rodata.
	// Either side can be immediate (e.g. `1.5f < 3.5f` folds neither
	// side); each gets its own scratch register.
	if src1.Type == "imm" && (src1.DataType == "float" || src1.DataType == "double") {
		label := ce.getFloatLabel(src1.Value)
		ce.output.WriteString(fmt.Sprintf("    movq %s(%%rip), %%r11\n", label))
		src1Str = "%r11"
	}
	if src2.Type == "imm" && (src2.DataType == "float" || src2.DataType == "double") {
		label := ce.getFloatLabel(src2.Value)
		// Load into a register for comparison
		ce.output.WriteString(fmt.Sprintf("    movq %s(%%rip), %%r10\n", label))
		src2Str = "%r10"
	} else if src2Str == ce.formatOperand(src2) {
		src2Str = ce.loadIntImmIfNeeded(src2, "%r10")
	}
	if src1Str == ce.formatOperand(src1) {
		src1Str = ce.loadIntImmIfNeeded(src1, "%r11")
	}
	
	src1IsMem := strings.Contains(src1Str, "(") && strings.Contains(src1Str, ")")
	src2IsMem := strings.Contains(src2Str, "(") && strings.Contains(src2Str, ")")
	
	if src1IsMem && src2IsMem {
		// Both are memory - load one into register
		if cmpOp == "cmpl" {
			ce.output.WriteString(fmt.Sprintf("    movl %s, %%eax\n", src1Str))
			ce.output.WriteString(fmt.Sprintf("    cmpl %s, %%eax\n", src2Str))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", src1Str))
			ce.output.WriteString(fmt.Sprintf("    cmpq %s, %%rax\n", src2Str))
		}
	} else {
		ce.output.WriteString(fmt.Sprintf("    %s %s, %s\n", cmpOp, src2Str, src1Str))
	}
	
	ce.output.WriteString(fmt.Sprintf("    %s %%al\n", setcc))
	
	dstStr := ce.formatOperand(dst)
	dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
	
	if dstIsMem {
		ce.output.WriteString("    movzbq %al, %rax\n")
		ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
	} else {
		ce.output.WriteString(fmt.Sprintf("    movzbq %%al, %s\n", dstStr))
	}
}

func (ce *CodeEmitter) emitLoad(dst, src *Operand) {
	switch src.Type {
	case "var":
		dstStr := ce.formatOperand(dst)
		// Check if destination is also memory
		if strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")") {
			// Load through register - use appropriate size
			if src.Size > 0 && src.Size < 8 {
				if src.IsGlobal {
					unsigned := strings.Contains(src.DataType, "unsigned")
					if src.Size == 4 {
						// Load 32-bit and zero-extend
						ce.output.WriteString(fmt.Sprintf("    movl %s, %%eax\n", ce.globalRef(src)))
					} else if src.Size == 2 {
						if unsigned {
							ce.output.WriteString(fmt.Sprintf("    movzwl %s, %%eax\n", ce.globalRef(src)))
						} else {
							ce.output.WriteString(fmt.Sprintf("    movswl %s, %%eax\n", ce.globalRef(src)))
						}
					} else if src.Size == 1 {
						if unsigned {
							ce.output.WriteString(fmt.Sprintf("    movzbl %s, %%eax\n", ce.globalRef(src)))
						} else {
							ce.output.WriteString(fmt.Sprintf("    movsbl %s, %%eax\n", ce.globalRef(src)))
						}
					}
				} else {
					unsigned := strings.Contains(src.DataType, "unsigned")
					if src.Size == 4 {
						ce.output.WriteString(fmt.Sprintf("    movl %d(%%rbp), %%eax\n", src.Offset))
					} else if src.Size == 2 {
						if unsigned {
							ce.output.WriteString(fmt.Sprintf("    movzwl %d(%%rbp), %%eax\n", src.Offset))
						} else {
							ce.output.WriteString(fmt.Sprintf("    movswl %d(%%rbp), %%eax\n", src.Offset))
						}
					} else if src.Size == 1 {
						if unsigned {
							ce.output.WriteString(fmt.Sprintf("    movzbl %d(%%rbp), %%eax\n", src.Offset))
						} else {
							ce.output.WriteString(fmt.Sprintf("    movsbl %d(%%rbp), %%eax\n", src.Offset))
						}
					}
				}
				// movl to %eax zeros upper 32 bits of %rax
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
			} else {
				// Default 8-byte load
				if src.IsGlobal {
					ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", ce.globalRef(src)))
				} else {
					ce.output.WriteString(fmt.Sprintf("    movq %d(%%rbp), %%rax\n", src.Offset))
				}
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
			}
		} else {
				// Direct load to register - use appropriate size
				if src.Size > 0 && src.Size < 8 {
					if src.IsGlobal {
						// For direct register loads, we can use movl directly which zeros upper bits
						dstStr32 := ce.formatOperand32(dst)
						unsigned := strings.Contains(src.DataType, "unsigned")
						if src.Size == 4 {
								ce.output.WriteString(fmt.Sprintf("    movl %s, %s\n", ce.globalRef(src), dstStr32))
						} else if src.Size == 2 {
								if unsigned {
									ce.output.WriteString(fmt.Sprintf("    movzwl %s, %s\n", ce.globalRef(src), dstStr32))
								} else {
									ce.output.WriteString(fmt.Sprintf("    movswl %s, %s\n", ce.globalRef(src), dstStr32))
								}
						} else if src.Size == 1 {
								if unsigned {
									ce.output.WriteString(fmt.Sprintf("    movzbl %s, %s\n", ce.globalRef(src), dstStr32))
								} else {
									ce.output.WriteString(fmt.Sprintf("    movsbl %s, %s\n", ce.globalRef(src), dstStr32))
								}
						}
					} else {
					// Local variable load (signed types sign-extend;
					// plain char/short are signed on x86-64).
					dstStr32 := ce.formatOperand32(dst)
					unsigned := strings.Contains(src.DataType, "unsigned")
					if src.Size == 4 {
						ce.output.WriteString(fmt.Sprintf("    movl %d(%%rbp), %s\n", src.Offset, dstStr32))
					} else if src.Size == 2 {
						if unsigned {
							ce.output.WriteString(fmt.Sprintf("    movzwl %d(%%rbp), %s\n", src.Offset, dstStr32))
						} else {
							ce.output.WriteString(fmt.Sprintf("    movswl %d(%%rbp), %s\n", src.Offset, dstStr32))
						}
					} else if src.Size == 1 {
						if unsigned {
							ce.output.WriteString(fmt.Sprintf("    movzbl %d(%%rbp), %s\n", src.Offset, dstStr32))
						} else {
							ce.output.WriteString(fmt.Sprintf("    movsbl %d(%%rbp), %s\n", src.Offset, dstStr32))
						}
					}
				}
			} else {
				// Default 8-byte load
				if src.IsGlobal {
					ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", ce.globalRef(src), dstStr))
				} else {
					ce.output.WriteString(fmt.Sprintf("    movq %d(%%rbp), %s\n", src.Offset, dstStr))
				}
			}
		}
	case "array":
		// Load from array[index]: base(%rbp) + index_temp
		indexReg := ce.formatOperand(src.IndexTemp)
		dstReg := ce.formatOperand(dst)
		
		// Index into r11 unless already a register (a blind copy would
		// clobber a live temp allocated to r11; immediates can't encode
		// in SIB addressing at all).
		if !strings.HasPrefix(indexReg, "%") {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%r11\n", indexReg))
			indexReg = "%r11"
		}
		
		if src.IsGlobal {
			// Global array: load from symbol + offset
			ce.output.WriteString(fmt.Sprintf("    leaq %s, %%rdx\n", ce.globalRef(src)))
			ce.emitArrayLoadFromBase(dst, dstReg, src.Size, indexReg)
		} else {
			// Local array: load from rbp + base_offset + computed_offset
			ce.output.WriteString(fmt.Sprintf("    leaq %d(%%rbp), %%rdx\n", src.Offset))
			ce.emitArrayLoadFromBase(dst, dstReg, src.Size, indexReg)
		}
	case "addr":
		// Address-of: compute address and store in dst
		dstStr := ce.formatOperand(dst)
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		
		if dstIsMem {
			// Destination is memory, go through rax
			if src.IsGlobal {
				ce.output.WriteString(fmt.Sprintf("    leaq %s, %%rax\n", ce.globalRef(src)))
			} else {
				ce.output.WriteString(fmt.Sprintf("    leaq %d(%%rbp), %%rax\n", src.Offset))
			}
			ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
		} else {
			// Destination is register
			if src.IsGlobal {
				ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %s\n", src.Value, dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    leaq %d(%%rbp), %s\n", src.Offset, dstStr))
			}
		}
	case "ptr":
		// Dereference: load from address in IndexTemp
		ptrReg := ce.formatOperand(src.IndexTemp)
		ptrIsMem := strings.Contains(ptrReg, "(") && strings.Contains(ptrReg, ")")
		
		dstStr := ce.formatOperand(dst)
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		
		// If pointer is in memory, load it first
		if ptrIsMem {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%r11\n", ptrReg))
			ptrReg = "%r11"
		}
		
		// Determine the appropriate mov instruction based on data type
		// Strip qualifiers like const, volatile, etc.
		dataType := strings.TrimSpace(src.DataType)
		dataType = strings.TrimPrefix(dataType, "const ")
		dataType = strings.TrimPrefix(dataType, "volatile ")
		dataType = strings.TrimPrefix(dataType, "register ")
		dataType = strings.TrimSpace(dataType)
		
		movInstr := "movq"
		// Signedness matters: plain char/short are SIGNED on x86-64
		// (movsbl/movswl); only unsigned uses zero-extension.
		isUnsigned := strings.Contains(dataType, "unsigned")
		if dataType == "char" || dataType == "signed char" || dataType == "unsigned char" {
			movInstr = "movb"
		} else if dataType == "short" || dataType == "short int" || dataType == "signed short" || dataType == "unsigned short" {
			movInstr = "movw"
		} else if dataType == "int" || dataType == "signed int" || dataType == "unsigned int" {
			movInstr = "movl"
		}
		// Explicit Size wins (covers enums/typedefs the name table misses).
		if src.Size == 4 {
			movInstr = "movl"
		} else if src.Size == 2 {
			movInstr = "movw"
		} else if src.Size == 1 {
			movInstr = "movb"
		}
		
		if dstIsMem {
			if movInstr == "movb" {
				if isUnsigned {
					ce.output.WriteString(fmt.Sprintf("    movb (%s), %%al\n", ptrReg))
					ce.output.WriteString("    movzbq %al, %rax\n")  // Zero-extend to 64-bit
				} else {
					ce.output.WriteString(fmt.Sprintf("    movsbq (%s), %%rax\n", ptrReg))
				}
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
			} else if movInstr == "movw" {
				if isUnsigned {
					ce.output.WriteString(fmt.Sprintf("    movzwl (%s), %%eax\n", ptrReg))
				} else {
					ce.output.WriteString(fmt.Sprintf("    movswl (%s), %%eax\n", ptrReg))
				}
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
			} else if movInstr == "movl" {
				// movl zeros upper 32 bits automatically
				ce.output.WriteString(fmt.Sprintf("    movl (%s), %%eax\n", ptrReg))
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq (%s), %%rax\n", ptrReg))
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
			}
		} else {
			// Register destination: load DIRECTLY into it (no rax staging:
			// a hardcoded scratch register would clobber a live temp
			// allocated there — this exact bug returned 0 for a[0]-b[0].
			if movInstr == "movb" {
				dstStr32 := ce.formatOperand32(dst)
				if isUnsigned {
					ce.output.WriteString(fmt.Sprintf("    movzbl (%s), %s\n", ptrReg, dstStr32))
				} else {
					ce.output.WriteString(fmt.Sprintf("    movsbl (%s), %s\n", ptrReg, dstStr32))
				}
			} else if movInstr == "movw" {
				dstStr32 := ce.formatOperand32(dst)
				if isUnsigned {
					ce.output.WriteString(fmt.Sprintf("    movzwl (%s), %s\n", ptrReg, dstStr32))
				} else {
					ce.output.WriteString(fmt.Sprintf("    movswl (%s), %s\n", ptrReg, dstStr32))
				}
			} else if movInstr == "movl" {
				// movl to 32-bit register zeros upper 32 bits
				dstStr32 := ce.formatOperand32(dst)
				ce.output.WriteString(fmt.Sprintf("    movl (%s), %s\n", ptrReg, dstStr32))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq (%s), %s\n", ptrReg, dstStr))
			}
		}
	case "label":
		// String literal or global label - use leaq to load address
		dstStr := ce.formatOperand(dst)
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		
		if dstIsMem {
			ce.output.WriteString(fmt.Sprintf("    leaq %s, %%rax\n", ce.globalRef(src)))
			ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
		} else {
			ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %s\n", src.Value, dstStr))
		}
	case "mem":
		// Load from stack location
		dstStr := ce.formatOperand(dst)
		srcStr := ce.formatOperand(src) // This will be offset(%rbp)
		
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		
		if dstIsMem {
			// Mem to mem - use intermediate register
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", srcStr))
			ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
		} else {
			// Mem to register - direct move
			ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcStr, dstStr))
		}
	default:
		ce.emitMov(dst, src)
	}
}

// emitSizedStoreToGlobal stores a register to a global symbol, honoring
// narrow widths so 4-byte globals don't clobber their BSS neighbors.
func (ce *CodeEmitter) emitSizedStoreToGlobal(dst *Operand, srcReg string) {
	if dst.Size == 4 {
		ce.output.WriteString(fmt.Sprintf("    movl %s, %s\n", "%"+ce.reg64to32(strings.TrimPrefix(srcReg, "%")), ce.globalRef(dst)))
	} else if dst.Size == 2 {
		ce.output.WriteString(fmt.Sprintf("    movw %s, %s\n", ce.get16BitReg(srcReg), ce.globalRef(dst)))
	} else if dst.Size == 1 {
		ce.output.WriteString(fmt.Sprintf("    movb %s, %s\n", ce.get8BitReg(srcReg), ce.globalRef(dst)))
	} else {
		ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcReg, ce.globalRef(dst)))
	}
}

// emitFloatStoreConv handles float<->double width mismatches on stores.
// Returns true if it emitted (caller returns). Bitwise copies are only
// correct when widths agree.
func (ce *CodeEmitter) emitFloatStoreConv(dst, src *Operand) bool {
	srcIsFloat := src.DataType == "float"
	srcIsDouble := src.DataType == "double"
	if !srcIsFloat && !srcIsDouble {
		return false
	}
	// Destination width: explicit Size wins, else float/double DataType.
	// Pointer/array destinations without Size are punted (generic path).
	dstWidth := dst.Size
	if dstWidth == 0 {
		if dst.DataType == "float" {
			dstWidth = 4
		} else if dst.DataType == "double" {
			dstWidth = 8
		} else {
			return false
		}
	}
	if srcIsFloat && dstWidth == 8 {
		// float -> double: convert up.
		ce.emitFloatLoadToXmm(src, "%xmm0")
		dstStr := ce.formatOperand(dst)
		if strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")") {
			ce.output.WriteString(fmt.Sprintf("    movsd %%xmm0, %s\n", dstStr))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movq %%xmm0, %%xmm1\n"))
			ce.output.WriteString(fmt.Sprintf("    movq %%xmm1, %s\n", dstStr))
		}
		return true
	}
	if srcIsDouble && dstWidth == 4 {
		// double -> float: convert down.
		ce.emitFloatLoadToXmm(src, "%xmm0")
		dstStr := ce.formatOperand(dst)
		ce.output.WriteString(fmt.Sprintf("    cvtsd2ss %%xmm0, %%xmm0\n"))
		if strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")") {
			ce.output.WriteString(fmt.Sprintf("    movss %%xmm0, %s\n", dstStr))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movd %%xmm0, %s\n", ce.formatOperand32(dst)))
		}
		return true
	}
	return false
}

func (ce *CodeEmitter) emitStore(dst, src *Operand) {
	// Float<->double cross-width stores need conversion (bit copy is wrong).
	if ce.emitFloatStoreConv(dst, src) {
		return
	}
	switch dst.Type {
	case "var":
		// Special handling for label sources (string literals)
		if src.Type == "label" {
			if dst.IsGlobal {
				ce.output.WriteString(fmt.Sprintf("    leaq %s, %%rax\n", ce.globalRef(src)))
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", ce.globalRef(dst)))
			} else {
				ce.output.WriteString(fmt.Sprintf("    leaq %s, %%rax\n", ce.globalRef(src)))
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %d(%%rbp)\n", dst.Offset))
			}
			return
		}
		
		srcStr := ce.formatOperand(src)
		srcIsMem := strings.Contains(srcStr, "(") && strings.Contains(srcStr, ")")
		
		if dst.IsGlobal {
			if src.Type == "imm" || srcIsMem {
				// Handle float immediates
				loadedStr := ce.loadFloatIfNeeded(src, "%rax")
				if loadedStr != "%rax" {
					ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", loadedStr))
				}
				ce.emitSizedStoreToGlobal(dst, "%rax")
			} else {
				ce.emitSizedStoreToGlobal(dst, srcStr)
			}
		} else {
			if srcIsMem || src.Type == "imm" {
				if src.Type == "imm" && src.DataType == "float" {
				// Single-precision literal: materialize exact bits.
				ce.output.WriteString(fmt.Sprintf("    movl $%d, %%eax\n", floatBits32(src.Value)))
				} else if src.Type == "imm" && (src.DataType == "double" || isFloatSpelling(src.Value)) {
				// Double immediate via .rodata.
				loadedStr := ce.loadFloatIfNeeded(src, "%rax")
				if loadedStr != "%rax" {
					ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", loadedStr))
				}
				} else if src.Type == "imm" {
					// Integer immediate: movabs when too wide for imm32
					// (a plain movl silently truncated UL constants).
					if dst.Size > 0 && dst.Size < 8 {
						ce.output.WriteString(fmt.Sprintf("    movl $%s, %%eax\n", src.Value))
					} else {
						ce.emitImmToReg64(src.Value, "%rax")
					}
				} else {
					ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", ce.formatOperand(src)))
				}
				// Check if we have a specific size to store
				if dst.Size > 0 && dst.Size < 8 {
					// Use appropriately sized move instruction
					if dst.Size == 4 {
						ce.output.WriteString(fmt.Sprintf("    movl %%eax, %d(%%rbp)\n", dst.Offset))
					} else if dst.Size == 2 {
						ce.output.WriteString(fmt.Sprintf("    movw %%ax, %d(%%rbp)\n", dst.Offset))
					} else if dst.Size == 1 {
						ce.output.WriteString(fmt.Sprintf("    movb %%al, %d(%%rbp)\n", dst.Offset))
					} else {
						ce.output.WriteString(fmt.Sprintf("    movq %%rax, %d(%%rbp)\n", dst.Offset))
					}
				} else {
				// Wide slot: %rax already holds the full value.
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %d(%%rbp)\n", dst.Offset))
			}
			} else if dst.Size > 0 && dst.Size < 8 {
				// Register source into a narrow slot (e.g. i = computed int).
				if dst.Size == 4 {
					ce.output.WriteString(fmt.Sprintf("    movl %s, %d(%%rbp)\n", ce.formatOperand32(src), dst.Offset))
				} else if dst.Size == 2 {
					ce.output.WriteString(fmt.Sprintf("    movw %s, %d(%%rbp)\n", ce.get16BitReg(srcStr), dst.Offset))
				} else if dst.Size == 1 {
					ce.output.WriteString(fmt.Sprintf("    movb %s, %d(%%rbp)\n", ce.get8BitReg(srcStr), dst.Offset))
				} else {
					ce.output.WriteString(fmt.Sprintf("    movq %s, %d(%%rbp)\n", srcStr, dst.Offset))
				}
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %d(%%rbp)\n", srcStr, dst.Offset))
			}
		}
	case "array":
		// Store to array[index]: base(%rbp) + index_temp
		// IMPORTANT: Use separate registers for index and value!
		indexReg := ce.formatOperand(dst.IndexTemp)
		
		// Index into r11 unless already a register (a blind copy would
		// clobber a live temp allocated to r11; immediates can't encode
		// in SIB addressing at all).
		if !strings.HasPrefix(indexReg, "%") {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%r11\n", indexReg))
			indexReg = "%r11"
		}
		
		// Get source value into rax (movabs for wide integer immediates)
		srcReg := ce.formatOperand(src)
		if src.Type == "imm" && src.DataType == "float" {
			ce.output.WriteString(fmt.Sprintf("    movl $%d, %%eax\n", floatBits32(src.Value)))
		} else if src.Type == "imm" && !isFloatSpelling(src.Value) && src.DataType != "float" && src.DataType != "double" {
			ce.emitImmToReg64(src.Value, "%rax")
		} else if src.Type == "imm" {
			// Use helper for float immediates
			loadedStr := ce.loadFloatIfNeeded(src, "%rax")
			if loadedStr != "%rax" {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", loadedStr))
			}
		} else {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", srcReg))
		}
		
		if dst.IsGlobal {
			// Global array: store to symbol + offset
			ce.output.WriteString(fmt.Sprintf("    leaq %s, %%rdx\n", ce.globalRef(dst)))
			ce.emitArrayStoreToBase(dst.Size, indexReg)
		} else {
			// Local array: store to rbp + base_offset + computed_offset
			ce.output.WriteString(fmt.Sprintf("    leaq %d(%%rbp), %%rdx\n", dst.Offset))
			ce.emitArrayStoreToBase(dst.Size, indexReg)
		}
	case "ptr":
		// Dereference store: store to address in IndexTemp
		ptrReg := ce.formatOperand(dst.IndexTemp)
		ptrIsMem := strings.Contains(ptrReg, "(") && strings.Contains(ptrReg, ")")
		
		// Load pointer into a dedicated register if it's in memory
		if ptrIsMem {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%r11\n", ptrReg))
			ptrReg = "%r11"
		}
		
		srcReg := ce.formatOperand(src)
		srcIsMem := strings.Contains(srcReg, "(") && strings.Contains(srcReg, ")")
		
		// Choose a value register that won't clobber the pointer
		valueReg := "%rax"
		if ptrReg == "%rax" {
			valueReg = "%r10"
		}
		
		if src.Type == "imm" || srcIsMem || src.Type == "label" {
			// Need to load source into register first
			if src.Type == "label" {
				ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %s\n", srcReg, valueReg))
			} else if src.Type == "imm" && src.DataType == "float" {
				ce.output.WriteString(fmt.Sprintf("    movl $%d, %s\n", floatBits32(src.Value), "%"+ce.reg64to32(strings.TrimPrefix(valueReg, "%"))))
			} else if src.Type == "imm" {
				// Use helper for float immediates
				loadedStr := ce.loadFloatIfNeeded(src, valueReg)
				if loadedStr != valueReg {
					ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", loadedStr, valueReg))
				}
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcReg, valueReg))
			}
			srcReg = valueReg
		}
		
		// Use appropriate store size based on dst.Size
		if dst.Size == 4 {
			// 32-bit store - convert register to 32-bit version
			srcReg32 := ce.get32BitReg(srcReg)
			ce.output.WriteString(fmt.Sprintf("    movl %s, (%s)\n", srcReg32, ptrReg))
		} else if dst.Size == 2 {
			// 16-bit store
			srcReg16 := ce.get16BitReg(srcReg)
			ce.output.WriteString(fmt.Sprintf("    movw %s, (%s)\n", srcReg16, ptrReg))
		} else if dst.Size == 1 {
			// 8-bit store
			srcReg8 := ce.get8BitReg(srcReg)
			ce.output.WriteString(fmt.Sprintf("    movb %s, (%s)\n", srcReg8, ptrReg))
		} else {
			// 64-bit store (default)
			ce.output.WriteString(fmt.Sprintf("    movq %s, (%s)\n", srcReg, ptrReg))
		}
	default:
		ce.emitMov(dst, src)
	}
}

// Helper to get or create a float literal label
func (ce *CodeEmitter) getFloatLabel(value string) string {
	// Convert integer immediates to float format (exponents are already
	// complete float syntax and must not gain a suffix).
	floatVal := value
	if !strings.Contains(floatVal, ".") && !strings.ContainsAny(floatVal, "eE") {
		floatVal = floatVal + ".0"
	}
	
	// Check if we already have this float value
	for label, val := range ce.floatLits {
		if val == floatVal {
			return label
		}
	}
	
	// Create new label
	ce.floatCounter++
	label := fmt.Sprintf(".FC%d", ce.floatCounter)
	ce.floatLits[label] = floatVal
	return label
}

// float32Label interns a single-precision literal for .float emission.
func (ce *CodeEmitter) float32Label(value string) string {
	for label, val := range ce.float32Lits {
		if val == value {
			return label
		}
	}
	ce.floatCounter++
	label := fmt.Sprintf(".FS%d", ce.floatCounter)
	ce.float32Lits[label] = value
	return label
}

// floatBits32 folds a decimal float literal to its IEEE-754 single bits.
func floatBits32(value string) uint32 {
	f, err := strconv.ParseFloat(value, 32)
	if err != nil {
		return 0
	}
	return math.Float32bits(float32(f))
}

// doubleBits64 folds a numeric literal to IEEE-754 double bits.
func doubleBits64(value string) uint64 {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return math.Float64bits(f)
}

// emitFloatLoadToXmm loads a float/double operand into an XMM register as a
// proper double, converting single-precision (cvtss2sd) when the operand is
// known-float. No silent bit reinterpretation.
func (ce *CodeEmitter) emitFloatLoadToXmm(op *Operand, xmm string) {
	isFloat := op.DataType == "float"
	// Integer operands convert (not bit-move): float h / int f must do
	// 32.0/1300.0, not 32.0/tiny-denormal. Pointers/unknowns keep
	// legacy bitwise behavior (their float use is invalid C anyway).
	if !isFloat && op.DataType != "double" && op.DataType != "" && !strings.Contains(op.DataType, "*") {
		if _, ok := cSizeOf(strings.TrimSpace(op.DataType)); ok {
			ce.emitIntToXmmDoubleTo(op, xmm)
			return
		}
	}
	switch op.Type {
	case "imm":
		if isFloat {
			label := ce.float32Label(op.Value)
			ce.output.WriteString(fmt.Sprintf("    cvtss2sd %s(%%rip), %s\n", label, xmm))
		} else {
			label := ce.getFloatLabel(op.Value)
			ce.output.WriteString(fmt.Sprintf("    movsd %s(%%rip), %s\n", label, xmm))
		}
	case "temp", "reg":
		s := ce.formatOperand(op)
		if strings.Contains(s, "(") && strings.Contains(s, ")") {
			// Spilled temp: memory load.
			if isFloat {
				ce.output.WriteString(fmt.Sprintf("    cvtss2sd %s, %s\n", s, xmm))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movsd %s, %s\n", s, xmm))
			}
		} else {
			// GPR holds the bit pattern.
			if isFloat {
				ce.output.WriteString(fmt.Sprintf("    movd %s, %s\n", ce.formatOperand32(op), xmm))
				ce.output.WriteString(fmt.Sprintf("    cvtss2sd %s, %s\n", xmm, xmm))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", s, xmm))
			}
		}
	default:
		s := ce.formatOperand(op)
		if isFloat {
			ce.output.WriteString(fmt.Sprintf("    cvtss2sd %s, %s\n", s, xmm))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movsd %s, %s\n", s, xmm))
		}
	}
}

// emitFloatStoreFromXmm stores xmm (holding a double) to dst: direct movsd
// for double destinations, cvtsd2ss+movd for known-float ones.
func (ce *CodeEmitter) emitFloatStoreFromXmm(xmm string, dst *Operand) {
	dstStr := ce.formatOperand(dst)
	dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
	if dst.DataType == "float" {
		ce.output.WriteString(fmt.Sprintf("    cvtsd2ss %s, %s\n", xmm, xmm))
		if dst.Type == "temp" || dst.Type == "reg" {
			if dstIsMem {
				ce.output.WriteString(fmt.Sprintf("    movd %s, %%eax\n", xmm))
				ce.output.WriteString(fmt.Sprintf("    movl %%eax, %s\n", dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movd %s, %s\n", xmm, ce.formatOperand32(dst)))
			}
		} else {
			ce.output.WriteString(fmt.Sprintf("    movss %s, %s\n", xmm, dstStr))
		}
		return
	}
	if dst.Type == "temp" || dst.Type == "reg" {
		if dstIsMem {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", xmm))
			ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", xmm, dstStr))
		}
	} else {
		ce.output.WriteString(fmt.Sprintf("    movsd %s, %s\n", xmm, dstStr))
	}
}

func (ce *CodeEmitter) emitSetArg(instr *IRInstruction) {
	// Set up function argument: move src into dst (argument register)
	// dst is the argument register (rdi, rsi, etc. or xmm0, xmm1, etc.)
	// src is the value to pass
	
	dstReg := instr.Dst.Value  // e.g., "rdi", "xmm0"
	src := instr.Src1
	
	// Format the destination register
	dstStr := "%" + dstReg
	
	// Check if destination is an XMM register (float)
	isFloatReg := strings.HasPrefix(dstReg, "xmm")
	
	// Handle different source types
	if isFloatReg {
		// Moving to float register (single-precision promotes to double).
		switch src.Type {
		case "imm":
			if src.DataType == "float" {
				label := ce.float32Label(src.Value)
				ce.output.WriteString(fmt.Sprintf("    cvtss2sd %s(%%rip), %s\n", label, dstStr))
			} else {
				// Double immediate (numerically exact as-is).
				label := ce.getFloatLabel(src.Value)
				ce.output.WriteString(fmt.Sprintf("    movsd %s(%%rip), %s\n", label, dstStr))
			}
		case "temp", "reg", "mem", "var":
			if src.DataType == "float" {
				ce.emitFloatLoadToXmm(src, dstStr)
			} else {
			srcStr := ce.formatOperand(src)
			if strings.Contains(srcStr, "(") && strings.Contains(srcStr, ")") {
				// Source is in memory
				ce.output.WriteString(fmt.Sprintf("    movsd %s, %s\n", srcStr, dstStr))
			} else {
				// Source is in a GPR - use movq to move bitwise
				ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcStr, dstStr))
			}
			}
		default:
			srcStr := ce.formatOperand(src)
			ce.output.WriteString(fmt.Sprintf("    movsd %s, %s\n", srcStr, dstStr))
		}
	} else {
		// Moving to integer register (movabs for wide immediates)
		switch src.Type {
		case "imm":
			if !isFloatSpelling(src.Value) {
				ce.emitImmToReg64(src.Value, dstStr)
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq $%s, %s\n", src.Value, dstStr))
			}
		case "temp", "reg":
			srcStr := ce.formatOperand(src)
			if strings.Contains(srcStr, "(") && strings.Contains(srcStr, ")") {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcStr, dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcStr, dstStr))
			}
		case "mem", "var":
			srcStr := ce.formatOperand(src)
			ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcStr, dstStr))
		case "label":
			ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %s\n", src.Value, dstStr))
		default:
			srcStr := ce.formatOperand(src)
			ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcStr, dstStr))
		}
	}
}

// Helper to load a float immediate into a register if needed
// immFits32 reports whether an integer immediate fits in a signed
// 32-bit instruction immediate (what andq/orq/addq/cmpq accept).
func immFits32(val string) bool {
	v := strings.TrimSpace(val)
	neg := false
	if strings.HasPrefix(v, "-") {
		neg = true
		v = v[1:]
	} else if strings.HasPrefix(v, "+") {
		v = v[1:]
	}
	// Strip integer suffixes.
	v = strings.TrimRight(v, "uUlL")
	u, err := strconv.ParseUint(v, 0, 64)
	if err != nil {
		return true // not parseable here: leave to existing paths
	}
	if neg {
		return u <= 2147483648
	}
	return u <= 2147483647
}

// loadIntImmIfNeeded materializes an oversized integer immediate via
// movabs (e.g. `& 0xFFFFFFFF` from SDL_Swap64: GAS rejects imm32-overflow
// in andq). Returns the operand spelling to use.
func (ce *CodeEmitter) loadIntImmIfNeeded(op *Operand, tempReg string) string {
	if op.Type == "imm" && op.DataType != "float" && op.DataType != "double" && !immFits32(op.Value) {
		ce.output.WriteString(fmt.Sprintf("    movabs $%s, %s\n", op.Value, tempReg))
		return tempReg
	}
	return ce.formatOperand(op)
}

func (ce *CodeEmitter) loadFloatIfNeeded(op *Operand, tempReg string) string {
	// Only treat as float if it's explicitly a float type
	if op.Type == "imm" && (op.DataType == "float" || op.DataType == "double") {
		// Float immediate - load from .rodata
		label := ce.getFloatLabel(op.Value)
		ce.output.WriteString(fmt.Sprintf("    movq %s(%%rip), %s\n", label, tempReg))
		return tempReg
	}
	return ce.formatOperand(op)
}

// emitIntToXmmDoubleTo loads an integer operand to any xmm as a double
// (64-bit longs use the q form; 32-bit and below go through eax).
func (ce *CodeEmitter) emitIntToXmmDoubleTo(op *Operand, xmm string) {
	if s, ok := cSizeOf(strings.TrimSpace(op.DataType)); ok && s == 8 {
		s := ce.formatOperand(op)
		ce.output.WriteString(fmt.Sprintf("    cvtsi2sdq %s, %s\n", s, xmm))
		return
	}
	ce.emitIntToEax(op)
	ce.output.WriteString(fmt.Sprintf("    cvtsi2sd %%eax, %s\n", xmm))
}

// emitIntToXmmDouble loads a 32-bit integer operand to xmm0 as a double.
func (ce *CodeEmitter) emitIntToXmmDouble(src *Operand) {
	ce.emitIntToEax(src)
	ce.output.WriteString("    cvtsi2sd %eax, %xmm0\n")
}

// emitIntToXmmFloat loads a 32-bit integer operand to xmm0. The value is
// left as a double; emitFloatStoreFromXmm narrows for float destinations.
// (Keeping a single double-based invariant avoids mismatched cvt pairs.)
func (ce *CodeEmitter) emitIntToXmmFloat(src *Operand) {
	ce.emitIntToEax(src)
	ce.output.WriteString("    cvtsi2sd %eax, %xmm0\n")
}

// emitIntToEax materializes a 32-bit integer operand in %eax (sign- or
// zero-extended from narrower widths by signedness).
func (ce *CodeEmitter) emitIntToEax(src *Operand) {
	unsigned := isUnsignedPrefix(src.DataType)
	switch src.Type {
	case "imm":
		ce.output.WriteString(fmt.Sprintf("    movl $%s, %%eax\n", src.Value))
	case "temp", "reg":
		s := ce.formatOperand(src)
		if strings.Contains(s, "(") && strings.Contains(s, ")") {
			ce.emitMemToEax(s, 4, unsigned)
		} else {
			ce.output.WriteString(fmt.Sprintf("    movl %s, %%eax\n", ce.formatOperand32(src)))
		}
	default:
		s := ce.formatOperand(src)
		if strings.Contains(s, "(") && strings.Contains(s, ")") {
			w := 4
			if src.Size == 1 || src.Size == 2 {
				w = src.Size
			}
			ce.emitMemToEax(s, w, unsigned)
		} else {
			ce.output.WriteString(fmt.Sprintf("    movl %s, %%eax\n", s))
		}
	}
}

// emitMemToEax loads 1/2/4 bytes from a memory address string into %eax.
func (ce *CodeEmitter) emitMemToEax(mem string, width int, unsigned bool) {
	switch width {
	case 1:
		if unsigned {
			ce.output.WriteString(fmt.Sprintf("    movzbl %s, %%eax\n", mem))
		} else {
		ce.output.WriteString(fmt.Sprintf("    movsbl %s, %%eax\n", mem))
		}
	case 2:
		if unsigned {
			ce.output.WriteString(fmt.Sprintf("    movzwl %s, %%eax\n", mem))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movswl %s, %%eax\n", mem))
		}
	default:
		ce.output.WriteString(fmt.Sprintf("    movl %s, %%eax\n", mem))
	}
}

// emitNarrowToInt extends 8/16-bit to 32-bit with C signedness.
func (ce *CodeEmitter) emitNarrowToInt(src, dst *Operand, mode string) {
	dstStr := ce.formatOperand32(dst)
	signed := mode == "s82i"
	// Immediates fold (movsbl takes no imm operand).
	if src.Type == "imm" {
		if v, err := strconv.ParseInt(src.Value, 0, 64); err == nil {
			if src.Size == 2 || (!signed && false) {
			}
			// Truncate to source width, then extend with C signedness.
			width := src.Size
			if width != 1 && width != 2 {
				if s, ok := cSizeOf(strings.TrimSpace(src.DataType)); ok && (s == 1 || s == 2) {
					width = s
				} else {
					width = 1
				}
			}
			if width == 2 {
				v = int64(int16(v))
				if !signed {
					v = int64(uint16(v))
				}
			} else {
				v = int64(int8(v))
				if !signed {
					v = int64(uint8(v))
				}
			}
			ce.output.WriteString(fmt.Sprintf("    movl $%d, %s\n", v, dstStr))
			return
		}
		ce.output.WriteString(fmt.Sprintf("    movl $%s, %%eax\n", src.Value))
		if signed {
			// eax holds full value; truncate via al/ax then extend.
			ce.output.WriteString(fmt.Sprintf("    movsbl %%al, %s\n", dstStr))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movzbl %%al, %s\n", dstStr))
		}
		return
	}
	// Determine source width: Size field, or DataType.
	width := src.Size
	if width != 1 && width != 2 {
		if s, ok := cSizeOf(strings.TrimSpace(src.DataType)); ok && (s == 1 || s == 2) {
			width = s
		} else {
			width = 1
		}
	}
	srcStr := ce.formatOperand(src)
	if strings.Contains(srcStr, "(") && strings.Contains(srcStr, ")") {
		if width == 1 {
			if signed {
				ce.output.WriteString(fmt.Sprintf("    movsbl %s, %s\n", srcStr, dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movzbl %s, %s\n", srcStr, dstStr))
			}
		} else {
			if signed {
				ce.output.WriteString(fmt.Sprintf("    movswl %s, %s\n", srcStr, dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movzwl %s, %s\n", srcStr, dstStr))
			}
		}
	} else {
		if width == 1 {
			r8 := ce.get8BitReg(srcStr)
			if signed {
				ce.output.WriteString(fmt.Sprintf("    movsbl %s, %s\n", r8, dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movzbl %s, %s\n", r8, dstStr))
			}
		} else {
			r16 := ce.reg64to16(strings.TrimPrefix(srcStr, "%"))
			if signed {
				ce.output.WriteString(fmt.Sprintf("    movswl %%%s, %s\n", r16, dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movzwl %%%s, %s\n", r16, dstStr))
			}
		}
	}
}

// emitNarrowToLong extends 8/16-bit to 64-bit with C signedness
// (sbl/swl sign-extend, zbl/zwl zero-extend). Sources may be regs,
// memory, or immediates.
func (ce *CodeEmitter) emitNarrowToLong(src, dst *Operand, mode string) {
	dstStr := ce.formatOperand(dst)
	signed := mode == "sbl2l" || mode == "swl2l"
	isByte := mode == "sbl2l" || mode == "zbl2l"
	if src.Type == "imm" {
		// Fold the constant at compile time when numeric.
		if v, err := strconv.ParseInt(src.Value, 0, 64); err == nil {
			if isByte {
				v = int64(int8(v))
				if !signed {
					v = int64(uint8(v))
				}
			} else {
				v = int64(int16(v))
				if !signed {
					v = int64(uint16(v))
				}
			}
			ce.output.WriteString(fmt.Sprintf("    movq $%d, %s\n", v, dstStr))
			return
		}
		ce.emitImmToReg64(src.Value, dstStr)
		return
	}
	srcStr := ce.formatOperand(src)
	if strings.Contains(srcStr, "(") && strings.Contains(srcStr, ")") {
		if isByte {
			if signed {
				ce.output.WriteString(fmt.Sprintf("    movsbq %s, %s\n", srcStr, dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movzbq %s, %s\n", srcStr, dstStr))
			}
		} else {
			if signed {
				ce.output.WriteString(fmt.Sprintf("    movswq %s, %s\n", srcStr, dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movzwq %s, %s\n", srcStr, dstStr))
			}
		}
	} else if isByte {
		// Register holds the value with low 8 valid: extend via
		// the 8-bit name (movsbq needs an 8-bit source).
		r8 := ce.get8BitReg(ce.formatOperand(src))
		if signed {
			ce.output.WriteString(fmt.Sprintf("    movsbq %s, %s\n", r8, dstStr))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movzbq %s, %s\n", r8, dstStr))
		}
	} else {
		// Register holds the value with low 16 valid: extend via
		// the 16-bit name (movswq needs a 16-bit source).
		r16 := ce.reg64to16(strings.TrimPrefix(ce.formatOperand(src), "%"))
		if signed {
			ce.output.WriteString(fmt.Sprintf("    movswq %%%s, %s\n", r16, dstStr))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movzwq %%%s, %s\n", r16, dstStr))
		}
	}
}

// emitIntToLong widens a 32-bit int to 64-bit:// emitIntToLong widens a 32-bit int to 64-bit: movslq (signed) or
// movl (unsigned, zero-extends). Handles imm/mem/reg sources.
func (ce *CodeEmitter) emitIntToLong(src, dst *Operand, signed bool) {
	dstStr := ce.formatOperand(dst)
	switch src.Type {
	case "imm":
		if signed {
			// Sign-extend the constant at compile time when numeric.
			if v, err := strconv.ParseInt(src.Value, 0, 32); err == nil {
				ce.output.WriteString(fmt.Sprintf("    movq $%d, %s\n", v, dstStr))
				return
			}
		}
		ce.emitImmToReg64(src.Value, dstStr)
		if signed {
			// movabs zero-extends; fix sign via movslq through eax.
			ce.output.WriteString(fmt.Sprintf("    movslq %s, %s\n", ce.formatOperand32(dst), dstStr))
		}
	default:
		srcStr := ce.formatOperand(src)
		// movl to a register must use the 32-bit name (zero-extends);
		// to memory the size is inferred. movslq always takes full names.
		dst32 := dstStr
		if !strings.Contains(dstStr, "(") {
			dst32 = ce.formatOperand32(dst)
		}
		if strings.Contains(srcStr, "(") && strings.Contains(srcStr, ")") {
			if signed {
				ce.output.WriteString(fmt.Sprintf("    movslq %s, %s\n", srcStr, dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movl %s, %s\n", srcStr, dst32))
			}
		} else {
			if signed {
				ce.output.WriteString(fmt.Sprintf("    movslq %s, %s\n", ce.formatOperand32(src), dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movl %s, %s\n", ce.formatOperand32(src), dst32))
			}
		}
	}
}

// emitCvt emits a numeric conversion (see OpCvt modes). Dst is always a
// fresh temp (register or spilled slot) carrying the target DataType.
func (ce *CodeEmitter) emitCvt(dst, src *Operand, mode string) {
	switch mode {
	case "f2d", "d2f":
		ce.emitFloatLoadToXmm(src, "%xmm0")
		ce.emitFloatStoreFromXmm("%xmm0", dst)
	case "i2d":
		ce.emitIntToXmmDouble(src)
		ce.emitFloatStoreFromXmm("%xmm0", dst)
	case "i2f":
		ce.emitIntToXmmFloat(src)
		ce.emitFloatStoreFromXmm("%xmm0", dst)
	case "i2l":
		// Signed 32->64: sign-extend (int -1 stays -1, not +4B).
		ce.emitIntToLong(src, dst, true)
	case "s82i", "u82i":
		// 8/16->32 (char/short to int): extend with C signedness.
		// Plain char/short are signed; only unsigned zero-extends.
		ce.emitNarrowToInt(src, dst, mode)
	case "sbl2l", "zbl2l", "swl2l", "zwl2l":
		// 8/16-bit to 64-bit (narrowing function returns).
		ce.emitNarrowToLong(src, dst, mode)
	case "u2l":
		// Unsigned 32->64: zero-extend.
		ce.emitIntToLong(src, dst, false)
	case "f2i", "d2i":
		ce.emitFloatLoadToXmm(src, "%xmm0")
		// Loader always leaves a double in xmm0 (float promotes up).
		ce.output.WriteString("    cvttsd2si %xmm0, %eax\n")
		dstStr := ce.formatOperand(dst)
		if strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")") {
			ce.output.WriteString(fmt.Sprintf("    movl %%eax, %s\n", dstStr))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movl %%eax, %s\n", ce.formatOperand32(dst)))
		}
	default:
		ce.emitMov(dst, src)
	}
}

func (ce *CodeEmitter) emitCall(instr *IRInstruction) {
	// Arguments should already be in registers from OpMov instructions
	// Stack alignment should be handled in function prologue, not here
	// (%al for varargs is set by a preceding OpSetAl.)

	// Call: direct by symbol, or indirect through register/memory.
	if instr.Src1.Type == "reg" || instr.Src1.Type == "freg" {
		ce.output.WriteString(fmt.Sprintf("    call *%%%s\n", instr.Src1.Value))
	} else if instr.Src1.Type == "label" {
		ce.output.WriteString(fmt.Sprintf("    call %s\n", instr.Src1.Value))
	} else {
		ce.output.WriteString(fmt.Sprintf("    call *%s\n", ce.formatOperand(instr.Src1)))
	}
	
	// Move result
	if instr.Dst != nil && instr.Dst.Value != "rax" {
		ce.emitMov(instr.Dst, &Operand{Type: "reg", Value: "rax"})
	}
}

// Helper to convert 64-bit register name to 32-bit
func (ce *CodeEmitter) get32BitReg(reg64 string) string {
	// Remove % prefix if present
	reg := strings.TrimPrefix(reg64, "%")
	
	switch reg {
	case "rax": return "%eax"
	case "rbx": return "%ebx"
	case "rcx": return "%ecx"
	case "rdx": return "%edx"
	case "rsi": return "%esi"
	case "rdi": return "%edi"
	case "rbp": return "%ebp"
	case "rsp": return "%esp"
	case "r8": return "%r8d"
	case "r9": return "%r9d"
	case "r10": return "%r10d"
	case "r11": return "%r11d"
	case "r12": return "%r12d"
	case "r13": return "%r13d"
	case "r14": return "%r14d"
	case "r15": return "%r15d"
	default: return reg64  // Return as-is if not recognized
	}
}

// Helper to convert 64-bit register name to 16-bit
func (ce *CodeEmitter) get16BitReg(reg64 string) string {
	reg := strings.TrimPrefix(reg64, "%")
	
	switch reg {
	case "rax": return "%ax"
	case "rbx": return "%bx"
	case "rcx": return "%cx"
	case "rdx": return "%dx"
	case "rsi": return "%si"
	case "rdi": return "%di"
	case "rbp": return "%bp"
	case "rsp": return "%sp"
	case "r8": return "%r8w"
	case "r9": return "%r9w"
	case "r10": return "%r10w"
	case "r11": return "%r11w"
	case "r12": return "%r12w"
	case "r13": return "%r13w"
	case "r14": return "%r14w"
	case "r15": return "%r15w"
	default: return reg64
	}
}

// Helper to convert 64-bit register name to 8-bit
func (ce *CodeEmitter) get8BitReg(reg64 string) string {
	reg := strings.TrimPrefix(reg64, "%")
	
	switch reg {
	case "rax": return "%al"
	case "rbx": return "%bl"
	case "rcx": return "%cl"
	case "rdx": return "%dl"
	case "rsi": return "%sil"
	case "rdi": return "%dil"
	case "rbp": return "%bpl"
	case "rsp": return "%spl"
	case "r8": return "%r8b"
	case "r9": return "%r9b"
	case "r10": return "%r10b"
	case "r11": return "%r11b"
	case "r12": return "%r12b"
	case "r13": return "%r13b"
	case "r14": return "%r14b"
	case "r15": return "%r15b"
	default: return reg64
	}
}

// globalRef renders a global operand with its member offset: name(%rip)
// or name+off(%rip). The old code dropped the offset, so every global
// struct member read/wrote member 0.
func (ce *CodeEmitter) globalRef(op *Operand) string {
	if op.Offset != 0 {
		return fmt.Sprintf("%s+%d(%%rip)", op.Value, op.Offset)
	}
	return op.Value + "(%rip)"
}

func (ce *CodeEmitter) formatOperand(op *Operand) string {
	if op == nil {
		return ""
	}
	
	switch op.Type {
	case "reg":
		return "%" + op.Value
	case "freg":
		return "%" + op.Value
	case "imm":
		// Convert escape sequences to numeric values for assembly
		val := op.Value
		if val == "\\0" {
			val = "0"
		} else if val == "\\n" {
			val = "10"
		} else if val == "\\t" {
			val = "9"
		} else if val == "\\r" {
			val = "13"
		} else if val == "\\\\" {
			val = "92"
		} else if val == "\\'" {
			val = "39"
		} else if val == "\\\"" {
			val = "34"
		}
		return "$" + val
	case "label":
		return op.Value
	case "mem":
		if op.Offset == 0 {
			return "(%rbp)"
		}
		return fmt.Sprintf("%d(%%rbp)", op.Offset)
	case "var":
		if op.IsGlobal {
			return ce.globalRef(op)
		}
		return fmt.Sprintf("%d(%%rbp)", op.Offset)
	case "array":
		// arr[index] where index is in IndexTemp
		// Need to generate: offset(%rbp, %index_reg, 8)
		// For now, simplify by using the computed offset in a temp reg
		if op.IndexTemp != nil {
			indexReg := ce.formatOperand(op.IndexTemp)
			if op.IsGlobal {
				return fmt.Sprintf("%s(%s, %s, 1)", op.Value, "%rip", indexReg)
			}
			return fmt.Sprintf("%d(%%rbp, %s, 1)", op.Offset, indexReg)
		}
		return fmt.Sprintf("%d(%%rbp)", op.Offset)
	case "addr":
		// Address of variable - use lea
		if op.IsGlobal {
			if op.Offset != 0 {
				return fmt.Sprintf("%s+%d(%%rip)", op.Value, op.Offset)
			}
			return op.Value + "(%rip)"
		}
		return fmt.Sprintf("%d(%%rbp)", op.Offset)
	case "sarg":
		// Stack call argument: rsp-relative (valid between StackAlloc/Free).
		if op.Offset == 0 {
			return "(%rsp)"
		}
		return fmt.Sprintf("%d(%%rsp)", op.Offset)
	case "prm":
		// Incoming stack parameter: positive rbp offset (16+8k per SysV).
		return fmt.Sprintf("%d(%%rbp)", op.Offset)
	case "ptr":
		// Dereference - load from address in IndexTemp
		if op.IndexTemp != nil {
			ptrReg := ce.formatOperand(op.IndexTemp)
			return fmt.Sprintf("(%s)", ptrReg)
		}
		return "(%%rax)"
	default:
		return op.Value
	}
}

func (ce *CodeEmitter) buildOutput() string {
	var result strings.Builder
	
	// RO data section
	if ce.rodataSection.Len() > 0 {
		result.WriteString(ce.rodataSection.String())
		result.WriteString("\n")
	}
	
	// Data section
	if ce.dataSection.Len() > 0 {
		result.WriteString("    .data\n")
		result.WriteString(ce.dataSection.String())
		result.WriteString("\n")
	}
	
	// BSS section
	if ce.bssSection.Len() > 0 {
		result.WriteString(ce.bssSection.String())
		result.WriteString("\n")
	}
	
	// Text section
	result.WriteString(ce.output.String())
	
	return result.String()
}

func escapeString(s string) string {
	// String from lexer already has escape sequences like \n, \t
	// Just need to escape quotes and backslashes for assembly
	s = strings.ReplaceAll(s, "\\", "\\\\")  // Escape backslashes first
	s = strings.ReplaceAll(s, "\"", "\\\"")  // Escape quotes
	// Now unescape common sequences so GAS interprets them
	s = strings.ReplaceAll(s, "\\\\n", "\\n")
	s = strings.ReplaceAll(s, "\\\\t", "\\t")
	s = strings.ReplaceAll(s, "\\\\r", "\\r")
	s = strings.ReplaceAll(s, "\\\\0", "\\0")
	return s
}

// EmitMachineCode generates machine code directly using the assembler
func (ce *CodeEmitter) EmitMachineCode() ([]byte, map[string]uint64, error) {
	// Generate assembly text first
	asmText := ce.Emit()
	
	// Create assembler and assemble
	assembler := NewAssembler()
	machineCode, err := assembler.AssembleText(asmText)
	if err != nil {
		return nil, nil, fmt.Errorf("assembly failed: %w", err)
	}
	
	// Return machine code and symbols
	return machineCode, assembler.GetSymbols(), nil
}

// GetSections returns rodata and data sections
func (ce *CodeEmitter) GetSections() (rodata, data []byte, bssSize uint64) {
	// For now, return empty sections
	// TODO: Parse and encode rodata and data sections
	return nil, nil, 0
}
