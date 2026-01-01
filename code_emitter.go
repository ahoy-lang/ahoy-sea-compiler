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
	floatLits    map[string]string  // label -> float literal value
	
	currentFunc   string
	stackSize     int
	usedRegisters []int
	
	labelCounter  int
	floatCounter  int
}

func NewCodeEmitter(instructions []*IRInstruction, stringLits map[string]string, globalVars map[string]*Symbol) *CodeEmitter {
	return &CodeEmitter{
		instructions:  instructions,
		stringLits:    stringLits,
		globalVars:    globalVars,
		floatLits:     make(map[string]string),
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
	if len(ce.stringLits) == 0 && len(ce.floatLits) == 0 {
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
	
	// Calculate stack size needed (skip the label instruction itself)
	ce.stackSize = ce.calculateStackSize(*startIdx + 1)
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
		// Load address of variable/memory location
		dstStr := ce.formatOperand(instr.Dst)
		src1 := instr.Src1
		
		if src1.Type == "var" {
			if src1.IsGlobal {
				ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %s\n", src1.Value, dstStr))
			} else {
				ce.output.WriteString(fmt.Sprintf("    leaq %d(%%rbp), %s\n", src1.Offset, dstStr))
			}
		} else if src1.Type == "mem" {
			ce.output.WriteString(fmt.Sprintf("    leaq %d(%%rbp), %s\n", src1.Offset, dstStr))
		} else if src1.Type == "temp" {
			// Temp might have been allocated on stack - check allocator mapping
			// For now, try to use formatOperand but strip the dereference
			srcStr := ce.formatOperand(src1)
			// If srcStr is like offset(%rbp), use leaq with it
			if strings.Contains(srcStr, "(%rbp)") {
				ce.output.WriteString(fmt.Sprintf("    leaq %s, %s\n", srcStr, dstStr))
			} else {
				// Shouldn't happen - fallback to moving the value
				ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcStr, dstStr))
			}
		} else {
			// Fallback
			ce.output.WriteString(fmt.Sprintf("    leaq %s, %s\n", ce.formatOperand(src1), dstStr))
		}
		
	case OpStore:
		ce.emitStore(instr.Dst, instr.Src1)
		
	case OpCall:
		ce.emitCall(instr)
		
	case OpJmp:
		ce.output.WriteString(fmt.Sprintf("    jmp %s\n", instr.Dst.Value))
		
	case OpJz:
		src1Str := ce.formatOperand(instr.Src1)
		if strings.Contains(src1Str, "(") && strings.Contains(src1Str, ")") {
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", src1Str))
			ce.output.WriteString("    testq %rax, %rax\n")
		} else {
			ce.output.WriteString(fmt.Sprintf("    testq %s, %s\n", src1Str, src1Str))
		}
		ce.output.WriteString(fmt.Sprintf("    jz %s\n", instr.Dst.Value))
		
	case OpJnz:
		src1Str := ce.formatOperand(instr.Src1)
		if strings.Contains(src1Str, "(") && strings.Contains(src1Str, ")") {
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
	}
}

func (ce *CodeEmitter) emitMov(dst, src *Operand) {
	if dst.Type == "reg" && src.Type == "reg" && dst.Value == src.Value {
		return // No-op
	}
	
	dstStr := ce.formatOperand(dst)
	srcStr := ce.formatOperand(src)
	
	// Handle floating point immediate values
	if src.Type == "imm" && strings.Contains(src.Value, ".") {
		// It's a float literal - store in .rodata and load address
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
			ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %%rax\n", src.Value))
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
	   src.Type != "imm" && !strings.Contains(src.Value, ".") {
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
	
	// Handle immediate to memory
	if dst.Type == "mem" && src.Type == "imm" {
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", srcStr))
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
		// Float operation using SSE instructions
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
			// Move src1 to xmm0
			if src1.Type == "imm" {
				label := ce.getFloatLabel(src1.Value)
				ce.output.WriteString(fmt.Sprintf("    movsd %s(%%rip), %%xmm0\n", label))
			} else if src1.Type == "temp" || src1.Type == "reg" {
				// For GPRs, move as bit pattern first
				ce.output.WriteString(fmt.Sprintf("    movq %s, %%xmm0\n", ce.formatOperand(src1)))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movsd %s, %%xmm0\n", ce.formatOperand(src1)))
			}
			
			// Apply operation with src2
			if src2.Type == "imm" {
				label := ce.getFloatLabel(src2.Value)
				ce.output.WriteString(fmt.Sprintf("    %s %s(%%rip), %%xmm0\n", floatOp, label))
			} else if src2.Type == "temp" || src2.Type == "reg" {
				// For GPRs, move to xmm1 first
				ce.output.WriteString(fmt.Sprintf("    movq %s, %%xmm1\n", ce.formatOperand(src2)))
				ce.output.WriteString(fmt.Sprintf("    %s %%xmm1, %%xmm0\n", floatOp))
			} else {
				ce.output.WriteString(fmt.Sprintf("    %s %s, %%xmm0\n", floatOp, ce.formatOperand(src2)))
			}
			
			// Store result
			if dst.Type == "temp" || dst.Type == "reg" {
				ce.output.WriteString(fmt.Sprintf("    movq %%xmm0, %s\n", ce.formatOperand(dst)))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movsd %%xmm0, %s\n", ce.formatOperand(dst)))
			}
			return
		}
	}
	
	// Integer operation
	// Move src1 to dst
	ce.emitMov(dst, src1)
	
	// Apply operation - handle float immediates
	src2Str := ce.loadFloatIfNeeded(src2, "%r10")
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
		// Float multiplication using SSE instructions
		// Move src1 to xmm0
		if src1.Type == "imm" {
			label := ce.getFloatLabel(src1.Value)
			ce.output.WriteString(fmt.Sprintf("    movsd %s(%%rip), %%xmm0\n", label))
		} else if src1.Type == "temp" || src1.Type == "reg" {
			// For GPRs, move as bit pattern first
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%xmm0\n", ce.formatOperand(src1)))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movsd %s, %%xmm0\n", ce.formatOperand(src1)))
		}
		
		// Multiply by src2
		if src2.Type == "imm" {
			label := ce.getFloatLabel(src2.Value)
			ce.output.WriteString(fmt.Sprintf("    mulsd %s(%%rip), %%xmm0\n", label))
		} else if src2.Type == "temp" || src2.Type == "reg" {
			// For GPRs, move to xmm1 first
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%xmm1\n", ce.formatOperand(src2)))
			ce.output.WriteString(fmt.Sprintf("    mulsd %%xmm1, %%xmm0\n"))
		} else {
			ce.output.WriteString(fmt.Sprintf("    mulsd %s, %%xmm0\n", ce.formatOperand(src2)))
		}
		
		// Store result
		if dst.Type == "temp" || dst.Type == "reg" {
			ce.output.WriteString(fmt.Sprintf("    movq %%xmm0, %s\n", ce.formatOperand(dst)))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movsd %%xmm0, %s\n", ce.formatOperand(dst)))
		}
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
		// Float division using SSE instructions
		// Move src1 to xmm0
		if src1.Type == "imm" {
			label := ce.getFloatLabel(src1.Value)
			ce.output.WriteString(fmt.Sprintf("    movsd %s(%%rip), %%xmm0\n", label))
		} else if src1.Type == "temp" || src1.Type == "reg" {
			// For GPRs, move as bit pattern first
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%xmm0\n", ce.formatOperand(src1)))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movsd %s, %%xmm0\n", ce.formatOperand(src1)))
		}
		
		// Divide by src2
		if src2.Type == "imm" {
			label := ce.getFloatLabel(src2.Value)
			ce.output.WriteString(fmt.Sprintf("    divsd %s(%%rip), %%xmm0\n", label))
		} else if src2.Type == "temp" || src2.Type == "reg" {
			// For GPRs, move to xmm1 first
			ce.output.WriteString(fmt.Sprintf("    movq %s, %%xmm1\n", ce.formatOperand(src2)))
			ce.output.WriteString(fmt.Sprintf("    divsd %%xmm1, %%xmm0\n"))
		} else {
			ce.output.WriteString(fmt.Sprintf("    divsd %s, %%xmm0\n", ce.formatOperand(src2)))
		}
		
		// Store result
		if dst.Type == "temp" || dst.Type == "reg" {
			ce.output.WriteString(fmt.Sprintf("    movq %%xmm0, %s\n", ce.formatOperand(dst)))
		} else {
			ce.output.WriteString(fmt.Sprintf("    movsd %%xmm0, %s\n", ce.formatOperand(dst)))
		}
		return
	}
	
	// Division requires RAX and RDX
	// Check if we're working with 32-bit integers
	use32Bit := (src2.DataType == "int" || src2.DataType == "unsigned int" || src2.DataType == "unsigned" || 
	             strings.HasPrefix(src2.DataType, "enum "))
	
	if use32Bit {
		// 32-bit division
		ce.output.WriteString(fmt.Sprintf("    movl %s, %%eax\n", ce.formatOperand32(src1)))
		ce.output.WriteString("    cdq\n") // sign-extend EAX to EDX:EAX
		
		if src2.Type == "imm" {
			ce.output.WriteString(fmt.Sprintf("    movl %s, %%r11d\n", src2.Value))
			ce.output.WriteString("    idivl %r11d\n")
		} else {
			ce.output.WriteString(fmt.Sprintf("    idivl %s\n", ce.formatOperand32(src2)))
		}
		
		ce.output.WriteString(fmt.Sprintf("    movl %%eax, %s\n", ce.formatOperand32(dst)))
	} else {
		// 64-bit division (original code)
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", ce.formatOperand(src1)))
		ce.output.WriteString("    cqto\n")
		
		if src2.Type == "imm" {
			src2Str := ce.loadFloatIfNeeded(src2, "%r11")
			if src2Str == "%r11" {
				ce.output.WriteString("    idivq %r11\n")
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %%r11\n", src2Str))
				ce.output.WriteString("    idivq %r11\n")
			}
		} else {
			ce.output.WriteString(fmt.Sprintf("    idivq %s\n", ce.formatOperand(src2)))
		}
		
		ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", ce.formatOperand(dst)))
	}
}

func (ce *CodeEmitter) emitMod(dst, src1, src2 *Operand) {
	// Modulo - result in RDX
	// Check if we're working with 32-bit integers
	use32Bit := (src2.DataType == "int" || src2.DataType == "unsigned int" || src2.DataType == "unsigned" || 
	             strings.HasPrefix(src2.DataType, "enum "))
	
	if use32Bit {
		// 32-bit division
		ce.output.WriteString(fmt.Sprintf("    movl %s, %%eax\n", ce.formatOperand32(src1)))
		ce.output.WriteString("    cdq\n") // sign-extend EAX to EDX:EAX
		
		if src2.Type == "imm" {
			ce.output.WriteString(fmt.Sprintf("    movl %s, %%r11d\n", src2.Value))
			ce.output.WriteString("    idivl %r11d\n")
		} else {
			ce.output.WriteString(fmt.Sprintf("    idivl %s\n", ce.formatOperand32(src2)))
		}
		
		ce.output.WriteString(fmt.Sprintf("    movl %%edx, %s\n", ce.formatOperand32(dst)))
	} else {
		// 64-bit division (original code)
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", ce.formatOperand(src1)))
		ce.output.WriteString("    cqto\n")
		
		if src2.Type == "imm" {
			src2Str := ce.loadFloatIfNeeded(src2, "%r11")
			if src2Str == "%r11" {
				ce.output.WriteString("    idivq %r11\n")
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %%r11\n", src2Str))
				ce.output.WriteString("    idivq %r11\n")
			}
		} else {
			ce.output.WriteString(fmt.Sprintf("    idivq %s\n", ce.formatOperand(src2)))
		}
		
		ce.output.WriteString(fmt.Sprintf("    movq %%rdx, %s\n", ce.formatOperand(dst)))
	}
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

func (ce *CodeEmitter) emitComparison(setcc string, dst, src1, src2 *Operand) {
	src1Str := ce.formatOperand(src1)
	src2Str := ce.formatOperand(src2)
	
	// Handle float immediates - they need to be in .rodata
	if src2.Type == "imm" && (src2.DataType == "float" || src2.DataType == "double") {
		label := ce.getFloatLabel(src2.Value)
		// Load into a register for comparison
		ce.output.WriteString(fmt.Sprintf("    movq %s(%%rip), %%r10\n", label))
		src2Str = "%r10"
	}
	
	src1IsMem := strings.Contains(src1Str, "(") && strings.Contains(src1Str, ")")
	src2IsMem := strings.Contains(src2Str, "(") && strings.Contains(src2Str, ")")
	
	if src1IsMem && src2IsMem {
		// Both are memory - load one into register
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", src1Str))
		ce.output.WriteString(fmt.Sprintf("    cmpq %s, %%rax\n", src2Str))
	} else {
		ce.output.WriteString(fmt.Sprintf("    cmpq %s, %s\n", src2Str, src1Str))
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
					if src.Size == 4 {
						// Load 32-bit and zero-extend
						ce.output.WriteString(fmt.Sprintf("    movl %s(%%rip), %%eax\n", src.Value))
					} else if src.Size == 2 {
						ce.output.WriteString(fmt.Sprintf("    movzwl %s(%%rip), %%eax\n", src.Value))
					} else if src.Size == 1 {
						ce.output.WriteString(fmt.Sprintf("    movzbl %s(%%rip), %%eax\n", src.Value))
					}
				} else {
					if src.Size == 4 {
						ce.output.WriteString(fmt.Sprintf("    movl %d(%%rbp), %%eax\n", src.Offset))
					} else if src.Size == 2 {
						ce.output.WriteString(fmt.Sprintf("    movzwl %d(%%rbp), %%eax\n", src.Offset))
					} else if src.Size == 1 {
						ce.output.WriteString(fmt.Sprintf("    movzbl %d(%%rbp), %%eax\n", src.Offset))
					}
				}
				// movl to %eax zeros upper 32 bits of %rax
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
			} else {
				// Default 8-byte load
				if src.IsGlobal {
					ce.output.WriteString(fmt.Sprintf("    movq %s(%%rip), %%rax\n", src.Value))
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
					if src.Size == 4 {
						ce.output.WriteString(fmt.Sprintf("    movl %s(%%rip), %s\n", src.Value, dstStr32))
					} else if src.Size == 2 {
						ce.output.WriteString(fmt.Sprintf("    movzwl %s(%%rip), %s\n", src.Value, dstStr32))
					} else if src.Size == 1 {
						ce.output.WriteString(fmt.Sprintf("    movzbl %s(%%rip), %s\n", src.Value, dstStr32))
					}
				} else {
					// Local variable load
					dstStr32 := ce.formatOperand32(dst)
					if src.Size == 4 {
						ce.output.WriteString(fmt.Sprintf("    movl %d(%%rbp), %s\n", src.Offset, dstStr32))
					} else if src.Size == 2 {
						ce.output.WriteString(fmt.Sprintf("    movzwl %d(%%rbp), %s\n", src.Offset, dstStr32))
					} else if src.Size == 1 {
						ce.output.WriteString(fmt.Sprintf("    movzbl %d(%%rbp), %s\n", src.Offset, dstStr32))
					}
				}
			} else {
				// Default 8-byte load
				if src.IsGlobal {
					ce.output.WriteString(fmt.Sprintf("    movq %s(%%rip), %s\n", src.Value, dstStr))
				} else {
					ce.output.WriteString(fmt.Sprintf("    movq %d(%%rbp), %s\n", src.Offset, dstStr))
				}
			}
		}
	case "array":
		// Load from array[index]: base(%rbp) + index_temp
		indexReg := ce.formatOperand(src.IndexTemp)
		dstReg := ce.formatOperand(dst)
		
		// Move index to r11 to avoid clobbering
		ce.output.WriteString(fmt.Sprintf("    movq %s, %%r11\n", indexReg))
		
		if src.IsGlobal {
			// Global array: load from symbol + offset
			ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %%rdx\n", src.Value))
			ce.output.WriteString(fmt.Sprintf("    movq (%%rdx, %%r11, 1), %s\n", dstReg))
		} else {
			// Local array: load from rbp + base_offset + computed_offset
			ce.output.WriteString(fmt.Sprintf("    leaq %d(%%rbp), %%rdx\n", src.Offset))
			ce.output.WriteString(fmt.Sprintf("    movq (%%rdx, %%r11, 1), %s\n", dstReg))
		}
	case "addr":
		// Address-of: compute address and store in dst
		dstStr := ce.formatOperand(dst)
		dstIsMem := strings.Contains(dstStr, "(") && strings.Contains(dstStr, ")")
		
		if dstIsMem {
			// Destination is memory, go through rax
			if src.IsGlobal {
				ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %%rax\n", src.Value))
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
		if dataType == "char" || dataType == "signed char" || dataType == "unsigned char" {
			movInstr = "movb"
		} else if dataType == "short" || dataType == "short int" || dataType == "signed short" || dataType == "unsigned short" {
			movInstr = "movw"
		} else if dataType == "int" || dataType == "signed int" || dataType == "unsigned int" {
			movInstr = "movl"
		}
		
		if dstIsMem {
			if movInstr == "movb" {
				ce.output.WriteString(fmt.Sprintf("    movb (%s), %%al\n", ptrReg))
				ce.output.WriteString("    movzbq %al, %rax\n")  // Zero-extend to 64-bit
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
			} else if movInstr == "movw" {
				ce.output.WriteString(fmt.Sprintf("    movzwl (%s), %%eax\n", ptrReg))
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
			if movInstr == "movb" {
				ce.output.WriteString(fmt.Sprintf("    movzbl (%s), %%eax\n", ptrReg))
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
			} else if movInstr == "movw" {
				ce.output.WriteString(fmt.Sprintf("    movzwl (%s), %%eax\n", ptrReg))
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s\n", dstStr))
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
			ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %%rax\n", src.Value))
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

func (ce *CodeEmitter) emitStore(dst, src *Operand) {
	switch dst.Type {
	case "var":
		// Special handling for label sources (string literals)
		if src.Type == "label" {
			if dst.IsGlobal {
				ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %%rax\n", src.Value))
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s(%%rip)\n", dst.Value))
			} else {
				ce.output.WriteString(fmt.Sprintf("    leaq %s(%%rip), %%rax\n", src.Value))
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
				ce.output.WriteString(fmt.Sprintf("    movq %%rax, %s(%%rip)\n", dst.Value))
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %s(%%rip)\n", srcStr, dst.Value))
			}
		} else {
			if srcIsMem || src.Type == "imm" {
				// Handle float immediates
				loadedStr := ce.loadFloatIfNeeded(src, "%rax")
				if loadedStr != "%rax" {
					ce.output.WriteString(fmt.Sprintf("    movq %s, %%rax\n", loadedStr))
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
					// For integer immediates, use movl to avoid garbage in upper bytes
					if src.Type == "imm" && !strings.Contains(src.Value, ".") {
						// Use 32-bit mov for integer immediates
						ce.output.WriteString(fmt.Sprintf("    movl $%s, %%eax\n", src.Value))
						ce.output.WriteString(fmt.Sprintf("    movq %%rax, %d(%%rbp)\n", dst.Offset))
					} else {
						ce.output.WriteString(fmt.Sprintf("    movq %%rax, %d(%%rbp)\n", dst.Offset))
					}
				}
			} else {
				ce.output.WriteString(fmt.Sprintf("    movq %s, %d(%%rbp)\n", srcStr, dst.Offset))
			}
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
	// Convert integer immediates to float format
	floatVal := value
	if !strings.Contains(floatVal, ".") {
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
		// Moving to float register
		switch src.Type {
		case "imm":
			// Float immediate - load from .rodata
			label := ce.getFloatLabel(src.Value)
			ce.output.WriteString(fmt.Sprintf("    movsd %s(%%rip), %s\n", label, dstStr))
		case "temp", "reg":
			srcStr := ce.formatOperand(src)
			if strings.Contains(srcStr, "(") && strings.Contains(srcStr, ")") {
				// Source is in memory
				ce.output.WriteString(fmt.Sprintf("    movsd %s, %s\n", srcStr, dstStr))
			} else {
				// Source is in a GPR - use movq to move bitwise
				ce.output.WriteString(fmt.Sprintf("    movq %s, %s\n", srcStr, dstStr))
			}
		case "mem", "var":
			srcStr := ce.formatOperand(src)
			ce.output.WriteString(fmt.Sprintf("    movsd %s, %s\n", srcStr, dstStr))
		default:
			srcStr := ce.formatOperand(src)
			ce.output.WriteString(fmt.Sprintf("    movsd %s, %s\n", srcStr, dstStr))
		}
	} else {
		// Moving to integer register
		switch src.Type {
		case "imm":
			ce.output.WriteString(fmt.Sprintf("    movq $%s, %s\n", src.Value, dstStr))
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

func (ce *CodeEmitter) emitCall(instr *IRInstruction) {
	// Arguments should already be in registers from OpMov instructions
	// Stack alignment should be handled in function prologue, not here
	
	// Call
	ce.output.WriteString(fmt.Sprintf("    call %s\n", instr.Src1.Value))
	
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
