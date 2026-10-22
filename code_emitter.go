package main

import (
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// line1 emits "    op\n" and line2 emits "    op a, b\n" without
// fmt.Sprintf: Sprintf's parser + interface boxing costs ~200ns/line and
// dominates emission (1.4M lines for the editor TU). The operands must
// already be rendered (via formatOperand).
func (ce *CodeEmitter) line1(op string) {
	ce.output.WriteString("    ")
	ce.output.WriteString(op)
	ce.output.WriteString("\n")
}

func (ce *CodeEmitter) line2(op, a, b string) {
	ce.output.WriteString("    ")
	ce.output.WriteString(op)
	ce.output.WriteString(" ")
	ce.output.WriteString(a)
	ce.output.WriteString(", ")
	ce.output.WriteString(b)
	ce.output.WriteString("\n")
}

// lineA emits "    op a\n" (single-operand instructions: jmp, push, ...).
func (ce *CodeEmitter) lineA(op, a string) {
	ce.output.WriteString("    ")
	ce.output.WriteString(op)
	ce.output.WriteString(" ")
	ce.output.WriteString(a)
	ce.output.WriteString("\n")
}

// frameRef renders "off(%rbp)" / "off(%rsp)" without fmt.Sprintf.
func frameRef(off int, base string) string {
	var buf [32]byte
	b := strconv.AppendInt(buf[:0], int64(off), 10)
	b = append(b, '(')
	b = append(b, base...)
	b = append(b, ')')
	return string(b)
}

// emitImmToReg64 moves an integer immediate into a 64-bit register, using
// movabs when it doesn't fit a sign-extended 32-bit immediate. Plain movq
// with a wide constant either fails to assemble or (via movl) silently
// truncates — both wrong for UL-sized constants.
func (ce *CodeEmitter) emitImmToReg64(value, dstReg string) {
	if v, err := strconv.ParseUint(value, 0, 64); err == nil && v > 0x7FFFFFFF {
		ce.output.WriteString("    movabs $")
		ce.output.WriteString(value)
		ce.output.WriteString(", ")
		ce.output.WriteString(dstReg)
		ce.output.WriteString("\n")
	} else {
		ce.output.WriteString("    movq $")
		ce.output.WriteString(value)
		ce.output.WriteString(", ")
		ce.output.WriteString(dstReg)
		ce.output.WriteString("\n")
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

	// opFmtCache memoizes formatOperand/formatOperand32 results per
	// operand pointer. Operands are never mutated during emission, and the
	// same pointers are formatted repeatedly (each IR operand is read 2-4x
	// across width checks, mem checks and the final write), so this cuts
	// ~2x redundant Sprintf/Contains work out of code emission.
	opFmtCache    map[*Operand]opFmt
	opFmtHits     int
	opFmtMisses   int
	// Leaf-operand value caches: distinct *Operand pointers with identical
	// content (e.g. every "mem -8" in the TU) used to each miss opFmtCache
	// and allocate an identical string (~37% of formats). These share one
	// format per value, keyed as cheaply as possible:
	//   - offCache: frame-relative kinds by (type,width,offset) — no
	//     strings in the key at all (covers mem/var-local/sarg/prm and
	//     offset-only array/addr/ptr forms, the bulk of operands).
	//   - immCache: immediates by spelling (escape mapping is pure).
	//   - globCache: global refs by (name,offset).
	// Labels and default-kind operands need no cache (identity/constant).
	// Recursive operands (array/ptr with IndexTemp, 32-bit ptr with
	// SourcePtr) stay on the pointer path.
	offCache      map[offKey]string
	immCache      map[string]string
	globCache     map[globKey]string

	labelCounter  int
	floatCounter  int

	// floatOrder records float-literal labels (.FCn/.FSn) in creation
	// order. Map iteration is nondeterministic, so the parallel merge
	// replays this order to renumber deterministically.
	floatOrder    []string
}

// opFmt holds memoized 64-bit and 32-bit renderings of one operand.
type opFmt struct {
	a64, a32   string
	ok64, ok32 bool
}

// offKey identifies an offset-only operand format (no strings involved).
type offKey struct {
	typ  uint8 // offTypeCode(op.Type); 0 = not offset-cacheable
	wide bool  // true = 32-bit rendering
	off  int32
}

// globKey identifies a global reference format.
type globKey struct {
	val string
	off int32
}

// offTypeCode maps offset-formatted operand types to key codes.
func offTypeCode(t string) uint8 {
	switch t {
	case "mem":
		return 1
	case "var":
		return 2 // local only; globals go to globCache
	case "array":
		return 3 // IndexTemp == nil only
	case "addr":
		return 4 // local only
	case "sarg":
		return 5
	case "prm":
		return 6
	case "ptr":
		return 7 // IndexTemp/SourcePtr == nil only (64-bit is constant)
	}
	return 0
}

// offCacheable reports whether op formats from its offset alone (both
// widths; the 64/32-bit cold paths agree on these kinds).
func offCacheable(op *Operand) bool {
	switch op.Type {
	case "mem", "sarg", "prm":
		return true
	case "var", "addr":
		return !op.IsGlobal
	case "array":
		return op.IndexTemp == nil
	case "ptr":
		return op.IndexTemp == nil && op.SourcePtr == nil
	}
	return false
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
		opFmtCache:    make(map[*Operand]opFmt, 512),
		offCache:      make(map[offKey]string, 8192),
		immCache:      make(map[string]string, 2048),
		globCache:     make(map[globKey]string, 8192),
	}
}

func (ce *CodeEmitter) Emit() string {
	ce.output.Grow(len(ce.instructions) * 32)
	ce.emitBssSection()
	ce.emitTextSection()
	// Emit data section last, after we've discovered all float literals
	ce.emitDataSection()

	return ce.buildOutput()
}

// OpFmtStats reports operand-format cache effectiveness (verbose diagnostics).
func (ce *CodeEmitter) OpFmtStats() (hits, misses int) {
	return ce.opFmtHits, ce.opFmtMisses
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

	// For string (char*) globals, emit the string in .data with its own
	// label, then store the address. Keeping both in .data avoids
	// cross-section local-label mis-resolution by the linker.
	align := sym.Size
	if align > 8 {
		align = 8
	}
	if align < 1 {
		align = 1
	}
	ce.dataSection.WriteString(fmt.Sprintf("    .align %d\n", align))
	ce.dataSection.WriteString(fmt.Sprintf("%s:\n", name))
	if kind == "string" {
		str := val
		if s, ok := ce.stringLits[val]; ok {
			str = s
		}
		// Emit the string immediately after the pointer slot, and store
		// a PC-relative offset (8) so the value is resolved at assembly
		// time (no cross-section relocation, which mis-resolves in large
		// files). The variable's address+8 points at the string.
		ce.dataSection.WriteString(fmt.Sprintf("    .quad . + 8\n"))
		ce.dataSection.WriteString(fmt.Sprintf("    .string \"%s\"\n", escapeString(str)))
		return true
	}
	switch kind {
	case "double":
		// Double literal into an integer global truncates (C init
		// conversion); mirrors emitDataMemberValue.
		if fl, db, _ := cvtClass(sym.Type); !fl && !db {
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				val = fmt.Sprintf("%d", int64(f))
			}
			kind = "int"
		} else {
			if sym.Size == 4 {
				ce.dataSection.WriteString(fmt.Sprintf("    .float %s\n", val))
			} else {
				ce.dataSection.WriteString(fmt.Sprintf("    .double %s\n", val))
			}
			break
		}
		fallthrough
	case "int":
		// C init conversion: an int literal into a float/double global
		// is the converted value (static double g = -20 is -20.0, not
		// int bits, which read back as -nan). Mirrors emitDataMemberValue.
		if fl, db, _ := cvtClass(sym.Type); fl || db {
			val = dataFloatSpelling(val)
			if sym.Size == 4 {
				ce.dataSection.WriteString(fmt.Sprintf("    .float %s\n", val))
			} else {
				ce.dataSection.WriteString(fmt.Sprintf("    .double %s\n", val))
			}
			return true
		}
		fallthrough
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
// dataFloatSpelling renders an integer-literal spelling as a float
// literal GAS accepts in .float/.double (C init conversion: -20 becomes
// -20.0). Falls back to appending ".0" when the spelling isn't a plain
// integer (matches the historical emitDataMemberValue behavior).
func dataFloatSpelling(v string) string {
	if n, err := strconv.ParseInt(strings.TrimSpace(v), 0, 64); err == nil {
		return strconv.FormatFloat(float64(n), 'f', -1, 64)
	}
	return v + ".0"
}

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
		// C init conversions between int and float element kinds (an
		// int literal into a double array is the converted value, not
		// int bits); mirrors emitDataGlobal/emitDataMemberValue.
		elemFloat := false
		if fl, db, _ := cvtClass(sym.Type); fl || db {
			elemFloat = true
		}
		if kind == "int" && elemFloat {
			val = dataFloatSpelling(val)
			kind = "double"
		} else if kind == "double" && !elemFloat && elemSize <= 8 {
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				val = fmt.Sprintf("%d", int64(f))
			}
			kind = "int"
		}
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
		// Use a non-local (global) label: a leading-dot label referenced
		// across sections (.data -> .rodata) was mis-resolved by the linker
		// for large strings, leaving the pointer (e.g. ahoy_lit_vs) 0.
		label := fmt.Sprintf("str_g%d", len(ce.stringLits))
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
				ce.emitFunction(instr.Dst.Value, &i, -1)
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

// cloneForEmit returns a worker emitter that shares the read-only tables
// (instructions backing store, stringLits, globalVars, structDefs,
// localFuncs, usedRegisters) with its parent but owns all mutable emission
// state (output Builder, float-literal tables + counter + order, operand
// format cache, currentFunc/stackSize). stringLits is only read during
// text emission (writes happen in the sequential BSS/data phases), so
// concurrent worker reads need no locks.
func (ce *CodeEmitter) cloneForEmit(instrs []*IRInstruction) *CodeEmitter {
	clone := &CodeEmitter{
		instructions:  instrs,
		stringLits:    ce.stringLits,
		globalVars:    ce.globalVars,
		structDefs:    ce.structDefs,
		localFuncs:    ce.localFuncs,
		floatLits:     make(map[string]string),
		float32Lits:   make(map[string]string),
		// Caches grow lazily: a worker handles ONE function whose unique
		// operand set is tiny; presizing here zeroed ~24KB of buckets per
		// function (~300MB on 12K-function TUs).
		opFmtCache:    make(map[*Operand]opFmt),
		offCache:      make(map[offKey]string),
		immCache:      make(map[string]string),
		globCache:     make(map[globKey]string),
		usedRegisters: ce.usedRegisters,
	}
	// Pre-size the segment text (~32 bytes/IR measured): Builder growth by
	// doubling re-copied ~2x the final bytes on large TUs.
	clone.output.Grow(len(instrs) * 32)
	return clone
}

// emitSegment describes one function's IR slice in AST order. frameMax
// is the deepest negative frame offset in the body, computed during the
// partition walk so per-function emission doesn't re-scan for it.
type emitSegment struct {
	name     string
	instrs   []*IRInstruction
	frameMax int
}

// frameMaxOf returns the deepest negative frame offset among mem/var
// operands in instrs (loop body shared by the segment walk and the
// sequential fallback).
func frameMaxOf(instrs []*IRInstruction) int {
	maxOffset := 0
	for _, instr := range instrs {
		if op := instr.Dst; op != nil && (op.Type == "mem" || op.Type == "var") && op.Offset < 0 {
			if offset := -op.Offset; offset > maxOffset {
				maxOffset = offset
			}
		}
		if op := instr.Src1; op != nil && (op.Type == "mem" || op.Type == "var") && op.Offset < 0 {
			if offset := -op.Offset; offset > maxOffset {
				maxOffset = offset
			}
		}
		if op := instr.Src2; op != nil && (op.Type == "mem" || op.Type == "var") && op.Offset < 0 {
			if offset := -op.Offset; offset > maxOffset {
				maxOffset = offset
			}
		}
	}
	return maxOffset
}

// splitEmitSegments partitions the merged IR the way emitTextSection walks
// it: leading non-function instructions (normally none; the sequential loop
// only emits bare labels there and drops anything else) plus one segment
// per function-start OpLabel through the next function-start OpLabel.
func (ce *CodeEmitter) splitEmitSegments() (preamble []*IRInstruction, segs []emitSegment) {
	i := 0
	for i < len(ce.instructions) {
		instr := ce.instructions[i]
		if instr.Op == OpLabel && ce.isFunctionLabel(instr.Dst.Value) {
			j := i + 1
			for j < len(ce.instructions) {
				nx := ce.instructions[j]
				if nx.Op == OpLabel && ce.isFunctionLabel(nx.Dst.Value) {
					break
				}
				j++
			}
			segs = append(segs, emitSegment{instr.Dst.Value, ce.instructions[i:j], frameMaxOf(ce.instructions[i+1 : j])})
			i = j
		} else {
			preamble = append(preamble, instr)
			i++
		}
	}
	return preamble, segs
}

// emitResult is one worker's output: text plus its local float tables in
// creation order (map iteration would be nondeterministic).
type emitResult struct {
	text        string
	floatLits   map[string]string
	float32Lits map[string]string
	floatOrder  []string
}

// rewriteFloatRefs renames local .FCn/.FSn references in one function's
// text to their merged global labels. Tokens are matched as ".FC"/".FS"
// plus the longest digit run, so .FC1 never partially matches .FC12.
func rewriteFloatRefs(text string, remap map[string]string) string {
	if len(remap) == 0 {
		return text
	}
	var sb strings.Builder
	sb.Grow(len(text))
	i := 0
	for i < len(text) {
		if text[i] == '.' && i+2 < len(text) && text[i+1] == 'F' &&
			(text[i+2] == 'C' || text[i+2] == 'S') {
			j := i + 3
			for j < len(text) && text[j] >= '0' && text[j] <= '9' {
				j++
			}
			if j > i+3 {
				if g, ok := remap[text[i:j]]; ok {
					sb.WriteString(g)
					i = j
					continue
				}
			}
		}
		sb.WriteByte(text[i])
		i++
	}
	return sb.String()
}

// emitTextSectionParallel emits each function with a worker pool
// (runtime.NumCPU) and merges the results in AST order.
//
// A sequential head (BSS/data globals, which can still intern strings)
// must already have run; a sequential tail (emitDataSection) runs after,
// once the merged float tables are complete. Workers share read-only
// tables and own their text + float state; the merge dedups equal float
// values in function order with deterministic renumbering (one shared
// counter for .FC/.FS, mirroring the sequential path) and rewrites each
// segment's references before concatenation.
func (ce *CodeEmitter) emitTextSectionParallel() {
	ce.output.WriteString("    .text\n")

	preamble, segs := ce.splitEmitSegments()

	// Preamble: same semantics as the sequential loop (bare labels only).
	for _, instr := range preamble {
		if instr.Op == OpLabel {
			ce.emitLabel(instr.Dst.Value)
		}
	}

	results := make([]emitResult, len(segs))
	if len(segs) > 0 {
		workers := runtime.NumCPU()
		if workers < 1 {
			workers = 1
		}
		if workers > len(segs) {
			workers = len(segs)
		}
		jobs := make(chan int, len(segs))
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobs {
					clone := ce.cloneForEmit(segs[j].instrs)
					idx := 0
					clone.emitFunction(segs[j].name, &idx, segs[j].frameMax)
					results[j] = emitResult{
						text:        clone.output.String(),
						floatLits:   clone.floatLits,
						float32Lits: clone.float32Lits,
						floatOrder:  clone.floatOrder,
					}
				}
			}()
		}
		for j := range segs {
			jobs <- j
		}
		close(jobs)
		wg.Wait()
	}

	// Merge in AST order with deterministic renumbering. Values seen in
	// an earlier function keep the first label; later duplicates remap.
	// Pre-size the merged text exactly (segments are complete): avoids
	// Builder doubling copies on large TUs.
	total := 0
	for _, r := range results {
		total += len(r.text)
	}
	ce.output.Grow(total)
	valToFC := make(map[string]string)
	valToFS := make(map[string]string)
	mergedFC := make(map[string]string)
	mergedFS := make(map[string]string)
	counter := 0
	for _, r := range results {
		remap := make(map[string]string, len(r.floatOrder))
		for _, local := range r.floatOrder {
			if val, ok := r.floatLits[local]; ok {
				g, ok := valToFC[val]
				if !ok {
					counter++
					g = fmt.Sprintf(".FC%d", counter)
					valToFC[val] = g
					mergedFC[g] = val
				}
				remap[local] = g
			} else if val, ok := r.float32Lits[local]; ok {
				g, ok := valToFS[val]
				if !ok {
					counter++
					g = fmt.Sprintf(".FS%d", counter)
					valToFS[val] = g
					mergedFS[g] = val
				}
				remap[local] = g
			}
		}
		ce.output.WriteString(rewriteFloatRefs(r.text, remap))
	}
	ce.floatLits = mergedFC
	ce.float32Lits = mergedFS
	ce.floatCounter = counter
}

// EmitParallel is the parallel counterpart of Emit: identical section
// layout, with function bodies emitted concurrently. The BSS/data-global
// phase stays sequential (it can intern strings), the text phase runs on
// a worker pool, and the data/rodata phase runs sequentially on the
// merged float tables.
func (ce *CodeEmitter) EmitParallel() string {
	ce.emitBssSection()
	ce.emitTextSectionParallel()
	// Emit data section last, after we've discovered all float literals
	ce.emitDataSection()

	return ce.buildOutput()
}

func (ce *CodeEmitter) emitFunction(name string, startIdx *int, frameMax int) {
	ce.currentFunc = name
	
	// Emit function header (local linkage for static/inline-only defs so
	// headers can't multiply-define them at link time).
	if !ce.localFuncs[name] {
		ce.output.WriteString("\n    .globl ")
		ce.output.WriteString(name)
		ce.output.WriteString("\n")
	}
	ce.output.WriteString("    .type ")
	ce.output.WriteString(name)
	ce.output.WriteString(", @function\n")
	ce.output.WriteString(name)
	ce.output.WriteString(":\n")
	
	// Prologue
	ce.output.WriteString("    pushq %rbp\n")
	ce.output.WriteString("    movq %rsp, %rbp\n")

	// Save callee-saved registers FIRST so the frame math accounts for
	// them: pushes change rsp, and the sub below must keep 16-alignment
	// (a push without padding misaligned every call -> printf crashed).
	savedRegs := ce.savedRegList()
	for _, reg := range savedRegs {
		ce.output.WriteString("    pushq %")
		ce.output.WriteString(reg)
		ce.output.WriteString("\n")
	}

	// Stack size from the precomputed frame max (parallel path) or a
	// fallback scan (sequential path). Include push bytes so (pushes + sub)
	// stays 16-aligned.
	if frameMax < 0 {
		frameMax = ce.calculateStackSize(*startIdx + 1)
	}
	ce.stackSize = frameMax
	pushBytes := len(savedRegs) * 8
	total := ce.stackSize + pushBytes
	// Align to 16 bytes
	total = (total + 15) & ^15
	ce.stackSize = total - pushBytes
	if ce.stackSize > 0 {
		ce.output.WriteString("    subq $")
		ce.output.WriteString(strconv.Itoa(ce.stackSize))
		ce.output.WriteString(", %rsp\n")
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
	
	ce.output.WriteString("    .size ")
	ce.output.WriteString(name)
	ce.output.WriteString(", .-")
	ce.output.WriteString(name)
	ce.output.WriteString("\n")
}

func (ce *CodeEmitter) calculateStackSize(startIdx int) int {
	maxOffset := 0

	for i := startIdx; i < len(ce.instructions); i++ {
		instr := ce.instructions[i]

		if instr.Op == OpLabel && ce.isFunctionLabel(instr.Dst.Value) {
			break
		}

		// Unrolled over Dst/Src1/Src2 (no slice literal: one alloc saved
		// per instruction scanned).
		if op := instr.Dst; op != nil && (op.Type == "mem" || op.Type == "var") && op.Offset < 0 {
			if offset := -op.Offset; offset > maxOffset {
				maxOffset = offset
			}
		}
		if op := instr.Src1; op != nil && (op.Type == "mem" || op.Type == "var") && op.Offset < 0 {
			if offset := -op.Offset; offset > maxOffset {
				maxOffset = offset
			}
		}
		if op := instr.Src2; op != nil && (op.Type == "mem" || op.Type == "var") && op.Offset < 0 {
			if offset := -op.Offset; offset > maxOffset {
				maxOffset = offset
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
				ce.output.WriteString("    pushq %")
				ce.output.WriteString(regNames[reg])
				ce.output.WriteString("\n")
			}
		}
	}
}

func (ce *CodeEmitter) emitRegisterRestores() {
	calleeSaved := []int{R15, R14, R13, R12, RBX}
	
	for _, reg := range calleeSaved {
		for _, usedReg := range ce.usedRegisters {
			if reg == usedReg {
				ce.output.WriteString("    popq %")
				ce.output.WriteString(regNames[reg])
				ce.output.WriteString("\n")
			}
		}
	}
}

func (ce *CodeEmitter) emitReturn() {
	// Pops must read the push area (just below rbp), not wherever rsp
	// wandered (locals/calls). Reset first, then pop, then tear down.
	n := len(ce.savedRegList())
	if n > 0 {
		ce.output.WriteString("    leaq -")
		ce.output.WriteString(strconv.Itoa(n*8))
		ce.output.WriteString("(%rbp), %rsp\n")
	}
	ce.emitRegisterRestores()
	ce.output.WriteString("    movq %rbp, %rsp\n")
	ce.output.WriteString("    popq %rbp\n")
	ce.output.WriteString("    ret\n")
}

func (ce *CodeEmitter) emitLabel(label string) {
	ce.output.WriteString(label)
	ce.output.WriteString(":\n")
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
		ce.lineA("negq", ce.formatOperand(instr.Dst))
		
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
			ce.line2("movq", dstStr, "%rax")
			ce.output.WriteString("    testq %rax, %rax\n")
			ce.output.WriteString("    sete %al\n")
			ce.output.WriteString("    movzbq %al, %rax\n")
			ce.line2("movq", "%rax", dstStr)
		} else {
			ce.line2("testq", dstStr, dstStr)
			ce.output.WriteString("    sete %al\n")
			ce.output.WriteString("    movzbq %al, ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
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
				ce.output.WriteString("    leaq ")
				ce.output.WriteString(src1.Value)
				ce.output.WriteString("(%rip), ")
				ce.output.WriteString(leaDst)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    leaq ")
				ce.output.WriteString(strconv.Itoa(src1.Offset))
				ce.output.WriteString("(%rbp), ")
				ce.output.WriteString(leaDst)
				ce.output.WriteString("\n")
			}
		} else if src1.Type == "mem" {
			ce.output.WriteString("    leaq ")
			ce.output.WriteString(strconv.Itoa(src1.Offset))
			ce.output.WriteString("(%rbp), ")
			ce.output.WriteString(leaDst)
			ce.output.WriteString("\n")
		} else if src1.Type == "temp" {
			// Temp might have been allocated on stack - check allocator mapping
			// For now, try to use formatOperand but strip the dereference
			srcStr := ce.formatOperand(src1)
			// If srcStr is like offset(%rbp), use leaq with it
			if strings.Contains(srcStr, "(%rbp)") {
				ce.output.WriteString("    leaq ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(leaDst)
				ce.output.WriteString("\n")
			} else {
				// Shouldn't happen - fallback to moving the value
				ce.output.WriteString("    movq ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(leaDst)
				ce.output.WriteString("\n")
			}
		} else {
			// Fallback
			ce.output.WriteString("    leaq ")
			ce.output.WriteString(ce.formatOperand(src1))
			ce.output.WriteString(", ")
			ce.output.WriteString(leaDst)
			ce.output.WriteString("\n")
		}
		if dstIsMem {
			ce.line2("movq", "%rax", dstStr)
		}
		
	case OpStore:
		ce.emitStore(instr.Dst, instr.Src1)
		
	case OpCall:
		ce.emitCall(instr)
		
	case OpJmp:
		ce.lineA("jmp", instr.Dst.Value)
		
	case OpJz:
		src1Str := ce.formatOperand(instr.Src1)
		if instr.Src1.Type == "imm" {
			// test doesn't take two immediates: materialize first.
			ce.line2("movq", src1Str, "%rax")
			ce.output.WriteString("    testq %rax, %rax\n")
		} else if strings.Contains(src1Str, "(") && strings.Contains(src1Str, ")") {
			ce.line2("movq", src1Str, "%rax")
			ce.output.WriteString("    testq %rax, %rax\n")
		} else {
			ce.line2("testq", src1Str, src1Str)
		}
		ce.lineA("jz", instr.Dst.Value)
		
	case OpJnz:
		src1Str := ce.formatOperand(instr.Src1)
		if instr.Src1.Type == "imm" {
			ce.line2("movq", src1Str, "%rax")
			ce.output.WriteString("    testq %rax, %rax\n")
		} else if strings.Contains(src1Str, "(") && strings.Contains(src1Str, ")") {
			ce.line2("movq", src1Str, "%rax")
			ce.output.WriteString("    testq %rax, %rax\n")
		} else {
			ce.line2("testq", src1Str, src1Str)
		}
		ce.lineA("jnz", instr.Dst.Value)
		
	case OpLabel:
		ce.emitLabel(instr.Dst.Value)
		
	case OpParam:
		// Parameters handled in OpCall
		
	case OpPush:
		ce.lineA("pushq", ce.formatOperand(instr.Src1))
		
	case OpPop:
		ce.lineA("popq", ce.formatOperand(instr.Dst))
		
	case OpSetArg:
		// Special handling for setting up function arguments
		// This bypasses the register allocator to avoid conflicts
		ce.emitSetArg(instr)

	case OpStackAlloc:
		ce.line2("subq", "$"+instr.Dst.Value, "%rsp")

	case OpStackFree:
		ce.line2("addq", "$"+instr.Dst.Value, "%rsp")

	case OpSetAl:
		ce.line2("mov", "$"+instr.Dst.Value, "%eax")

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
		ce.output.WriteString("    movl (%rdx, %")
		ce.output.WriteString(idx)
		ce.output.WriteString(", 1), ")
		ce.output.WriteString(dstStr32)
		ce.output.WriteString("\n")
	case 2:
		dstStr32 := ce.formatOperand32(dst)
		ce.output.WriteString("    movzwl (%rdx, %")
		ce.output.WriteString(idx)
		ce.output.WriteString(", 1), ")
		ce.output.WriteString(dstStr32)
		ce.output.WriteString("\n")
	case 1:
		dstStr32 := ce.formatOperand32(dst)
		ce.output.WriteString("    movzbl (%rdx, %")
		ce.output.WriteString(idx)
		ce.output.WriteString(", 1), ")
		ce.output.WriteString(dstStr32)
		ce.output.WriteString("\n")
	default:
		ce.output.WriteString("    movq (%rdx, %")
		ce.output.WriteString(idx)
		ce.output.WriteString(", 1), ")
		ce.output.WriteString(dstReg)
		ce.output.WriteString("\n")
	}
}

// emitArrayStoreToBase emits the final indexed store; value is in %rax.
func (ce *CodeEmitter) emitArrayStoreToBase(size int, indexReg string) {
	idx := strings.TrimPrefix(indexReg, "%")
	switch size {
	case 4:
		ce.output.WriteString("    movl %eax, (%rdx, %")
		ce.output.WriteString(idx)
		ce.output.WriteString(", 1)\n")
	case 2:
		ce.output.WriteString("    movw %ax, (%rdx, %")
		ce.output.WriteString(idx)
		ce.output.WriteString(", 1)\n")
	case 1:
		ce.output.WriteString("    movb %al, (%rdx, %")
		ce.output.WriteString(idx)
		ce.output.WriteString(", 1)\n")
	default:
		ce.output.WriteString("    movq %rax, (%rdx, %")
		ce.output.WriteString(idx)
		ce.output.WriteString(", 1)\n")
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
	ce.lineA(cc, "%al")
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
		ce.line2("movq", "%rax", dstStr)
	} else {
		ce.output.WriteString("    movzbq %al, ")
		ce.output.WriteString(dstStr)
		ce.output.WriteString("\n")
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
			ce.output.WriteString("    movsd ")
			ce.output.WriteString(label)
			ce.output.WriteString("(%rip), ")
			ce.output.WriteString(xmm)
			ce.output.WriteString("\n")
		} else if strings.Contains(s, "(") && strings.Contains(s, ")") {
			ce.output.WriteString("    movsd ")
			ce.output.WriteString(s)
			ce.output.WriteString(", ")
			ce.output.WriteString(xmm)
			ce.output.WriteString("\n")
		} else {
			ce.output.WriteString("    movq ")
			ce.output.WriteString(s)
			ce.output.WriteString(", ")
			ce.output.WriteString(xmm)
			ce.output.WriteString("\n")
		}
		return
	}
	if op.Type == "imm" && isFloatSpelling(op.Value) {
		label := ce.getFloatLabel(op.Value)
		ce.output.WriteString("    movsd ")
		ce.output.WriteString(label)
		ce.output.WriteString("(%rip), ")
		ce.output.WriteString(xmm)
		ce.output.WriteString("\n")
		return
	}
	// Integer to double (signed). 64-bit longs use the q form.
	// Immediates materialize through eax first (cvtsi2sd takes no imm).
	if op.Type == "imm" {
		ce.output.WriteString("    movq $")
		ce.output.WriteString(op.Value)
		ce.output.WriteString(", %rax\n")
		if op.DataType == "long" || op.DataType == "unsigned long" || op.DataType == "long long" {
			ce.output.WriteString("    cvtsi2sdq %rax, ")
			ce.output.WriteString(xmm)
			ce.output.WriteString("\n")
		} else {
			ce.output.WriteString("    cvtsi2sd %eax, ")
			ce.output.WriteString(xmm)
			ce.output.WriteString("\n")
		}
		return
	}
	is64 := op.DataType == "long" || op.DataType == "unsigned long" || op.DataType == "long long"
	s := ce.formatOperand(op)
	if strings.Contains(s, "(") && strings.Contains(s, ")") {
		if is64 {
			ce.output.WriteString("    cvtsi2sdq ")
			ce.output.WriteString(s)
			ce.output.WriteString(", ")
			ce.output.WriteString(xmm)
			ce.output.WriteString("\n")
		} else {
			ce.output.WriteString("    cvtsi2sdl ")
			ce.output.WriteString(s)
			ce.output.WriteString(", ")
			ce.output.WriteString(xmm)
			ce.output.WriteString("\n")
		}
	} else if is64 {
		ce.output.WriteString("    cvtsi2sdq ")
		ce.output.WriteString(s)
		ce.output.WriteString(", ")
		ce.output.WriteString(xmm)
		ce.output.WriteString("\n")
	} else {
		ce.output.WriteString("    cvtsi2sd ")
		ce.output.WriteString(ce.formatOperand32(op))
		ce.output.WriteString(", ")
		ce.output.WriteString(xmm)
		ce.output.WriteString("\n")
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
			ce.output.WriteString("    movsd ")
			ce.output.WriteString(srcStr)
			ce.output.WriteString(", ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		} else {
			ce.line2("movq", srcStr, dstStr)
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
				ce.output.WriteString("    movl $")
				ce.output.WriteString(strconv.FormatUint(uint64(bits), 10))
				ce.output.WriteString(", %eax\n")
				ce.output.WriteString("    movl %eax, ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movl $")
				ce.output.WriteString(strconv.FormatUint(uint64(bits), 10))
				ce.output.WriteString(", ")
				ce.output.WriteString(ce.formatOperand32(dst))
				ce.output.WriteString("\n")
			}
			return
		}
		// It's a double literal - store in .rodata and load address
		label, exists := ce.floatLits[src.Value]
		if !exists {
			ce.floatCounter++
			label = fmt.Sprintf(".FC%d", ce.floatCounter)
			ce.floatLits[label] = src.Value
			ce.floatOrder = append(ce.floatOrder, label)
		}
		// Load the float constant as a 64-bit integer from .rodata
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		if dstIsMem {
			ce.output.WriteString("    movq ")
			ce.output.WriteString(label)
			ce.output.WriteString("(%rip), %rax\n")
			ce.line2("movq", "%rax", dstStr)
		} else {
			ce.output.WriteString("    movq ")
			ce.output.WriteString(label)
			ce.output.WriteString("(%rip), ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		}
		return
	}
	
	// Handle label (string literals, addresses) - use leaq
	if src.Type == "label" {
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		if dstIsMem {
			ce.output.WriteString("    leaq ")
			ce.output.WriteString(ce.globalRef(src))
			ce.output.WriteString(", %rax\n")
			ce.line2("movq", "%rax", dstStr)
		} else {
			ce.output.WriteString("    leaq ")
			ce.output.WriteString(src.Value)
			ce.output.WriteString("(%rip), ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
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
			ce.output.WriteString("    movl ")
			ce.output.WriteString(srcStr32)
			ce.output.WriteString(", %eax\n")
			ce.output.WriteString("    movl %eax, ")
			ce.output.WriteString(dstStr32)
			ce.output.WriteString("\n")
		} else {
			// Use movslq for sign-extending 32-bit to 64-bit when loading to register
			if !dstIsMem && srcIsMem {
				ce.output.WriteString("    movslq ")
				ce.output.WriteString(srcStr32)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movl ")
				ce.output.WriteString(srcStr32)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr32)
				ce.output.WriteString("\n")
			}
		}
		return
	}
	
	// Handle immediate to memory (movabs for wide constants)
	if dst.Type == "mem" && src.Type == "imm" {
		if !isFloatSpelling(src.Value) {
			ce.emitImmToReg64(src.Value, "%rax")
		} else {
			ce.line2("movq", srcStr, "%rax")
		}
		ce.line2("movq", "%rax", dstStr)
		return
	}
	
	// Check for memory-to-memory by pattern (both have (%reg) syntax)
	srcIsMem := strings.Contains(srcStr, "(") && strings.Contains(srcStr, ")")
	dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
	
	if srcIsMem && dstIsMem {
		// Memory to memory through register
		ce.line2("movq", srcStr, "%rax")
		ce.line2("movq", "%rax", dstStr)
		return
	}

	// Wide integer immediates need movabs (plain movq can't encode them).
	if src.Type == "imm" && !isFloatSpelling(src.Value) {
		if dstIsMem {
			ce.emitImmToReg64(src.Value, "%rax")
			ce.line2("movq", "%rax", dstStr)
		} else {
			ce.emitImmToReg64(src.Value, dstStr)
		}
		return
	}

	ce.line2("movq", srcStr, dstStr)
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
			ce.output.WriteString("    movsd ")
			ce.output.WriteString(label)
			ce.output.WriteString("(%rip), %")
			ce.output.WriteString(dst.Value)
			ce.output.WriteString("\n")
		} else {
			// Load to temp XMM register first, then move to destination
			ce.output.WriteString("    movsd ")
			ce.output.WriteString(label)
			ce.output.WriteString("(%rip), %xmm0\n")
			ce.output.WriteString("    movsd %xmm0, ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		}
		return
	}
	
	// Source is in memory or temp register, destination is XMM register
	if dst.Type == "freg" {
		if src.Type == "temp" || src.Type == "reg" {
			// Move from GPR to XMM (use movq for bit pattern transfer)
			ce.output.WriteString("    movq ")
			ce.output.WriteString(srcStr)
			ce.output.WriteString(", %")
			ce.output.WriteString(dst.Value)
			ce.output.WriteString("\n")
		} else {
			// Load from memory to XMM
			ce.output.WriteString("    movsd ")
			ce.output.WriteString(srcStr)
			ce.output.WriteString(", %")
			ce.output.WriteString(dst.Value)
			ce.output.WriteString("\n")
		}
		return
	}
	
	// If not a float register destination, fall back to regular mov
	ce.emitMov(dst, src)
}
// normFloatDT strips storage qualifiers for float detection ("static
// float" etc.). Fast paths first: plain spellings (the common case) skip
// the qualifier loop, and single words can't hold "qualifier + type".
func normFloatDT(t string) string {
	t = strings.TrimSpace(t)
	if t == "float" || t == "double" {
		return t
	}
	if strings.IndexByte(t, ' ') < 0 {
		return t
	}
	for _, q := range []string{"static ", "inline ", "extern ", "const ", "volatile ", "register "} {
		for strings.HasPrefix(t, q) {
			t = strings.TrimSpace(t[len(q):])
		}
	}
	return t
}

func (ce *CodeEmitter) emitBinaryOp(op string, dst, src1, src2 *Operand) {
	// Check if this is a float operation
	isFloat := (normFloatDT(dst.DataType) == "float" || normFloatDT(dst.DataType) == "double" ||
	            normFloatDT(src1.DataType) == "float" || normFloatDT(src1.DataType) == "double" ||
	            normFloatDT(src2.DataType) == "float" || normFloatDT(src2.DataType) == "double")
	
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
			ce.output.WriteString("    ")
			ce.output.WriteString(floatOp)
			ce.output.WriteString(" %xmm1, %xmm0\n")

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
		ce.line2("movq", src2Str, "%rax")
		ce.line2(op, "%rax", dstStr)
	} else {
		ce.line2(op, src2Str, dstStr)
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
		ce.line2("movq", dstStr, "%rax")
		ce.output.WriteString("    imulq ")
		ce.output.WriteString(src2Str)
		ce.output.WriteString(", %rax\n")
		ce.line2("movq", "%rax", dstStr)
	} else {
		ce.output.WriteString("    imulq ")
		ce.output.WriteString(src2Str)
		ce.output.WriteString(", ")
		ce.output.WriteString(dstStr)
		ce.output.WriteString("\n")
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
		ce.output.WriteString("    movl ")
		ce.output.WriteString(ce.formatOperand32(src1))
		ce.output.WriteString(", %eax\n")
		ce.output.WriteString("    " + extOp + "\n") // sign- or zero-extend EAX to EDX:EAX

		if src2.Type == "imm" {
			ce.output.WriteString("    movl $")
			ce.output.WriteString(src2.Value)
			ce.output.WriteString(", %r11d\n")
			ce.output.WriteString("    ")
			ce.output.WriteString(divOp)
			ce.output.WriteString(" %r11d\n")
		} else {
			ce.output.WriteString("    ")
			ce.output.WriteString(divOp)
			ce.output.WriteString(" ")
			ce.output.WriteString(ce.formatOperand32(src2))
			ce.output.WriteString("\n")
		}

		ce.output.WriteString("    movl %eax, ")
		ce.output.WriteString(ce.formatOperand32(dst))
		ce.output.WriteString("\n")
	} else {
		// 64-bit division (original code)
		ce.line2("movq", ce.formatOperand(src1), "%rax")
		ce.output.WriteString("    ")
		ce.output.WriteString(extOp64)
		ce.output.WriteString("\n")

		if src2.Type == "imm" {
			src2Str := ce.loadFloatIfNeeded(src2, "%r11")
			if src2Str == "%r11" {
				ce.output.WriteString("    ")
				ce.output.WriteString(divOp64)
				ce.output.WriteString(" %r11\n")
			} else {
				ce.output.WriteString("    movq ")
				ce.output.WriteString(src2Str)
				ce.output.WriteString(", %r11\n")
				ce.output.WriteString("    ")
				ce.output.WriteString(divOp64)
				ce.output.WriteString(" %r11\n")
			}
		} else {
			ce.output.WriteString("    ")
			ce.output.WriteString(divOp64)
			ce.output.WriteString(" ")
			ce.output.WriteString(ce.formatOperand(src2))
			ce.output.WriteString("\n")
		}

		ce.line2("movq", "%rax", ce.formatOperand(dst))
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
		ce.output.WriteString("    movl ")
		ce.output.WriteString(ce.formatOperand32(src1))
		ce.output.WriteString(", %eax\n")
		ce.output.WriteString("    ")
		ce.output.WriteString(extOp)
		ce.output.WriteString("\n")

		if src2.Type == "imm" {
			ce.output.WriteString("    movl $")
			ce.output.WriteString(src2.Value)
			ce.output.WriteString(", %r11d\n")
			ce.output.WriteString("    ")
			ce.output.WriteString(divOp)
			ce.output.WriteString(" %r11d\n")
		} else {
			ce.output.WriteString("    ")
			ce.output.WriteString(divOp)
			ce.output.WriteString(" ")
			ce.output.WriteString(ce.formatOperand32(src2))
			ce.output.WriteString("\n")
		}

		ce.output.WriteString("    movl %edx, ")
		ce.output.WriteString(ce.formatOperand32(dst))
		ce.output.WriteString("\n")
	} else {
		// 64-bit division (original code)
		ce.line2("movq", ce.formatOperand(src1), "%rax")
		ce.output.WriteString("    ")
		ce.output.WriteString(extOp64)
		ce.output.WriteString("\n")

		if src2.Type == "imm" {
			src2Str := ce.loadFloatIfNeeded(src2, "%r11")
			if src2Str == "%r11" {
				ce.output.WriteString("    ")
				ce.output.WriteString(divOp64)
				ce.output.WriteString(" %r11\n")
			} else {
				ce.output.WriteString("    movq ")
				ce.output.WriteString(src2Str)
				ce.output.WriteString(", %r11\n")
				ce.output.WriteString("    ")
				ce.output.WriteString(divOp64)
				ce.output.WriteString(" %r11\n")
			}
		} else {
			ce.output.WriteString("    ")
			ce.output.WriteString(divOp64)
			ce.output.WriteString(" ")
			ce.output.WriteString(ce.formatOperand(src2))
			ce.output.WriteString("\n")
		}

		ce.output.WriteString("    movq %rdx, ")
		ce.output.WriteString(ce.formatOperand(dst))
		ce.output.WriteString("\n")
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
		ce.output.WriteString("    movq ")
		ce.output.WriteString(ce.formatOperand(src2))
		ce.output.WriteString(", %rcx\n")
		ce.output.WriteString("    ")
		ce.output.WriteString(op)
		ce.output.WriteString(" %cl, ")
		ce.output.WriteString(ce.formatOperand(dst))
		ce.output.WriteString("\n")
	} else {
		// Handle float immediates
		src2Str := ce.loadFloatIfNeeded(src2, "%rcx")
		if src2Str == "%rcx" {
			ce.output.WriteString("    ")
			ce.output.WriteString(op)
			ce.output.WriteString(" %cl, ")
			ce.output.WriteString(ce.formatOperand(dst))
			ce.output.WriteString("\n")
		} else {
			ce.output.WriteString("    ")
			ce.output.WriteString(op)
			ce.output.WriteString(" ")
			ce.output.WriteString(src2Str)
			ce.output.WriteString(", ")
			ce.output.WriteString(ce.formatOperand(dst))
			ce.output.WriteString("\n")
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
		ce.output.WriteString("    movq ")
		ce.output.WriteString(label)
		ce.output.WriteString("(%rip), %r11\n")
		src1Str = "%r11"
	}
	if src2.Type == "imm" && (src2.DataType == "float" || src2.DataType == "double") {
		label := ce.getFloatLabel(src2.Value)
		// Load into a register for comparison
		ce.output.WriteString("    movq ")
		ce.output.WriteString(label)
		ce.output.WriteString("(%rip), %r10\n")
		src2Str = "%r10"
	} else if src2Str == ce.formatOperand(src2) {
		src2Str = ce.loadIntImmIfNeeded(src2, "%r10")
	}
	if src1Str == ce.formatOperand(src1) {
		src1Str = ce.loadIntImmIfNeeded(src1, "%r11")
	}

	// x86 cmp needs a register or memory side: two immediates (e.g. an
	// `if (RTLD_LAZY != 1)` guard after macro expansion) are unencodable,
	// so stage the left side in a scratch register.
	if strings.HasPrefix(src1Str, "$") && strings.HasPrefix(src2Str, "$") {
		if cmpOp == "cmpl" {
			ce.output.WriteString("    movl ")
			ce.output.WriteString(src1Str)
			ce.output.WriteString(", %r11d\n")
			src1Str = "%r11d"
		} else {
			ce.output.WriteString("    movq ")
			ce.output.WriteString(src1Str)
			ce.output.WriteString(", %r11\n")
			src1Str = "%r11"
		}
	}

	src1IsMem := strings.Contains(src1Str, "(") && strings.Contains(src1Str, ")")
	src2IsMem := strings.Contains(src2Str, "(") && strings.Contains(src2Str, ")")
	
	if src1IsMem && src2IsMem {
		// Both are memory - load one into register
		if cmpOp == "cmpl" {
			ce.output.WriteString("    movl ")
			ce.output.WriteString(src1Str)
			ce.output.WriteString(", %eax\n")
			ce.output.WriteString("    cmpl ")
			ce.output.WriteString(src2Str)
			ce.output.WriteString(", %eax\n")
		} else {
			ce.line2("movq", src1Str, "%rax")
			ce.output.WriteString("    cmpq ")
			ce.output.WriteString(src2Str)
			ce.output.WriteString(", %rax\n")
		}
	} else {
		ce.output.WriteString("    ")
		ce.output.WriteString(cmpOp)
		ce.output.WriteString(" ")
		ce.output.WriteString(src2Str)
		ce.output.WriteString(", ")
		ce.output.WriteString(src1Str)
		ce.output.WriteString("\n")
	}
	
	ce.lineA(setcc, "%al")
	
	dstStr := ce.formatOperand(dst)
	dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
	
	if dstIsMem {
		ce.output.WriteString("    movzbq %al, %rax\n")
		ce.line2("movq", "%rax", dstStr)
	} else {
		ce.output.WriteString("    movzbq %al, ")
		ce.output.WriteString(dstStr)
		ce.output.WriteString("\n")
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
						ce.output.WriteString("    movl ")
						ce.output.WriteString(ce.globalRef(src))
						ce.output.WriteString(", %eax\n")
					} else if src.Size == 2 {
						if unsigned {
							ce.output.WriteString("    movzwl ")
							ce.output.WriteString(ce.globalRef(src))
							ce.output.WriteString(", %eax\n")
						} else {
							ce.output.WriteString("    movswl ")
							ce.output.WriteString(ce.globalRef(src))
							ce.output.WriteString(", %eax\n")
						}
					} else if src.Size == 1 {
						if unsigned {
							ce.output.WriteString("    movzbl ")
							ce.output.WriteString(ce.globalRef(src))
							ce.output.WriteString(", %eax\n")
						} else {
							ce.output.WriteString("    movsbl ")
							ce.output.WriteString(ce.globalRef(src))
							ce.output.WriteString(", %eax\n")
						}
					}
				} else {
					unsigned := strings.Contains(src.DataType, "unsigned")
					if src.Size == 4 {
						ce.output.WriteString("    movl ")
						ce.output.WriteString(strconv.Itoa(src.Offset))
						ce.output.WriteString("(%rbp), %eax\n")
					} else if src.Size == 2 {
						if unsigned {
							ce.output.WriteString("    movzwl ")
							ce.output.WriteString(strconv.Itoa(src.Offset))
							ce.output.WriteString("(%rbp), %eax\n")
						} else {
							ce.output.WriteString("    movswl ")
							ce.output.WriteString(strconv.Itoa(src.Offset))
							ce.output.WriteString("(%rbp), %eax\n")
						}
					} else if src.Size == 1 {
						if unsigned {
							ce.output.WriteString("    movzbl ")
							ce.output.WriteString(strconv.Itoa(src.Offset))
							ce.output.WriteString("(%rbp), %eax\n")
						} else {
							ce.output.WriteString("    movsbl ")
							ce.output.WriteString(strconv.Itoa(src.Offset))
							ce.output.WriteString("(%rbp), %eax\n")
						}
					}
				}
				// movl to %eax zeros upper 32 bits of %rax
				ce.line2("movq", "%rax", dstStr)
			} else {
				// Default 8-byte load
				if src.IsGlobal {
					ce.line2("movq", ce.globalRef(src), "%rax")
				} else {
					ce.output.WriteString("    movq ")
					ce.output.WriteString(strconv.Itoa(src.Offset))
					ce.output.WriteString("(%rbp), %rax\n")
				}
				ce.line2("movq", "%rax", dstStr)
			}
		} else {
				// Direct load to register - use appropriate size
				if src.Size > 0 && src.Size < 8 {
					if src.IsGlobal {
						// For direct register loads, we can use movl directly which zeros upper bits
						dstStr32 := ce.formatOperand32(dst)
						unsigned := strings.Contains(src.DataType, "unsigned")
						if src.Size == 4 {
								ce.output.WriteString("    movl ")
								ce.output.WriteString(ce.globalRef(src))
								ce.output.WriteString(", ")
								ce.output.WriteString(dstStr32)
								ce.output.WriteString("\n")
						} else if src.Size == 2 {
								if unsigned {
									ce.output.WriteString("    movzwl ")
									ce.output.WriteString(ce.globalRef(src))
									ce.output.WriteString(", ")
									ce.output.WriteString(dstStr32)
									ce.output.WriteString("\n")
								} else {
									ce.output.WriteString("    movswl ")
									ce.output.WriteString(ce.globalRef(src))
									ce.output.WriteString(", ")
									ce.output.WriteString(dstStr32)
									ce.output.WriteString("\n")
								}
						} else if src.Size == 1 {
								if unsigned {
									ce.output.WriteString("    movzbl ")
									ce.output.WriteString(ce.globalRef(src))
									ce.output.WriteString(", ")
									ce.output.WriteString(dstStr32)
									ce.output.WriteString("\n")
								} else {
									ce.output.WriteString("    movsbl ")
									ce.output.WriteString(ce.globalRef(src))
									ce.output.WriteString(", ")
									ce.output.WriteString(dstStr32)
									ce.output.WriteString("\n")
								}
						}
					} else {
					// Local variable load (signed types sign-extend;
					// plain char/short are signed on x86-64).
					dstStr32 := ce.formatOperand32(dst)
					unsigned := strings.Contains(src.DataType, "unsigned")
					if src.Size == 4 {
						ce.output.WriteString("    movl ")
						ce.output.WriteString(strconv.Itoa(src.Offset))
						ce.output.WriteString("(%rbp), ")
						ce.output.WriteString(dstStr32)
						ce.output.WriteString("\n")
					} else if src.Size == 2 {
						if unsigned {
							ce.output.WriteString("    movzwl ")
							ce.output.WriteString(strconv.Itoa(src.Offset))
							ce.output.WriteString("(%rbp), ")
							ce.output.WriteString(dstStr32)
							ce.output.WriteString("\n")
						} else {
							ce.output.WriteString("    movswl ")
							ce.output.WriteString(strconv.Itoa(src.Offset))
							ce.output.WriteString("(%rbp), ")
							ce.output.WriteString(dstStr32)
							ce.output.WriteString("\n")
						}
					} else if src.Size == 1 {
						if unsigned {
							ce.output.WriteString("    movzbl ")
							ce.output.WriteString(strconv.Itoa(src.Offset))
							ce.output.WriteString("(%rbp), ")
							ce.output.WriteString(dstStr32)
							ce.output.WriteString("\n")
						} else {
							ce.output.WriteString("    movsbl ")
							ce.output.WriteString(strconv.Itoa(src.Offset))
							ce.output.WriteString("(%rbp), ")
							ce.output.WriteString(dstStr32)
							ce.output.WriteString("\n")
						}
					}
				}
			} else {
				// Default 8-byte load
				if src.IsGlobal {
					ce.output.WriteString("    movq ")
					ce.output.WriteString(ce.globalRef(src))
					ce.output.WriteString(", ")
					ce.output.WriteString(dstStr)
					ce.output.WriteString("\n")
				} else {
					ce.output.WriteString("    movq ")
					ce.output.WriteString(strconv.Itoa(src.Offset))
					ce.output.WriteString("(%rbp), ")
					ce.output.WriteString(dstStr)
					ce.output.WriteString("\n")
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
			ce.output.WriteString("    movq ")
			ce.output.WriteString(indexReg)
			ce.output.WriteString(", %r11\n")
			indexReg = "%r11"
		}
		
		if src.IsGlobal {
			// Global array: load from symbol + offset
			ce.output.WriteString("    leaq ")
			ce.output.WriteString(ce.globalRef(src))
			ce.output.WriteString(", %rdx\n")
			ce.emitArrayLoadFromBase(dst, dstReg, src.Size, indexReg)
		} else {
			// Local array: load from rbp + base_offset + computed_offset
			ce.output.WriteString("    leaq ")
			ce.output.WriteString(strconv.Itoa(src.Offset))
			ce.output.WriteString("(%rbp), %rdx\n")
			ce.emitArrayLoadFromBase(dst, dstReg, src.Size, indexReg)
		}
	case "addr":
		// Address-of: compute address and store in dst
		dstStr := ce.formatOperand(dst)
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		
		if dstIsMem {
			// Destination is memory, go through rax
			if src.IsGlobal {
				ce.output.WriteString("    leaq ")
				ce.output.WriteString(ce.globalRef(src))
				ce.output.WriteString(", %rax\n")
			} else {
				ce.output.WriteString("    leaq ")
				ce.output.WriteString(strconv.Itoa(src.Offset))
				ce.output.WriteString("(%rbp), %rax\n")
			}
			ce.line2("movq", "%rax", dstStr)
		} else {
			// Destination is register
			if src.IsGlobal {
				ce.output.WriteString("    leaq ")
				ce.output.WriteString(src.Value)
				ce.output.WriteString("(%rip), ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    leaq ")
				ce.output.WriteString(strconv.Itoa(src.Offset))
				ce.output.WriteString("(%rbp), ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
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
			ce.output.WriteString("    movq ")
			ce.output.WriteString(ptrReg)
			ce.output.WriteString(", %r11\n")
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
					ce.output.WriteString("    movb (")
					ce.output.WriteString(ptrReg)
					ce.output.WriteString("), %al\n")
					ce.output.WriteString("    movzbq %al, %rax\n")  // Zero-extend to 64-bit
				} else {
					ce.output.WriteString("    movsbq (")
					ce.output.WriteString(ptrReg)
					ce.output.WriteString("), %rax\n")
				}
				ce.line2("movq", "%rax", dstStr)
			} else if movInstr == "movw" {
				if isUnsigned {
					ce.output.WriteString("    movzwl (")
					ce.output.WriteString(ptrReg)
					ce.output.WriteString("), %eax\n")
				} else {
					ce.output.WriteString("    movswl (")
					ce.output.WriteString(ptrReg)
					ce.output.WriteString("), %eax\n")
				}
				ce.line2("movq", "%rax", dstStr)
			} else if movInstr == "movl" {
				// movl zeros upper 32 bits automatically
				ce.output.WriteString("    movl (")
				ce.output.WriteString(ptrReg)
				ce.output.WriteString("), %eax\n")
				ce.line2("movq", "%rax", dstStr)
			} else {
				ce.output.WriteString("    movq (")
				ce.output.WriteString(ptrReg)
				ce.output.WriteString("), %rax\n")
				ce.line2("movq", "%rax", dstStr)
			}
		} else {
			// Register destination: load DIRECTLY into it (no rax staging:
			// a hardcoded scratch register would clobber a live temp
			// allocated there — this exact bug returned 0 for a[0]-b[0].
			if movInstr == "movb" {
				dstStr32 := ce.formatOperand32(dst)
				if isUnsigned {
					ce.output.WriteString("    movzbl (")
					ce.output.WriteString(ptrReg)
					ce.output.WriteString("), ")
					ce.output.WriteString(dstStr32)
					ce.output.WriteString("\n")
				} else {
					ce.output.WriteString("    movsbl (")
					ce.output.WriteString(ptrReg)
					ce.output.WriteString("), ")
					ce.output.WriteString(dstStr32)
					ce.output.WriteString("\n")
				}
			} else if movInstr == "movw" {
				dstStr32 := ce.formatOperand32(dst)
				if isUnsigned {
					ce.output.WriteString("    movzwl (")
					ce.output.WriteString(ptrReg)
					ce.output.WriteString("), ")
					ce.output.WriteString(dstStr32)
					ce.output.WriteString("\n")
				} else {
					ce.output.WriteString("    movswl (")
					ce.output.WriteString(ptrReg)
					ce.output.WriteString("), ")
					ce.output.WriteString(dstStr32)
					ce.output.WriteString("\n")
				}
			} else if movInstr == "movl" {
				// movl to 32-bit register zeros upper 32 bits
				dstStr32 := ce.formatOperand32(dst)
				ce.output.WriteString("    movl (")
				ce.output.WriteString(ptrReg)
				ce.output.WriteString("), ")
				ce.output.WriteString(dstStr32)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movq (")
				ce.output.WriteString(ptrReg)
				ce.output.WriteString("), ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			}
		}
	case "label":
		// String literal or global label - use leaq to load address
		dstStr := ce.formatOperand(dst)
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		
		if dstIsMem {
			ce.output.WriteString("    leaq ")
			ce.output.WriteString(ce.globalRef(src))
			ce.output.WriteString(", %rax\n")
			ce.line2("movq", "%rax", dstStr)
		} else {
			ce.output.WriteString("    leaq ")
			ce.output.WriteString(src.Value)
			ce.output.WriteString("(%rip), ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		}
	case "mem":
		// Load from stack location
		dstStr := ce.formatOperand(dst)
		srcStr := ce.formatOperand(src) // This will be offset(%rbp)
		
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		
		if dstIsMem {
			// Mem to mem - use intermediate register
			ce.line2("movq", srcStr, "%rax")
			ce.line2("movq", "%rax", dstStr)
		} else {
			// Mem to register - direct move
			ce.line2("movq", srcStr, dstStr)
		}
	default:
		ce.emitMov(dst, src)
	}
}

// emitSizedStoreToGlobal stores a register to a global symbol, honoring
// narrow widths so 4-byte globals don't clobber their BSS neighbors.
func (ce *CodeEmitter) emitSizedStoreToGlobal(dst *Operand, srcReg string) {
	if dst.Size == 4 {
		ce.output.WriteString("    movl ")
		ce.output.WriteString("%")
		ce.output.WriteString(ce.reg64to32(strings.TrimPrefix(srcReg, "%")))
		ce.output.WriteString(", ")
		ce.output.WriteString(ce.globalRef(dst))
		ce.output.WriteString("\n")
	} else if dst.Size == 2 {
		ce.output.WriteString("    movw ")
		ce.output.WriteString(ce.get16BitReg(srcReg))
		ce.output.WriteString(", ")
		ce.output.WriteString(ce.globalRef(dst))
		ce.output.WriteString("\n")
	} else if dst.Size == 1 {
		ce.output.WriteString("    movb ")
		ce.output.WriteString(ce.get8BitReg(srcReg))
		ce.output.WriteString(", ")
		ce.output.WriteString(ce.globalRef(dst))
		ce.output.WriteString("\n")
	} else {
		ce.output.WriteString("    movq ")
		ce.output.WriteString(srcReg)
		ce.output.WriteString(", ")
		ce.output.WriteString(ce.globalRef(dst))
		ce.output.WriteString("\n")
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
			ce.output.WriteString("    movsd %xmm0, ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		} else {
			ce.output.WriteString(fmt.Sprintf("    movq %%xmm0, %%xmm1\n"))
			ce.output.WriteString("    movq %xmm1, ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		}
		return true
	}
	if srcIsDouble && dstWidth == 4 {
		// double -> float: convert down.
		ce.emitFloatLoadToXmm(src, "%xmm0")
		dstStr := ce.formatOperand(dst)
		ce.output.WriteString(fmt.Sprintf("    cvtsd2ss %%xmm0, %%xmm0\n"))
		if strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")") {
			ce.output.WriteString("    movss %xmm0, ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		} else {
			ce.output.WriteString("    movd %xmm0, ")
			ce.output.WriteString(ce.formatOperand32(dst))
			ce.output.WriteString("\n")
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
				ce.output.WriteString("    leaq ")
				ce.output.WriteString(ce.globalRef(src))
				ce.output.WriteString(", %rax\n")
				ce.line2("movq", "%rax", ce.globalRef(dst))
			} else {
				ce.output.WriteString("    leaq ")
				ce.output.WriteString(ce.globalRef(src))
				ce.output.WriteString(", %rax\n")
				ce.output.WriteString("    movq %rax, ")
				ce.output.WriteString(strconv.Itoa(dst.Offset))
				ce.output.WriteString("(%rbp)\n")
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
					ce.line2("movq", loadedStr, "%rax")
				}
				ce.emitSizedStoreToGlobal(dst, "%rax")
			} else {
				ce.emitSizedStoreToGlobal(dst, srcStr)
			}
		} else {
			if srcIsMem || src.Type == "imm" {
				if src.Type == "imm" && src.DataType == "float" {
				// Single-precision literal: materialize exact bits.
				ce.output.WriteString("    movl $")
				ce.output.WriteString(strconv.FormatUint(uint64(floatBits32(src.Value)), 10))
				ce.output.WriteString(", %eax\n")
				} else if src.Type == "imm" && (src.DataType == "double" || isFloatSpelling(src.Value)) {
				// Double immediate via .rodata.
				loadedStr := ce.loadFloatIfNeeded(src, "%rax")
				if loadedStr != "%rax" {
					ce.line2("movq", loadedStr, "%rax")
				}
				} else if src.Type == "imm" {
					// Integer immediate: movabs when too wide for imm32
					// (a plain movl silently truncated UL constants).
					if dst.Size > 0 && dst.Size < 8 {
						ce.output.WriteString("    movl $")
						ce.output.WriteString(src.Value)
						ce.output.WriteString(", %eax\n")
					} else {
						ce.emitImmToReg64(src.Value, "%rax")
					}
				} else {
					ce.line2("movq", ce.formatOperand(src), "%rax")
				}
				// Check if we have a specific size to store
				if dst.Size > 0 && dst.Size < 8 {
					// Use appropriately sized move instruction
					if dst.Size == 4 {
						ce.output.WriteString("    movl %eax, ")
						ce.output.WriteString(strconv.Itoa(dst.Offset))
						ce.output.WriteString("(%rbp)\n")
					} else if dst.Size == 2 {
						ce.output.WriteString("    movw %ax, ")
						ce.output.WriteString(strconv.Itoa(dst.Offset))
						ce.output.WriteString("(%rbp)\n")
					} else if dst.Size == 1 {
						ce.output.WriteString("    movb %al, ")
						ce.output.WriteString(strconv.Itoa(dst.Offset))
						ce.output.WriteString("(%rbp)\n")
					} else {
						ce.output.WriteString("    movq %rax, ")
						ce.output.WriteString(strconv.Itoa(dst.Offset))
						ce.output.WriteString("(%rbp)\n")
					}
				} else {
				// Wide slot: %rax already holds the full value.
				ce.output.WriteString("    movq %rax, ")
				ce.output.WriteString(strconv.Itoa(dst.Offset))
				ce.output.WriteString("(%rbp)\n")
			}
			} else if dst.Size > 0 && dst.Size < 8 {
				// Register source into a narrow slot (e.g. i = computed int).
				if dst.Size == 4 {
					ce.output.WriteString("    movl ")
					ce.output.WriteString(ce.formatOperand32(src))
					ce.output.WriteString(", ")
					ce.output.WriteString(strconv.Itoa(dst.Offset))
					ce.output.WriteString("(%rbp)\n")
				} else if dst.Size == 2 {
					ce.output.WriteString("    movw ")
					ce.output.WriteString(ce.get16BitReg(srcStr))
					ce.output.WriteString(", ")
					ce.output.WriteString(strconv.Itoa(dst.Offset))
					ce.output.WriteString("(%rbp)\n")
				} else if dst.Size == 1 {
					ce.output.WriteString("    movb ")
					ce.output.WriteString(ce.get8BitReg(srcStr))
					ce.output.WriteString(", ")
					ce.output.WriteString(strconv.Itoa(dst.Offset))
					ce.output.WriteString("(%rbp)\n")
				} else {
					ce.output.WriteString("    movq ")
					ce.output.WriteString(srcStr)
					ce.output.WriteString(", ")
					ce.output.WriteString(strconv.Itoa(dst.Offset))
					ce.output.WriteString("(%rbp)\n")
				}
			} else {
				ce.output.WriteString("    movq ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(strconv.Itoa(dst.Offset))
				ce.output.WriteString("(%rbp)\n")
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
			ce.output.WriteString("    movq ")
			ce.output.WriteString(indexReg)
			ce.output.WriteString(", %r11\n")
			indexReg = "%r11"
		}
		
		// Get source value into rax (movabs for wide integer immediates)
		srcReg := ce.formatOperand(src)
		if src.Type == "imm" && src.DataType == "float" {
			ce.output.WriteString("    movl $")
			ce.output.WriteString(strconv.FormatUint(uint64(floatBits32(src.Value)), 10))
			ce.output.WriteString(", %eax\n")
		} else if src.Type == "imm" && !isFloatSpelling(src.Value) && src.DataType != "float" && src.DataType != "double" {
			ce.emitImmToReg64(src.Value, "%rax")
		} else if src.Type == "imm" {
			// Use helper for float immediates
			loadedStr := ce.loadFloatIfNeeded(src, "%rax")
			if loadedStr != "%rax" {
				ce.line2("movq", loadedStr, "%rax")
			}
		} else {
			ce.line2("movq", srcReg, "%rax")
		}
		
		if dst.IsGlobal {
			// Global array: store to symbol + offset
			ce.output.WriteString("    leaq ")
			ce.output.WriteString(ce.globalRef(dst))
			ce.output.WriteString(", %rdx\n")
			ce.emitArrayStoreToBase(dst.Size, indexReg)
		} else {
			// Local array: store to rbp + base_offset + computed_offset
			ce.output.WriteString("    leaq ")
			ce.output.WriteString(strconv.Itoa(dst.Offset))
			ce.output.WriteString("(%rbp), %rdx\n")
			ce.emitArrayStoreToBase(dst.Size, indexReg)
		}
	case "ptr":
		// Dereference store: store to address in IndexTemp
		ptrReg := ce.formatOperand(dst.IndexTemp)
		ptrIsMem := strings.Contains(ptrReg, "(") && strings.Contains(ptrReg, ")")
		
		// Load pointer into a dedicated register if it's in memory
		if ptrIsMem {
			ce.output.WriteString("    movq ")
			ce.output.WriteString(ptrReg)
			ce.output.WriteString(", %r11\n")
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
				ce.output.WriteString("    leaq ")
				ce.output.WriteString(srcReg)
				ce.output.WriteString("(%rip), ")
				ce.output.WriteString(valueReg)
				ce.output.WriteString("\n")
			} else if src.Type == "imm" && src.DataType == "float" {
				ce.output.WriteString("    movl $")
				ce.output.WriteString(strconv.FormatUint(uint64(floatBits32(src.Value)), 10))
				ce.output.WriteString(", ")
				ce.output.WriteString("%")
				ce.output.WriteString(ce.reg64to32(strings.TrimPrefix(valueReg, "%")))
				ce.output.WriteString("\n")
			} else if src.Type == "imm" {
				// Use helper for float immediates
				loadedStr := ce.loadFloatIfNeeded(src, valueReg)
				if loadedStr != valueReg {
					ce.output.WriteString("    movq ")
					ce.output.WriteString(loadedStr)
					ce.output.WriteString(", ")
					ce.output.WriteString(valueReg)
					ce.output.WriteString("\n")
				}
			} else {
				ce.output.WriteString("    movq ")
				ce.output.WriteString(srcReg)
				ce.output.WriteString(", ")
				ce.output.WriteString(valueReg)
				ce.output.WriteString("\n")
			}
			srcReg = valueReg
		}
		
		// Use appropriate store size based on dst.Size
		if dst.Size == 4 {
			// 32-bit store - convert register to 32-bit version
			srcReg32 := ce.get32BitReg(srcReg)
			ce.output.WriteString("    movl ")
			ce.output.WriteString(srcReg32)
			ce.output.WriteString(", (")
			ce.output.WriteString(ptrReg)
			ce.output.WriteString(")\n")
		} else if dst.Size == 2 {
			// 16-bit store
			srcReg16 := ce.get16BitReg(srcReg)
			ce.output.WriteString("    movw ")
			ce.output.WriteString(srcReg16)
			ce.output.WriteString(", (")
			ce.output.WriteString(ptrReg)
			ce.output.WriteString(")\n")
		} else if dst.Size == 1 {
			// 8-bit store
			srcReg8 := ce.get8BitReg(srcReg)
			ce.output.WriteString("    movb ")
			ce.output.WriteString(srcReg8)
			ce.output.WriteString(", (")
			ce.output.WriteString(ptrReg)
			ce.output.WriteString(")\n")
		} else {
			// 64-bit store (default)
			ce.output.WriteString("    movq ")
			ce.output.WriteString(srcReg)
			ce.output.WriteString(", (")
			ce.output.WriteString(ptrReg)
			ce.output.WriteString(")\n")
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
	ce.floatOrder = append(ce.floatOrder, label)
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
	ce.floatOrder = append(ce.floatOrder, label)
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
	norm := strings.TrimSpace(op.DataType)
	for _, q := range []string{"static ", "inline ", "extern ", "const ", "volatile ", "register "} {
		for strings.HasPrefix(norm, q) {
			norm = strings.TrimSpace(norm[len(q):])
		}
	}
	isFloat := norm == "float"
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
			ce.output.WriteString("    cvtss2sd ")
			ce.output.WriteString(label)
			ce.output.WriteString("(%rip), ")
			ce.output.WriteString(xmm)
			ce.output.WriteString("\n")
		} else {
			label := ce.getFloatLabel(op.Value)
			ce.output.WriteString("    movsd ")
			ce.output.WriteString(label)
			ce.output.WriteString("(%rip), ")
			ce.output.WriteString(xmm)
			ce.output.WriteString("\n")
		}
	case "temp", "reg":
		s := ce.formatOperand(op)
		if strings.Contains(s, "(") && strings.Contains(s, ")") {
			// Spilled temp: memory load.
			if isFloat {
				ce.output.WriteString("    cvtss2sd ")
				ce.output.WriteString(s)
				ce.output.WriteString(", ")
				ce.output.WriteString(xmm)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movsd ")
				ce.output.WriteString(s)
				ce.output.WriteString(", ")
				ce.output.WriteString(xmm)
				ce.output.WriteString("\n")
			}
		} else {
			// GPR holds the bit pattern.
			if isFloat {
				ce.output.WriteString("    movd ")
				ce.output.WriteString(ce.formatOperand32(op))
				ce.output.WriteString(", ")
				ce.output.WriteString(xmm)
				ce.output.WriteString("\n")
				ce.output.WriteString("    cvtss2sd ")
				ce.output.WriteString(xmm)
				ce.output.WriteString(", ")
				ce.output.WriteString(xmm)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movq ")
				ce.output.WriteString(s)
				ce.output.WriteString(", ")
				ce.output.WriteString(xmm)
				ce.output.WriteString("\n")
			}
		}
	default:
		s := ce.formatOperand(op)
		if isFloat {
			ce.output.WriteString("    cvtss2sd ")
			ce.output.WriteString(s)
			ce.output.WriteString(", ")
			ce.output.WriteString(xmm)
			ce.output.WriteString("\n")
		} else {
			ce.output.WriteString("    movsd ")
			ce.output.WriteString(s)
			ce.output.WriteString(", ")
			ce.output.WriteString(xmm)
			ce.output.WriteString("\n")
		}
	}
}

// emitFloatStoreFromXmm stores xmm (holding a double) to dst: direct movsd
// for double destinations, cvtsd2ss+movd for known-float ones.
func (ce *CodeEmitter) emitFloatStoreFromXmm(xmm string, dst *Operand) {
	dstStr := ce.formatOperand(dst)
	dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
	if dst.DataType == "float" {
		ce.output.WriteString("    cvtsd2ss ")
		ce.output.WriteString(xmm)
		ce.output.WriteString(", ")
		ce.output.WriteString(xmm)
		ce.output.WriteString("\n")
		if dst.Type == "temp" || dst.Type == "reg" {
			if dstIsMem {
				ce.output.WriteString("    movd ")
				ce.output.WriteString(xmm)
				ce.output.WriteString(", %eax\n")
				ce.output.WriteString("    movl %eax, ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movd ")
				ce.output.WriteString(xmm)
				ce.output.WriteString(", ")
				ce.output.WriteString(ce.formatOperand32(dst))
				ce.output.WriteString("\n")
			}
		} else {
			ce.output.WriteString("    movss ")
			ce.output.WriteString(xmm)
			ce.output.WriteString(", ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		}
		return
	}
	if dst.Type == "temp" || dst.Type == "reg" {
		if dstIsMem {
			ce.line2("movq", xmm, "%rax")
			ce.line2("movq", "%rax", dstStr)
		} else {
			ce.output.WriteString("    movq ")
			ce.output.WriteString(xmm)
			ce.output.WriteString(", ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		}
	} else {
		ce.output.WriteString("    movsd ")
		ce.output.WriteString(xmm)
		ce.output.WriteString(", ")
		ce.output.WriteString(dstStr)
		ce.output.WriteString("\n")
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
				ce.output.WriteString("    cvtss2sd ")
				ce.output.WriteString(label)
				ce.output.WriteString("(%rip), ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				// Double immediate (numerically exact as-is).
				label := ce.getFloatLabel(src.Value)
				ce.output.WriteString("    movsd ")
				ce.output.WriteString(label)
				ce.output.WriteString("(%rip), ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			}
		case "temp", "reg", "mem", "var":
			if src.DataType == "float" {
				ce.emitFloatLoadToXmm(src, dstStr)
			} else {
			srcStr := ce.formatOperand(src)
			if strings.Contains(srcStr, "(") && strings.Contains(srcStr, ")") {
				// Source is in memory
				ce.output.WriteString("    movsd ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				// Source is in a GPR - use movq to move bitwise
				ce.line2("movq", srcStr, dstStr)
			}
			}
		default:
			srcStr := ce.formatOperand(src)
			ce.output.WriteString("    movsd ")
			ce.output.WriteString(srcStr)
			ce.output.WriteString(", ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		}
	} else {
		// Moving to integer register (movabs for wide immediates)
		switch src.Type {
		case "imm":
			if !isFloatSpelling(src.Value) {
				ce.emitImmToReg64(src.Value, dstStr)
			} else {
				ce.output.WriteString("    movq $")
				ce.output.WriteString(src.Value)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			}
		case "temp", "reg":
			srcStr := ce.formatOperand(src)
			if strings.Contains(srcStr, "(") && strings.Contains(srcStr, ")") {
				ce.line2("movq", srcStr, dstStr)
			} else {
				ce.line2("movq", srcStr, dstStr)
			}
		case "mem", "var":
			srcStr := ce.formatOperand(src)
			ce.line2("movq", srcStr, dstStr)
		case "label":
			ce.output.WriteString("    leaq ")
			ce.output.WriteString(src.Value)
			ce.output.WriteString("(%rip), ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		default:
			srcStr := ce.formatOperand(src)
			ce.line2("movq", srcStr, dstStr)
		}
	}
}

// Helper to load a float immediate into a register if needed
// immFits32 reports whether an integer immediate fits in a signed
// 32-bit instruction immediate (what andq/orq/addq/cmpq accept).
func immFits32(val string) bool {
	v := strings.TrimSpace(val)
	// Fast path: short plain decimals (the common case: "0", "8", "16")
	// always fit, with no ParseUint/TrimRight overhead.
	if len(v) > 0 && len(v) <= 10 {
		i := 0
		if v[0] == '-' || v[0] == '+' {
			i = 1
		}
		if i < len(v) && len(v)-i <= 9 {
			allDigits := true
			for j := i; j < len(v); j++ {
				if v[j] < '0' || v[j] > '9' {
					allDigits = false
					break
				}
			}
			// <=999,999,999 < 2^31: fits for either sign.
			if allDigits {
				return true
			}
		}
	}
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
		ce.output.WriteString("    movabs $")
		ce.output.WriteString(op.Value)
		ce.output.WriteString(", ")
		ce.output.WriteString(tempReg)
		ce.output.WriteString("\n")
		return tempReg
	}
	return ce.formatOperand(op)
}

func (ce *CodeEmitter) loadFloatIfNeeded(op *Operand, tempReg string) string {
	// Only treat as float if it's explicitly a float type
	if op.Type == "imm" && (op.DataType == "float" || op.DataType == "double") {
		// Float immediate - load from .rodata
		label := ce.getFloatLabel(op.Value)
		ce.output.WriteString("    movq ")
		ce.output.WriteString(label)
		ce.output.WriteString("(%rip), ")
		ce.output.WriteString(tempReg)
		ce.output.WriteString("\n")
		return tempReg
	}
	return ce.formatOperand(op)
}

// emitIntToXmmDoubleTo loads an integer operand to any xmm as a double
// (64-bit longs use the q form; 32-bit and below go through eax).
func (ce *CodeEmitter) emitIntToXmmDoubleTo(op *Operand, xmm string) {
	if s, ok := cSizeOf(strings.TrimSpace(op.DataType)); ok && s == 8 {
		if op.Type == "imm" {
			// cvtsi2sdq has no immediate form: materialize via rax.
			ce.output.WriteString("    movabs $")
			ce.output.WriteString(strings.TrimRight(op.Value, "uUlL"))
			ce.output.WriteString(", %rax\n")
			ce.output.WriteString("    cvtsi2sdq %rax, ")
			ce.output.WriteString(xmm)
			ce.output.WriteString("\n")
			return
		}
		s := ce.formatOperand(op)
		ce.output.WriteString("    cvtsi2sdq ")
		ce.output.WriteString(s)
		ce.output.WriteString(", ")
		ce.output.WriteString(xmm)
		ce.output.WriteString("\n")
		return
	}
	ce.emitIntToEax(op)
	ce.output.WriteString("    cvtsi2sd %eax, ")
	ce.output.WriteString(xmm)
	ce.output.WriteString("\n")
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
		ce.output.WriteString("    movl $")
		ce.output.WriteString(src.Value)
		ce.output.WriteString(", %eax\n")
	case "temp", "reg":
		s := ce.formatOperand(src)
		if strings.Contains(s, "(") && strings.Contains(s, ")") {
			ce.emitMemToEax(s, 4, unsigned)
		} else {
			ce.output.WriteString("    movl ")
			ce.output.WriteString(ce.formatOperand32(src))
			ce.output.WriteString(", %eax\n")
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
			ce.output.WriteString("    movl ")
			ce.output.WriteString(s)
			ce.output.WriteString(", %eax\n")
		}
	}
}

// emitMemToEax loads 1/2/4 bytes from a memory address string into %eax.
func (ce *CodeEmitter) emitMemToEax(mem string, width int, unsigned bool) {
	switch width {
	case 1:
		if unsigned {
			ce.output.WriteString("    movzbl ")
			ce.output.WriteString(mem)
			ce.output.WriteString(", %eax\n")
		} else {
		ce.output.WriteString("    movsbl ")
		ce.output.WriteString(mem)
		ce.output.WriteString(", %eax\n")
		}
	case 2:
		if unsigned {
			ce.output.WriteString("    movzwl ")
			ce.output.WriteString(mem)
			ce.output.WriteString(", %eax\n")
		} else {
			ce.output.WriteString("    movswl ")
			ce.output.WriteString(mem)
			ce.output.WriteString(", %eax\n")
		}
	default:
		ce.output.WriteString("    movl ")
		ce.output.WriteString(mem)
		ce.output.WriteString(", %eax\n")
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
			ce.output.WriteString("    movl $")
			ce.output.WriteString(strconv.FormatInt(v, 10))
			ce.output.WriteString(", ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
			return
		}
		ce.output.WriteString("    movl $")
		ce.output.WriteString(src.Value)
		ce.output.WriteString(", %eax\n")
		if signed {
			// eax holds full value; truncate via al/ax then extend.
			ce.output.WriteString("    movsbl %al, ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		} else {
			ce.output.WriteString("    movzbl %al, ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
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
				ce.output.WriteString("    movsbl ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movzbl ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			}
		} else {
			if signed {
				ce.output.WriteString("    movswl ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movzwl ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			}
		}
	} else {
		if width == 1 {
			r8 := ce.get8BitReg(srcStr)
			if signed {
				ce.output.WriteString("    movsbl ")
				ce.output.WriteString(r8)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movzbl ")
				ce.output.WriteString(r8)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			}
		} else {
			r16 := ce.reg64to16(strings.TrimPrefix(srcStr, "%"))
			if signed {
				ce.output.WriteString("    movswl %")
				ce.output.WriteString(r16)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movzwl %")
				ce.output.WriteString(r16)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
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
			ce.output.WriteString("    movq $")
			ce.output.WriteString(strconv.FormatInt(v, 10))
			ce.output.WriteString(", ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
			return
		}
		ce.emitImmToReg64(src.Value, dstStr)
		return
	}
	srcStr := ce.formatOperand(src)
	if strings.Contains(srcStr, "(") && strings.Contains(srcStr, ")") {
		if isByte {
			if signed {
				ce.output.WriteString("    movsbq ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movzbq ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			}
		} else {
			if signed {
				ce.output.WriteString("    movswq ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movzwq ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			}
		}
	} else if isByte {
		// Register holds the value with low 8 valid: extend via
		// the 8-bit name (movsbq needs an 8-bit source).
		r8 := ce.get8BitReg(ce.formatOperand(src))
		if signed {
			ce.output.WriteString("    movsbq ")
			ce.output.WriteString(r8)
			ce.output.WriteString(", ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		} else {
			ce.output.WriteString("    movzbq ")
			ce.output.WriteString(r8)
			ce.output.WriteString(", ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		}
	} else {
		// Register holds the value with low 16 valid: extend via
		// the 16-bit name (movswq needs a 16-bit source).
		r16 := ce.reg64to16(strings.TrimPrefix(ce.formatOperand(src), "%"))
		if signed {
			ce.output.WriteString("    movswq %")
			ce.output.WriteString(r16)
			ce.output.WriteString(", ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		} else {
			ce.output.WriteString("    movzwq %")
			ce.output.WriteString(r16)
			ce.output.WriteString(", ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
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
				ce.output.WriteString("    movq $")
				ce.output.WriteString(strconv.FormatInt(v, 10))
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
				return
			}
		}
		ce.emitImmToReg64(src.Value, dstStr)
		if signed {
			// movabs zero-extends; fix sign via movslq through eax.
			ce.output.WriteString("    movslq ")
			ce.output.WriteString(ce.formatOperand32(dst))
			ce.output.WriteString(", ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
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
				ce.output.WriteString("    movslq ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movl ")
				ce.output.WriteString(srcStr)
				ce.output.WriteString(", ")
				ce.output.WriteString(dst32)
				ce.output.WriteString("\n")
			}
		} else {
			if signed {
				ce.output.WriteString("    movslq ")
				ce.output.WriteString(ce.formatOperand32(src))
				ce.output.WriteString(", ")
				ce.output.WriteString(dstStr)
				ce.output.WriteString("\n")
			} else {
				ce.output.WriteString("    movl ")
				ce.output.WriteString(ce.formatOperand32(src))
				ce.output.WriteString(", ")
				ce.output.WriteString(dst32)
				ce.output.WriteString("\n")
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
		ce.emitIntToXmmDoubleTo(src, "%xmm0")
		ce.emitFloatStoreFromXmm("%xmm0", dst)
	case "i2f":
		// Same double-based invariant as emitIntToXmmFloat: the store
		// narrows to float when dst is float.
		ce.emitIntToXmmDoubleTo(src, "%xmm0")
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
		if s, ok := cSizeOf(strings.TrimSpace(dst.DataType)); ok && s == 8 {
			// 64-bit destination (long): q-form convert + 8-byte store.
			ce.output.WriteString("    cvttsd2siq %xmm0, %rax\n")
			ce.line2("movq", "%rax", ce.formatOperand(dst))
			break
		}
		ce.output.WriteString("    cvttsd2si %xmm0, %eax\n")
		dstStr := ce.formatOperand(dst)
		if strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")") {
			ce.output.WriteString("    movl %eax, ")
			ce.output.WriteString(dstStr)
			ce.output.WriteString("\n")
		} else {
			ce.output.WriteString("    movl %eax, ")
			ce.output.WriteString(ce.formatOperand32(dst))
			ce.output.WriteString("\n")
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
		ce.output.WriteString("    call *%")
		ce.output.WriteString(instr.Src1.Value)
		ce.output.WriteString("\n")
	} else if instr.Src1.Type == "label" {
		ce.output.WriteString("    call ")
		ce.output.WriteString(instr.Src1.Value)
		ce.output.WriteString("\n")
	} else {
		ce.output.WriteString("    call *")
		ce.output.WriteString(ce.formatOperand(instr.Src1))
		ce.output.WriteString("\n")
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
		return op.Value + "+" + strconv.Itoa(op.Offset) + "(%rip)"
	}
	return op.Value + "(%rip)"
}

func (ce *CodeEmitter) formatOperand(op *Operand) string {
	if op == nil {
		return ""
	}
	// Fast path: registers (half of all post-RA operands) hit a shared
	// precomputed table — no cache-map lookup, no allocation. Anything
	// unlisted falls through to the regular path.
	if op.Type == "reg" || op.Type == "freg" {
		if s, ok := regSpell64[op.Value]; ok {
			return s
		}
	}
	// Leaf paths: identical operands share one format + one string.
	// Labels and default kinds are identity/constant (no lookup at all);
	// offset-only kinds use a string-free key; immediates and globals key
	// by spelling.
	if op.Type == "label" {
		return op.Value
	}
	if op.Type == "imm" {
		if s, ok := ce.immCache[op.Value]; ok {
			ce.opFmtHits++
			return s
		}
		s := ce.formatOperandCold(op)
		if ce.immCache == nil {
			ce.immCache = make(map[string]string)
		}
		ce.immCache[op.Value] = s
		ce.opFmtMisses++
		return s
	}
	if (op.Type == "var" || op.Type == "addr") && op.IsGlobal {
		k := globKey{val: op.Value, off: int32(op.Offset)}
		if s, ok := ce.globCache[k]; ok {
			ce.opFmtHits++
			return s
		}
		s := ce.formatOperandCold(op)
		if ce.globCache == nil {
			ce.globCache = make(map[globKey]string)
		}
		ce.globCache[k] = s
		ce.opFmtMisses++
		return s
	}
	if offCacheable(op) {
		k := offKey{typ: offTypeCode(op.Type), off: int32(op.Offset)}
		if s, ok := ce.offCache[k]; ok {
			ce.opFmtHits++
			return s
		}
		s := ce.formatOperandCold(op)
		if ce.offCache == nil {
			ce.offCache = make(map[offKey]string)
		}
		ce.offCache[k] = s
		ce.opFmtMisses++
		return s
	}
	if ce.opFmtCache != nil {
		if e, ok := ce.opFmtCache[op]; ok && e.ok64 {
			ce.opFmtHits++
			return e.a64
		}
	}
	s := ce.formatOperandCold(op)
	if ce.opFmtCache == nil {
		ce.opFmtCache = make(map[*Operand]opFmt)
	}
	e := ce.opFmtCache[op]
	e.a64, e.ok64 = s, true
	ce.opFmtCache[op] = e
	ce.opFmtMisses++
	return s
}

func (ce *CodeEmitter) formatOperandCold(op *Operand) string {
	
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
		return frameRef(op.Offset, "%rbp")
	case "var":
		if op.IsGlobal {
			return ce.globalRef(op)
		}
		return frameRef(op.Offset, "%rbp")
	case "array":
		// arr[index] where index is in IndexTemp
		// Need to generate: offset(%rbp, %index_reg, 8)
		// For now, simplify by using the computed offset in a temp reg
		if op.IndexTemp != nil {
			indexReg := ce.formatOperand(op.IndexTemp)
			if op.IsGlobal {
				var sb strings.Builder
				sb.Grow(len(op.Value) + len(indexReg) + 8)
				sb.WriteString(op.Value)
				sb.WriteString("(%rip,")
				sb.WriteString(indexReg)
				sb.WriteString(",1)")
				return sb.String()
			}
			var offbuf [24]byte
			b := strconv.AppendInt(offbuf[:0], int64(op.Offset), 10)
			var sb strings.Builder
			sb.Grow(len(b) + len(indexReg) + 8)
			sb.Write(b)
			sb.WriteString("(%rbp,")
			sb.WriteString(indexReg)
			sb.WriteString(",1)")
			return sb.String()
		}
		return frameRef(op.Offset, "%rbp")
	case "addr":
		// Address of variable - use lea
		if op.IsGlobal {
			if op.Offset != 0 {
				var offbuf [24]byte
				b := strconv.AppendInt(offbuf[:0], int64(op.Offset), 10)
				var sb strings.Builder
				sb.Grow(len(op.Value) + len(b) + 7)
				sb.WriteString(op.Value)
				sb.WriteByte('+')
				sb.Write(b)
				sb.WriteString("(%rip)")
				return sb.String()
			}
			return op.Value + "(%rip)"
		}
		return frameRef(op.Offset, "%rbp")
	case "sarg":
		// Stack call argument: rsp-relative (valid between StackAlloc/Free).
		if op.Offset == 0 {
			return "(%rsp)"
		}
		return frameRef(op.Offset, "%rsp")
	case "prm":
		// Incoming stack parameter: positive rbp offset (16+8k per SysV).
		return frameRef(op.Offset, "%rbp")
	case "ptr":
		// Dereference - load from address in IndexTemp
		if op.IndexTemp != nil {
			ptrReg := ce.formatOperand(op.IndexTemp)
			return "(" + ptrReg + ")"
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
