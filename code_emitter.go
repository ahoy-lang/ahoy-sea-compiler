package main

import (
	"fmt"
	"strings"
)

// Code emitter - generates x86-64 assembly from IR
type CodeEmitter struct {
	output       strings.Builder
	dataSection  strings.Builder
	bssSection   strings.Builder
	rodataSection strings.Builder
	
	instructions []*IRInstruction
	stringLits   map[string]string
	globalVars   map[string]*Symbol
	
	currentFunc   string
	stackSize     int
	usedRegisters []int
	
	labelCounter  int
}

func NewCodeEmitter(instructions []*IRInstruction, stringLits map[string]string, globalVars map[string]*Symbol) *CodeEmitter {
	return &CodeEmitter{
		instructions:  instructions,
		stringLits:    stringLits,
		globalVars:    globalVars,
	}
}

func (ce *CodeEmitter) Emit() string {
	ce.emitDataSection()
	ce.emitBssSection()
	ce.emitTextSection()
	
	return ce.buildOutput()
}

func (ce *CodeEmitter) emitDataSection() {
	if len(ce.stringLits) == 0 {
		return
	}
	
	ce.rodataSection.WriteString("    .section .rodata\n")
	for label, str := range ce.stringLits {
		ce.rodataSection.WriteString(fmt.Sprintf("%s:\n", label))
		ce.rodataSection.WriteString(fmt.Sprintf("    .string \"%s\"\n", escapeString(str)))
	}
}

func (ce *CodeEmitter) emitBssSection() {
	if len(ce.globalVars) == 0 {
		return
	}
	
	ce.bssSection.WriteString("    .bss\n")
	for name, sym := range ce.globalVars {
		ce.bssSection.WriteString(fmt.Sprintf("    .comm %s,%d,%d\n", name, sym.Size, sym.Size))
	}
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
	
	// Emit function header
	ce.output.WriteString(fmt.Sprintf("\n    .globl %s\n", name))
	ce.output.WriteString(fmt.Sprintf("    .type %s, @function\n", name))
	ce.output.WriteString(fmt.Sprintf("%s:\n", name))
	
	// Prologue
	ce.output.WriteString("    pushq %rbp\n")
	ce.output.WriteString("    movq %rsp, %rbp\n")
	
	// Calculate stack size needed
	ce.stackSize = ce.calculateStackSize(*startIdx)
	if ce.stackSize > 0 {
		// Align to 16 bytes
		ce.stackSize = (ce.stackSize + 15) & ^15
		ce.output.WriteString(fmt.Sprintf("    subq $%d, %%rsp\n", ce.stackSize))
	}
	
	// Save callee-saved registers
	ce.emitRegisterSaves()
	
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
			ce.emitReturn()
			*startIdx++
			break
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
			if op != nil && op.Type == "mem" && op.Offset < 0 {
				offset := -op.Offset
				if offset > maxOffset {
					maxOffset = offset
				}
			}
		}
	}
	
	return maxOffset
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
		ce.output.WriteString(fmt.Sprintf("    testq %s, %s\n", ce.formatOperand(instr.Dst), ce.formatOperand(instr.Dst)))
		ce.output.WriteString(fmt.Sprintf("    sete %%al\n"))
		ce.output.WriteString(fmt.Sprintf("    movzbq %%al, %s\n", ce.formatOperand(instr.Dst)))
		
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
		
	case OpStore:
		ce.emitStore(instr.Dst, instr.Src1)
		
	case OpCall:
		ce.emitCall(instr)
		
	case OpJmp:
		ce.output.WriteString(fmt.Sprintf("    jmp %s\n", instr.Dst.Value))
		
	case OpJz:
		ce.output.WriteString(fmt.Sprintf("    testq %s, %s\n", ce.formatOperand(instr.Src1), ce.formatOperand(instr.Src1)))
		ce.output.WriteString(fmt.Sprintf("    jz %s\n", instr.Dst.Value))
		
	case OpJnz:
		ce.output.WriteString(fmt.Sprintf("    testq %s, %s\n", ce.formatOperand(instr.Src1), ce.formatOperand(instr.Src1)))
		ce.output.WriteString(fmt.Sprintf("    jnz %s\n", instr.Dst.Value))
		
	case OpLabel:
		ce.emitLabel(instr.Dst.Value)
		
	case OpParam:
		// Parameters handled in OpCall
		
	case OpPush:
		ce.output.WriteString(fmt.Sprintf("    pushq %s\n", ce.formatOperand(instr.Src1)))
		
	case OpPop:
		ce.output.WriteString(fmt.Sprintf("    popq %s\n", ce.formatOperand(instr.Dst)))
	}
}

func (ce *CodeEmitter) emitMov(dst, src *Operand) {
	if dst.Type == "reg" && src.Type == "reg" && dst.Value == src.Value {
		return // No-op
	}
	
	dstStr := ce.formatOperand(dst)
	srcStr := ce.formatOperand(src)
	
	// Handle label (string literals, addresses) - use leaq
	if src.Type == "label" {
		ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %s\n", src.Value, dstStr))
		return
	}
	
	// Handle immediate to memory
	if dst.Type == "mem" && src.Type == "imm" {
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", srcStr))
		ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
	} else if dst.Type == "mem" && src.Type == "mem" {
		// Memory to memory through register
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", srcStr))
		ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
	} else {
		ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcStr, dstStr))
	}
}

func (ce *CodeEmitter) emitBinaryOp(op string, dst, src1, src2 *Operand) {
	// Move src1 to dst
	ce.emitMov(dst, src1)
	
	// Apply operation
	src2Str := ce.formatOperand(src2)
	dstStr := ce.formatOperand(dst)
	
	if dst.Type == "mem" && src2.Type == "mem" {
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", src2Str))
		ce.output.WriteString(fmt.Sprintf("    %s %%rax, %s\n", op, dstStr))
	} else {
		ce.output.WriteString(fmt.Sprintf("    %s %s, %s\n", op, src2Str, dstStr))
	}
}

func (ce *CodeEmitter) emitMul(dst, src1, src2 *Operand) {
	ce.emitMov(dst, src1)
	
	src2Str := ce.formatOperand(src2)
	dstStr := ce.formatOperand(dst)
	
	ce.output.WriteString(fmt.Sprintf("    imulq %s, %s\n", src2Str, dstStr))
}

func (ce *CodeEmitter) emitDiv(dst, src1, src2 *Operand) {
	// Division requires RAX and RDX
	ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", ce.formatOperand(src1)))
	ce.output.WriteString("    cqto\n")
	ce.output.WriteString(fmt.Sprintf("    idivq %s\n", ce.formatOperand(src2)))
	ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", ce.formatOperand(dst)))
}

func (ce *CodeEmitter) emitMod(dst, src1, src2 *Operand) {
	// Modulo - result in RDX
	ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", ce.formatOperand(src1)))
	ce.output.WriteString("    cqto\n")
	ce.output.WriteString(fmt.Sprintf("    idivq %s\n", ce.formatOperand(src2)))
	ce.output.WriteString(fmt.Sprintf("    movq %%rdx, %s\n", ce.formatOperand(dst)))
}

func (ce *CodeEmitter) emitShift(op string, dst, src1, src2 *Operand) {
	ce.emitMov(dst, src1)
	
	// Shift amount must be in CL
	if src2.Type != "imm" {
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rcx\n", ce.formatOperand(src2)))
		ce.output.WriteString(fmt.Sprintf("    %s %%cl, %s\n", op, ce.formatOperand(dst)))
	} else {
		ce.output.WriteString(fmt.Sprintf("    %s %s, %s\n", op, ce.formatOperand(src2), ce.formatOperand(dst)))
	}
}

func (ce *CodeEmitter) emitComparison(setcc string, dst, src1, src2 *Operand) {
	ce.output.WriteString(fmt.Sprintf("    cmpq %s, %s\n", ce.formatOperand(src2), ce.formatOperand(src1)))
	ce.output.WriteString(fmt.Sprintf("    %s %%al\n", setcc))
	ce.output.WriteString(fmt.Sprintf("    movzbq %%al, %s\n", ce.formatOperand(dst)))
}

func (ce *CodeEmitter) emitLoad(dst, src *Operand) {
	switch src.Type {
	case "var":
		if src.IsGlobal {
			ce.output.WriteString(fmt.Sprintf("    movq %s(%%rip), %s\n", src.Value, ce.formatOperand(dst)))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movq %d(%%rbp), %s\n", src.Offset, ce.formatOperand(dst)))
		}
	case "array":
		// Load from array[index]: base(%rbp) + index_temp
		indexReg := ce.formatOperand(src.IndexTemp)
		dstReg := ce.formatOperand(dst)
		
		// Move index to r11 to avoid clobbering
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%r11\n", indexReg))
		
		if src.IsGlobal {
			// Global array: load from symbol + offset
			ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %%rax\n", src.Value))
			ce.output.WriteString(fmt.Sprintf("    movq (%%rax, %%r11, 1), %s\n", dstReg))
		} else {
			// Local array: load from rbp + base_offset + computed_offset
			ce.output.WriteString(fmt.Sprintf("    leaq %d(%%rbp), %%rax\n", src.Offset))
			ce.output.WriteString(fmt.Sprintf("    movq (%%rax, %%r11, 1), %s\n", dstReg))
		}
	case "addr":
		// Address-of: compute address and store in dst
		if src.IsGlobal {
			ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %s\n", src.Value, ce.formatOperand(dst)))
		} else {
			ce.output.WriteString(fmt.Sprintf("    leaq %d(%%rbp), %s\n", src.Offset, ce.formatOperand(dst)))
		}
	case "ptr":
		// Dereference: load from address in IndexTemp
		ptrReg := ce.formatOperand(src.IndexTemp)
		ce.output.WriteString(fmt.Sprintf("    movq (%s), %s\n", ptrReg, ce.formatOperand(dst)))
	case "label":
		// String literal or global label - use leaq to load address
		ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %s\n", src.Value, ce.formatOperand(dst)))
	default:
		ce.emitMov(dst, src)
	}
}

func (ce *CodeEmitter) emitStore(dst, src *Operand) {
	switch dst.Type {
	case "var":
		if dst.IsGlobal {
			if src.Type == "imm" {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", ce.formatOperand(src)))
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s(%%rip)\n", dst.Value))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %s(%%rip)\n", ce.formatOperand(src), dst.Value))
			}
		} else {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %d(%%rbp)\n", ce.formatOperand(src), dst.Offset))
		}
	case "array":
		// Store to array[index]: base(%rbp) + index_temp
		// IMPORTANT: Use separate registers for index and value!
		indexReg := ce.formatOperand(dst.IndexTemp)
		
		// Move index to r11 to avoid clobbering
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%r11\n", indexReg))
		
		// Get source value into rax
		srcReg := ce.formatOperand(src)
		if src.Type == "imm" {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", srcReg))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", srcReg))
		}
		
		if dst.IsGlobal {
			// Global array: store to symbol + offset
			ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %%rdx\n", dst.Value))
			ce.output.WriteString(fmt.Sprintf("    movq %%rax, (%%rdx, %%r11, 1)\n"))
		} else {
			// Local array: store to rbp + base_offset + computed_offset
			ce.output.WriteString(fmt.Sprintf("    leaq %d(%%rbp), %%rdx\n", dst.Offset))
			ce.output.WriteString(fmt.Sprintf("    movq %%rax, (%%rdx, %%r11, 1)\n"))
		}
	case "ptr":
		// Dereference store: store to address in IndexTemp
		ptrReg := ce.formatOperand(dst.IndexTemp)
		srcReg := ce.formatOperand(src)
		
		if src.Type == "imm" {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", srcReg))
			srcReg = "%rax"
		}
		
		ce.output.WriteString(fmt.Sprintf("    movq %s, (%s)\n", srcReg, ptrReg))
	default:
		ce.emitMov(dst, src)
	}
}

func (ce *CodeEmitter) emitCall(instr *IRInstruction) {
	// Arguments should already be in registers from OpMov instructions
	// Just need to align stack and call
	
	// Align stack to 16 bytes
	ce.output.WriteString("    andq $-16, %rsp\n")
	
	// Call
	ce.output.WriteString(fmt.Sprintf("    call %s\n", instr.Src1.Value))
	
	// Move result
	if instr.Dst != nil && instr.Dst.Value != "rax" {
		ce.emitMov(instr.Dst, &Operand{Type: "reg", Value: "rax"})
	}
}

func (ce *CodeEmitter) formatOperand(op *Operand) string {
	if op == nil {
		return ""
	}
	
	switch op.Type {
	case "reg":
		return "%" + op.Value
	case "imm":
		return "$" + op.Value
	case "label":
		return op.Value
	case "mem":
		if op.Offset == 0 {
			return "(%%rbp)"
		}
		return fmt.Sprintf("%d(%%rbp)", op.Offset)
	case "var":
		if op.IsGlobal {
			return op.Value + "(%rip)"
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
			return op.Value + "(%rip)"
		}
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
