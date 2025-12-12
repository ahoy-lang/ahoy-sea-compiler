package main

import (
	"fmt"
	"strconv"
	"strings"
)

// x86-64 Assembler - Encodes assembly instructions to machine code
type Assembler struct {
	code         []byte
	symbols      map[string]uint64
	relocations  []Relocation
	labelTargets map[string]int
	currentAddr  uint64
}

type Relocation struct {
	Type   RelocationType
	Offset uint64
	Symbol string
	Addend int64
}

type RelocationType int

const (
	R_X86_64_NONE RelocationType = iota
	R_X86_64_64
	R_X86_64_PC32
	R_X86_64_PLT32
	R_X86_64_GOTPCREL
)

// Register encoding
const (
	REG_RAX = 0
	REG_RCX = 1
	REG_RDX = 2
	REG_RBX = 3
	REG_RSP = 4
	REG_RBP = 5
	REG_RSI = 6
	REG_RDI = 7
	REG_R8  = 8
	REG_R9  = 9
	REG_R10 = 10
	REG_R11 = 11
	REG_R12 = 12
	REG_R13 = 13
	REG_R14 = 14
	REG_R15 = 15
)

var regNameToCode = map[string]int{
	"rax": REG_RAX, "eax": REG_RAX, "al": REG_RAX,
	"rcx": REG_RCX, "ecx": REG_RCX, "cl": REG_RCX,
	"rdx": REG_RDX, "edx": REG_RDX, "dl": REG_RDX,
	"rbx": REG_RBX, "ebx": REG_RBX, "bl": REG_RBX,
	"rsp": REG_RSP, "esp": REG_RSP,
	"rbp": REG_RBP, "ebp": REG_RBP,
	"rsi": REG_RSI, "esi": REG_RSI,
	"rdi": REG_RDI, "edi": REG_RDI,
	"r8":  REG_R8,  "r8d": REG_R8,  "r8b": REG_R8,
	"r9":  REG_R9,  "r9d": REG_R9,  "r9b": REG_R9,
	"r10": REG_R10, "r10d": REG_R10,
	"r11": REG_R11, "r11d": REG_R11,
	"r12": REG_R12, "r12d": REG_R12,
	"r13": REG_R13, "r13d": REG_R13,
	"r14": REG_R14, "r14d": REG_R14,
	"r15": REG_R15, "r15d": REG_R15,
}

func NewAssembler() *Assembler {
	return &Assembler{
		code:         make([]byte, 0, 4096),
		symbols:      make(map[string]uint64),
		relocations:  make([]Relocation, 0),
		labelTargets: make(map[string]int),
		currentAddr:  0,
	}
}

func (a *Assembler) AssembleText(asmText string) ([]byte, error) {
	lines := strings.Split(asmText, "\n")
	
	// Debug: print input
	debugMode := false  // Disable debug
	if debugMode {
		fmt.Printf("=== ASSEMBLER INPUT (%d bytes) ===\n%s\n=== END INPUT ===\n", len(asmText), asmText)
	}
	
	// First pass: collect labels
	offset := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		
		if strings.HasSuffix(line, ":") {
			label := strings.TrimSuffix(line, ":")
			a.labelTargets[label] = offset
			a.symbols[label] = uint64(offset)
			continue
		}
		
		if strings.HasPrefix(line, ".") {
			continue
		}
		
		size := a.estimateInstructionSize(line)
		offset += size
	}
	
	// Debug output (can be removed later)
	if debugMode {
		fmt.Printf("After first pass: expected size = %d\n", offset)
	}
	
	// Second pass: encode instructions
	instructionCount := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasSuffix(line, ":") {
			continue
		}
		
		if strings.HasPrefix(line, ".") {
			continue
		}
		
		instructionCount++
		beforeSize := len(a.code)
		err := a.encodeInstruction(line)
		if err != nil {
			return nil, fmt.Errorf("failed to encode '%s': %w", line, err)
		}
		
		if debugMode {
			fmt.Printf("#%d Encoded '%s': %d bytes (total now: %d)\n", instructionCount, line, len(a.code)-beforeSize, len(a.code))
		}
	}
	
	if debugMode {
		fmt.Printf("Final code size: %d bytes\n", len(a.code))
	}
	
	return a.code, nil
}

func (a *Assembler) estimateInstructionSize(line string) int {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return 0
	}
	
	mnemonic := parts[0]
	
	switch mnemonic {
	case "pushq", "popq":
		return 2
	case "ret", "nop":
		return 1
	case "call", "jmp", "je", "jne", "jl", "jle", "jg", "jge":
		return 5
	case "movq", "addq", "subq", "imulq", "cmpq":
		return 8
	case "idivq":
		return 3
	case "cqto":
		return 1
	default:
		return 8
	}
}

func (a *Assembler) encodeInstruction(line string) error {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil
	}
	
	mnemonic := parts[0]
	
	switch mnemonic {
	case "pushq":
		return a.encodePush(parts[1:])
	case "popq":
		return a.encodePop(parts[1:])
	case "movq":
		return a.encodeMov(parts[1:])
	case "addq":
		return a.encodeAdd(parts[1:])
	case "subq":
		return a.encodeSub(parts[1:])
	case "imulq":
		return a.encodeImul(parts[1:])
	case "idivq":
		return a.encodeIdiv(parts[1:])
	case "cmpq":
		return a.encodeCmp(parts[1:])
	case "andq":
		return a.encodeAnd(parts[1:])
	case "orq":
		return a.encodeOr(parts[1:])
	case "xorq":
		return a.encodeXor(parts[1:])
	case "ret":
		a.emit(0xC3)
		return nil
	case "syscall":
		a.emit(0x0F, 0x05)
		return nil
	case "nop":
		a.emit(0x90)
		return nil
	case "cqto":
		a.emit(0x48, 0x99)
		return nil
	case "call":
		return a.encodeCall(parts[1:])
	case "jmp":
		return a.encodeJmp(parts[1:])
	case "je", "jz":
		return a.encodeJe(parts[1:])
	case "jne", "jnz":
		return a.encodeJne(parts[1:])
	case "jl":
		return a.encodeJl(parts[1:])
	case "jle":
		return a.encodeJle(parts[1:])
	case "jg":
		return a.encodeJg(parts[1:])
	case "jge":
		return a.encodeJge(parts[1:])
	case "sete", "setz":
		return a.encodeSetCC(0x94, parts[1:])
	case "setne", "setnz":
		return a.encodeSetCC(0x95, parts[1:])
	case "setl":
		return a.encodeSetCC(0x9C, parts[1:])
	case "setle":
		return a.encodeSetCC(0x9E, parts[1:])
	case "setg":
		return a.encodeSetCC(0x9F, parts[1:])
	case "setge":
		return a.encodeSetCC(0x9D, parts[1:])
	case "movzbq":
		return a.encodeMovzbq(parts[1:])
	case "testq":
		return a.encodeTest(parts[1:])
	case "leaq":
		return a.encodeLea(parts[1:])
	case "salq", "shlq":
		return a.encodeShift(0xE0, parts[1:]) // SAL/SHL use /4
	case "sarq":
		return a.encodeShift(0xF8, parts[1:]) // SAR uses /7
	case "shrq":
		return a.encodeShift(0xE8, parts[1:]) // SHR uses /5
	case "negq":
		return a.encodeNeg(parts[1:])
	default:
		return fmt.Errorf("unsupported mnemonic: %s", mnemonic)
	}
}

func (a *Assembler) encodePush(operands []string) error {
	if len(operands) != 1 {
		return fmt.Errorf("push requires 1 operand")
	}
	
	reg := parseRegister(operands[0])
	if reg == -1 {
		return fmt.Errorf("invalid register: %s", operands[0])
	}
	
	if reg >= 8 {
		a.emit(0x41)
	}
	a.emit(0x50 + byte(reg&7))
	return nil
}

func (a *Assembler) encodePop(operands []string) error {
	if len(operands) != 1 {
		return fmt.Errorf("pop requires 1 operand")
	}
	
	reg := parseRegister(operands[0])
	if reg == -1 {
		return fmt.Errorf("invalid register: %s", operands[0])
	}
	
	if reg >= 8 {
		a.emit(0x41)
	}
	a.emit(0x58 + byte(reg&7))
	return nil
}

func (a *Assembler) encodeMov(operands []string) error {
	if len(operands) != 2 {
		return fmt.Errorf("mov requires 2 operands")
	}
	
	src := strings.TrimSuffix(strings.TrimSpace(operands[0]), ",")
	dst := strings.TrimSpace(operands[1])
	
	// Check for immediate to register or memory
	if strings.HasPrefix(src, "$") {
		imm, err := parseImmediate(src)
		if err != nil {
			return err
		}
		
		dstReg := parseRegister(dst)
		if dstReg != -1 {
			// Destination is register
			// REX.W prefix for 64-bit
			rex := byte(0x48)
			if dstReg >= 8 {
				rex |= 0x01
			}
			a.emit(rex)
			
			// Check if small immediate (can use sign-extended 32-bit)
			if imm >= -2147483648 && imm <= 2147483647 {
				a.emit(0xC7)
				a.emit(0xC0 + byte(dstReg&7))
				a.emitInt32(int32(imm))
			} else {
				a.emit(0xB8 + byte(dstReg&7))
				a.emitInt64(imm)
			}
			return nil
		}
		
		// Check if destination is memory
		if strings.Contains(dst, "(%") && strings.HasSuffix(dst, ")") {
			// Parse memory operand
			memStr := dst
			lastPct := strings.LastIndex(memStr, "%")
			if lastPct == -1 {
				return fmt.Errorf("invalid memory operand: %s", dst)
			}
			baseRegStr := memStr[lastPct:]
			baseRegStr = strings.TrimSuffix(baseRegStr, ")")
			baseReg := parseRegister(baseRegStr)
			if baseReg == -1 {
				return fmt.Errorf("invalid base register in: %s", dst)
			}
			
			offsetStr := strings.TrimPrefix(memStr[:lastPct], "(")
			offsetStr = strings.TrimSuffix(offsetStr, "(")
			offset := int32(0)
			if offsetStr != "" {
				val, err := strconv.ParseInt(offsetStr, 10, 32)
				if err != nil {
					return fmt.Errorf("invalid offset in: %s", dst)
				}
				offset = int32(val)
			}
			
			// movq $imm, offset(%base)
			rex := byte(0x48)
			if baseReg >= 8 {
				rex |= 0x01 // REX.B
				baseReg -= 8
			}
			a.emit(rex, 0xC7)
			
			// ModR/M for memory (opcode extension /0)
			if offset == 0 && (baseReg&7) != 5 {
				modrm := byte(0x00) | byte(baseReg&7)
				a.emit(modrm)
				if (baseReg & 7) == 4 {
					a.emit(0x24)
				}
			} else if offset >= -128 && offset <= 127 {
				modrm := byte(0x40) | byte(baseReg&7)
				a.emit(modrm)
				if (baseReg & 7) == 4 {
					a.emit(0x24)
				}
				a.emit(byte(offset))
			} else {
				modrm := byte(0x80) | byte(baseReg&7)
				a.emit(modrm)
				if (baseReg & 7) == 4 {
					a.emit(0x24)
				}
				a.emitInt32(offset)
			}
			a.emitInt32(int32(imm))
			return nil
		}
		
		return fmt.Errorf("invalid destination register: %s", dst)
	}
	
	// Register to register
	srcReg := parseRegister(src)
	dstReg := parseRegister(dst)
	
	if srcReg != -1 && dstReg != -1 {
		// REX.W prefix
		rex := byte(0x48)
		if srcReg >= 8 {
			rex |= 0x04
		}
		if dstReg >= 8 {
			rex |= 0x01
		}
		a.emit(rex)
		
		a.emit(0x89)
		modrm := byte(0xC0) | byte((srcReg&7)<<3) | byte(dstReg&7)
		a.emit(modrm)
		return nil
	}
	
	// Check for memory-to-memory move (offset(%reg) to offset(%reg))
	// Register indirect (%reg) is OK as source or dest
	// RIP-relative (%rip) is also special
	srcHasOffset := strings.Contains(src, "(%") && !strings.HasPrefix(src, "(") && !strings.Contains(src, "(%rip)")
	dstHasOffset := strings.Contains(dst, "(%") && !strings.HasPrefix(dst, "(") && !strings.Contains(dst, "(%rip)")
	
	if srcHasOffset && dstHasOffset {
		return fmt.Errorf("memory-to-memory move not supported: movq %s, %s (code generator should split this)", src, dst)
	}
	
	// Check for RIP-relative addressing: symbol(%rip)
	if strings.Contains(src, "(%rip)") {
		// movq symbol(%rip), %reg or movq symbol(%rip), mem
		symbol := strings.TrimSuffix(src, "(%rip)")
		symbol = strings.TrimSpace(symbol)
		
		if dstReg != -1 {
			// Destination is register
			rex := byte(0x48)
			if dstReg >= 8 {
				rex |= 0x04 // REX.R
				dstReg -= 8
			}
			
			a.emit(rex, 0x8B)
			// ModR/M: 00 reg 101 (RIP-relative)
			modrm := byte(0x05) | byte((dstReg&7)<<3)
			a.emit(modrm)
			
			// Add relocation
			a.relocations = append(a.relocations, Relocation{
				Type:   R_X86_64_PC32,
				Offset: uint64(len(a.code)),
				Symbol: symbol,
				Addend: -4,
			})
			a.emitInt32(0)
			return nil
		}
		
		// Destination is memory - not directly supported, but we can suggest split
		return fmt.Errorf("movq %%rip, mem not directly supported - split into: movq %s, %%rax; movq %%rax, %s", src, dst)
	}
	
	if strings.Contains(dst, "(%rip)") && srcReg != -1 {
		// movq %reg, symbol(%rip) - store to RIP-relative address
		symbol := strings.TrimSuffix(dst, "(%rip)")
		symbol = strings.TrimSpace(symbol)
		
		rex := byte(0x48)
		if srcReg >= 8 {
			rex |= 0x04 // REX.R
			srcReg -= 8
		}
		
		a.emit(rex, 0x89)
		// ModR/M: 00 reg 101 (RIP-relative)
		modrm := byte(0x05) | byte((srcReg&7)<<3)
		a.emit(modrm)
		
		// Add relocation
		a.relocations = append(a.relocations, Relocation{
			Type:   R_X86_64_PC32,
			Offset: uint64(len(a.code)),
			Symbol: symbol,
			Addend: -4,
		})
		a.emitInt32(0)
		return nil
	}
	
	// Check for memory addressing: (%reg) or offset(%reg)
	if strings.Contains(dst, "(%") && strings.HasSuffix(dst, ")") {
		// movq %src, (%dst) or movq %src, offset(%dst) or movq mem, (%dst)
		
		// If source is also memory, this is a special case we'll handle
		// For movq mem, (%reg), we can't encode directly - would need to be split
		// But let's be helpful and provide better guidance
		if srcReg == -1 {
			if strings.Contains(src, "(%") {
				// Both are memory - definitely not encodable
				return fmt.Errorf("movq mem, mem not supported - split into: movq %s, %%rax; movq %%rax, %s", src, dst)
			}
			// Source is immediate or label
			return fmt.Errorf("source must be register for memory store: %s to %s", src, dst)
		}
		
		// Parse destination: (%reg) or offset(%reg)
		memStr := dst
		// Extract base register (everything after the last %)
		lastPct := strings.LastIndex(memStr, "%")
		if lastPct == -1 {
			return fmt.Errorf("invalid memory operand: %s", dst)
		}
		baseRegStr := memStr[lastPct:]
		baseRegStr = strings.TrimSuffix(baseRegStr, ")")
		baseReg := parseRegister(baseRegStr)
		if baseReg == -1 {
			return fmt.Errorf("invalid base register in: %s", dst)
		}
		
		// Extract offset (everything before the %)
		offsetStr := strings.TrimPrefix(memStr[:lastPct], "(")
		offsetStr = strings.TrimSuffix(offsetStr, "(")
		offset := int32(0)
		if offsetStr != "" {
			val, err := strconv.ParseInt(offsetStr, 10, 32)
			if err != nil {
				return fmt.Errorf("invalid offset in: %s (offset: %s)", dst, offsetStr)
			}
			offset = int32(val)
		}
		
		// REX.W prefix
		rex := byte(0x48)
		if srcReg >= 8 {
			rex |= 0x04 // REX.R
		}
		if baseReg >= 8 {
			rex |= 0x01 // REX.B
		}
		a.emit(rex, 0x89)
		
		// ModR/M byte
		if offset == 0 && (baseReg&7) != 5 { // Not RBP
			// [base] - mod=00
			modrm := byte(0x00) | byte((srcReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 { // RSP needs SIB
				a.emit(0x24) // SIB: scale=0, index=none, base=RSP
			}
		} else if offset >= -128 && offset <= 127 {
			// [base+disp8] - mod=01
			modrm := byte(0x40) | byte((srcReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 { // RSP needs SIB
				a.emit(0x24)
			}
			a.emit(byte(offset))
		} else {
			// [base+disp32] - mod=10
			modrm := byte(0x80) | byte((srcReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 { // RSP needs SIB
				a.emit(0x24)
			}
			a.emitInt32(offset)
		}
		return nil
	}
	
	if strings.Contains(src, "(%") && strings.HasSuffix(src, ")") {
		// movq (%src), %dst or movq offset(%src), %dst
		if dstReg == -1 {
			return fmt.Errorf("destination must be register for memory load")
		}
		
		// Parse source: (%reg) or offset(%reg)
		memStr := src
		// Extract base register (everything after the last %)
		lastPct := strings.LastIndex(memStr, "%")
		if lastPct == -1 {
			return fmt.Errorf("invalid memory operand: %s", src)
		}
		baseRegStr := memStr[lastPct:]
		baseRegStr = strings.TrimSuffix(baseRegStr, ")")
		baseReg := parseRegister(baseRegStr)
		if baseReg == -1 {
			return fmt.Errorf("invalid base register in: %s", src)
		}
		
		// Extract offset (everything before the %)
		offsetStr := strings.TrimPrefix(memStr[:lastPct], "(")
		offsetStr = strings.TrimSuffix(offsetStr, "(")
		offset := int32(0)
		if offsetStr != "" {
			val, err := strconv.ParseInt(offsetStr, 10, 32)
			if err != nil {
				return fmt.Errorf("invalid offset in: %s (offset: %s)", src, offsetStr)
			}
			offset = int32(val)
		}
		
		// REX.W prefix
		rex := byte(0x48)
		if dstReg >= 8 {
			rex |= 0x04 // REX.R
		}
		if baseReg >= 8 {
			rex |= 0x01 // REX.B
		}
		a.emit(rex, 0x8B)
		
		// ModR/M byte
		if offset == 0 && (baseReg&7) != 5 { // Not RBP
			// [base] - mod=00
			modrm := byte(0x00) | byte((dstReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 { // RSP needs SIB
				a.emit(0x24)
			}
		} else if offset >= -128 && offset <= 127 {
			// [base+disp8] - mod=01
			modrm := byte(0x40) | byte((dstReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 { // RSP needs SIB
				a.emit(0x24)
			}
			a.emit(byte(offset))
		} else {
			// [base+disp32] - mod=10
			modrm := byte(0x80) | byte((dstReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 { // RSP needs SIB
				a.emit(0x24)
			}
			a.emitInt32(offset)
		}
		return nil
	}
	
	return fmt.Errorf("unsupported mov operands: %s, %s", src, dst)
}

func (a *Assembler) encodeAdd(operands []string) error {
	return a.encodeALU(0x01, 0x81, 0, operands)
}

func (a *Assembler) encodeSub(operands []string) error {
	return a.encodeALU(0x29, 0x81, 5, operands)
}

func (a *Assembler) encodeImul(operands []string) error {
	if len(operands) != 2 {
		return fmt.Errorf("imul requires 2 operands")
	}
	
	src := strings.TrimSuffix(strings.TrimSpace(operands[0]), ",")
	dst := strings.TrimSpace(operands[1])
	
	// Check for immediate source: imulq $imm, %reg
	if strings.HasPrefix(src, "$") {
		dstReg := parseRegister(dst)
		if dstReg == -1 {
			return fmt.Errorf("destination must be register for imul")
		}
		
		imm, err := parseImmediate(src)
		if err != nil {
			return err
		}
		
		rex := byte(0x48)
		if dstReg >= 8 {
			rex |= 0x05 // REX.RB (dst is both src and dst)
			dstReg -= 8
		}
		a.emit(rex)
		
		// Use imul r64, r64, imm32 form (opcode 0x69)
		if imm >= -128 && imm <= 127 {
			// Sign-extended imm8
			a.emit(0x6B)
			modrm := byte(0xC0) | byte((dstReg&7)<<3) | byte(dstReg&7)
			a.emit(modrm)
			a.emit(byte(imm))
		} else {
			// imm32
			a.emit(0x69)
			modrm := byte(0xC0) | byte((dstReg&7)<<3) | byte(dstReg&7)
			a.emit(modrm)
			a.emitInt32(int32(imm))
		}
		return nil
	}
	
	srcReg := parseRegister(src)
	dstReg := parseRegister(dst)
	
	// Check for imulq mem, %reg
	if srcReg == -1 && dstReg != -1 && strings.Contains(src, "(%") && strings.HasSuffix(src, ")") {
		// Parse memory operand
		memStr := src
		lastPct := strings.LastIndex(memStr, "%")
		if lastPct == -1 {
			return fmt.Errorf("invalid memory operand: %s", src)
		}
		baseRegStr := memStr[lastPct:]
		baseRegStr = strings.TrimSuffix(baseRegStr, ")")
		baseReg := parseRegister(baseRegStr)
		if baseReg == -1 {
			return fmt.Errorf("invalid base register in: %s", src)
		}
		
		offsetStr := strings.TrimPrefix(memStr[:lastPct], "(")
		offsetStr = strings.TrimSuffix(offsetStr, "(")
		offset := int32(0)
		if offsetStr != "" {
			val, err := strconv.ParseInt(offsetStr, 10, 32)
			if err != nil {
				return fmt.Errorf("invalid offset in: %s", src)
			}
			offset = int32(val)
		}
		
		// imulq mem, %reg uses opcode 0x0F 0xAF
		rex := byte(0x48)
		if dstReg >= 8 {
			rex |= 0x04 // REX.R
			dstReg -= 8
		}
		if baseReg >= 8 {
			rex |= 0x01 // REX.B
			baseReg -= 8
		}
		a.emit(rex, 0x0F, 0xAF)
		
		// ModR/M
		if offset == 0 && (baseReg&7) != 5 {
			modrm := byte(0x00) | byte((dstReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 {
				a.emit(0x24)
			}
		} else if offset >= -128 && offset <= 127 {
			modrm := byte(0x40) | byte((dstReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 {
				a.emit(0x24)
			}
			a.emit(byte(offset))
		} else {
			modrm := byte(0x80) | byte((dstReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 {
				a.emit(0x24)
			}
			a.emitInt32(offset)
		}
		return nil
	}
	
	if srcReg == -1 || dstReg == -1 {
		return fmt.Errorf("imul requires register operands")
	}
	
	rex := byte(0x48)
	if dstReg >= 8 {
		rex |= 0x04
	}
	if srcReg >= 8 {
		rex |= 0x01
	}
	a.emit(rex)
	
	a.emit(0x0F, 0xAF)
	modrm := byte(0xC0) | byte((dstReg&7)<<3) | byte(srcReg&7)
	a.emit(modrm)
	return nil
}

func (a *Assembler) encodeIdiv(operands []string) error {
	if len(operands) != 1 {
		return fmt.Errorf("idiv requires 1 operand")
	}
	
	op := strings.TrimSpace(operands[0])
	reg := parseRegister(op)
	
	if reg == -1 {
		// Check for memory operand
		if strings.Contains(op, "(%") && strings.HasSuffix(op, ")") {
			// Parse memory operand
			memStr := op
			lastPct := strings.LastIndex(memStr, "%")
			if lastPct == -1 {
				return fmt.Errorf("invalid memory operand: %s", op)
			}
			baseRegStr := memStr[lastPct:]
			baseRegStr = strings.TrimSuffix(baseRegStr, ")")
			baseReg := parseRegister(baseRegStr)
			if baseReg == -1 {
				return fmt.Errorf("invalid base register in: %s", op)
			}
			
			offsetStr := strings.TrimPrefix(memStr[:lastPct], "(")
			offsetStr = strings.TrimSuffix(offsetStr, "(")
			offset := int32(0)
			if offsetStr != "" {
				val, err := strconv.ParseInt(offsetStr, 10, 32)
				if err != nil {
					return fmt.Errorf("invalid offset in: %s", op)
				}
				offset = int32(val)
			}
			
			rex := byte(0x48)
			if baseReg >= 8 {
				rex |= 0x01 // REX.B
				baseReg -= 8
			}
			a.emit(rex, 0xF7)
			
			// ModR/M for idiv /7
			if offset == 0 && (baseReg&7) != 5 {
				modrm := byte(0x38) | byte(baseReg&7) // /7 = 111 in bits 5-3
				a.emit(modrm)
				if (baseReg & 7) == 4 {
					a.emit(0x24)
				}
			} else if offset >= -128 && offset <= 127 {
				modrm := byte(0x78) | byte(baseReg&7) // mod=01, /7
				a.emit(modrm)
				if (baseReg & 7) == 4 {
					a.emit(0x24)
				}
				a.emit(byte(offset))
			} else {
				modrm := byte(0xB8) | byte(baseReg&7) // mod=10, /7
				a.emit(modrm)
				if (baseReg & 7) == 4 {
					a.emit(0x24)
				}
				a.emitInt32(offset)
			}
			return nil
		}
		return fmt.Errorf("invalid register: %s", op)
	}
	
	rex := byte(0x48)
	if reg >= 8 {
		rex |= 0x01
	}
	a.emit(rex)
	
	a.emit(0xF7)
	a.emit(0xF8 + byte(reg&7))
	return nil
}

func (a *Assembler) encodeCmp(operands []string) error {
	return a.encodeALU(0x39, 0x81, 7, operands)
}

func (a *Assembler) encodeAnd(operands []string) error {
	return a.encodeALU(0x21, 0x81, 4, operands)
}

func (a *Assembler) encodeOr(operands []string) error {
	return a.encodeALU(0x09, 0x81, 1, operands)
}

func (a *Assembler) encodeXor(operands []string) error {
	return a.encodeALU(0x31, 0x81, 6, operands)
}

func (a *Assembler) encodeALU(regOpcode, immOpcode byte, immExt byte, operands []string) error {
	if len(operands) != 2 {
		return fmt.Errorf("ALU op requires 2 operands")
	}
	
	src := strings.TrimSuffix(strings.TrimSpace(operands[0]), ",")
	dst := strings.TrimSpace(operands[1])
	
	if strings.HasPrefix(src, "$") {
		dstReg := parseRegister(dst)
		
		if dstReg == -1 {
			// Destination might be memory: op $imm, offset(%base)
			if strings.Contains(dst, "(%") && strings.HasSuffix(dst, ")") {
				// Parse memory operand
				memStr := dst
				lastPct := strings.LastIndex(memStr, "%")
				if lastPct == -1 {
					return fmt.Errorf("invalid memory operand: %s", dst)
				}
				baseRegStr := memStr[lastPct:]
				baseRegStr = strings.TrimSuffix(baseRegStr, ")")
				baseReg := parseRegister(baseRegStr)
				if baseReg == -1 {
					return fmt.Errorf("invalid base register in: %s", dst)
				}
				
				offsetStr := strings.TrimPrefix(memStr[:lastPct], "(")
				offsetStr = strings.TrimSuffix(offsetStr, "(")
				offset := int32(0)
				if offsetStr != "" {
					val, err := strconv.ParseInt(offsetStr, 10, 32)
					if err != nil {
						return fmt.Errorf("invalid offset in: %s", dst)
					}
					offset = int32(val)
				}
				
				imm, err := parseImmediate(src)
				if err != nil {
					return err
				}
				
				rex := byte(0x48)
				if baseReg >= 8 {
					rex |= 0x01 // REX.B
					baseReg -= 8
				}
				a.emit(rex, immOpcode)
				
				// ModR/M for memory
				if offset == 0 && (baseReg&7) != 5 {
					modrm := byte(0x00) | byte(immExt<<3) | byte(baseReg&7)
					a.emit(modrm)
					if (baseReg & 7) == 4 {
						a.emit(0x24)
					}
				} else if offset >= -128 && offset <= 127 {
					modrm := byte(0x40) | byte(immExt<<3) | byte(baseReg&7)
					a.emit(modrm)
					if (baseReg & 7) == 4 {
						a.emit(0x24)
					}
					a.emit(byte(offset))
				} else {
					modrm := byte(0x80) | byte(immExt<<3) | byte(baseReg&7)
					a.emit(modrm)
					if (baseReg & 7) == 4 {
						a.emit(0x24)
					}
					a.emitInt32(offset)
				}
				a.emitInt32(int32(imm))
				return nil
			}
			return fmt.Errorf("invalid destination: %s", dst)
		}
		
		imm, err := parseImmediate(src)
		if err != nil {
			return err
		}
		
		rex := byte(0x48)
		if dstReg >= 8 {
			rex |= 0x01
		}
		a.emit(rex)
		
		a.emit(immOpcode)
		modrm := byte(0xC0) | byte(immExt<<3) | byte(dstReg&7)
		a.emit(modrm)
		a.emitInt32(int32(imm))
		return nil
	}
	
	srcReg := parseRegister(src)
	dstReg := parseRegister(dst)
	
	if srcReg != -1 && dstReg != -1 {
		rex := byte(0x48)
		if srcReg >= 8 {
			rex |= 0x04
		}
		if dstReg >= 8 {
			rex |= 0x01
		}
		a.emit(rex)
		
		a.emit(regOpcode)
		modrm := byte(0xC0) | byte((srcReg&7)<<3) | byte(dstReg&7)
		a.emit(modrm)
		return nil
	}
	
	// Check for ALU reg, memory: op %reg, offset(%base)
	if srcReg != -1 && strings.Contains(dst, "(%") && strings.HasSuffix(dst, ")") {
		// Parse memory operand
		memStr := dst
		lastPct := strings.LastIndex(memStr, "%")
		if lastPct == -1 {
			return fmt.Errorf("invalid memory operand: %s", dst)
		}
		baseRegStr := memStr[lastPct:]
		baseRegStr = strings.TrimSuffix(baseRegStr, ")")
		baseReg := parseRegister(baseRegStr)
		if baseReg == -1 {
			return fmt.Errorf("invalid base register in: %s", dst)
		}
		
		offsetStr := strings.TrimPrefix(memStr[:lastPct], "(")
		offsetStr = strings.TrimSuffix(offsetStr, "(")
		offset := int32(0)
		if offsetStr != "" {
			val, err := strconv.ParseInt(offsetStr, 10, 32)
			if err != nil {
				return fmt.Errorf("invalid offset in: %s", dst)
			}
			offset = int32(val)
		}
		
		rex := byte(0x48)
		if srcReg >= 8 {
			rex |= 0x04 // REX.R
			srcReg -= 8
		}
		if baseReg >= 8 {
			rex |= 0x01 // REX.B
			baseReg -= 8
		}
		a.emit(rex, regOpcode)
		
		// ModR/M
		if offset == 0 && (baseReg&7) != 5 {
			modrm := byte(0x00) | byte((srcReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 {
				a.emit(0x24)
			}
		} else if offset >= -128 && offset <= 127 {
			modrm := byte(0x40) | byte((srcReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 {
				a.emit(0x24)
			}
			a.emit(byte(offset))
		} else {
			modrm := byte(0x80) | byte((srcReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 {
				a.emit(0x24)
			}
			a.emitInt32(offset)
		}
		return nil
	}
	
	// Check for ALU memory, reg: op offset(%base), %reg
	if dstReg != -1 && strings.Contains(src, "(%") && strings.HasSuffix(src, ")") {
		// Parse memory operand
		memStr := src
		lastPct := strings.LastIndex(memStr, "%")
		if lastPct == -1 {
			return fmt.Errorf("invalid memory operand: %s", src)
		}
		baseRegStr := memStr[lastPct:]
		baseRegStr = strings.TrimSuffix(baseRegStr, ")")
		baseReg := parseRegister(baseRegStr)
		if baseReg == -1 {
			return fmt.Errorf("invalid base register in: %s", src)
		}
		
		offsetStr := strings.TrimPrefix(memStr[:lastPct], "(")
		offsetStr = strings.TrimSuffix(offsetStr, "(")
		offset := int32(0)
		if offsetStr != "" {
			val, err := strconv.ParseInt(offsetStr, 10, 32)
			if err != nil {
				return fmt.Errorf("invalid offset in: %s", src)
			}
			offset = int32(val)
		}
		
		rex := byte(0x48)
		if dstReg >= 8 {
			rex |= 0x04 // REX.R
			dstReg -= 8
		}
		if baseReg >= 8 {
			rex |= 0x01 // REX.B
			baseReg -= 8
		}
		// Use reverse opcode (add 0x02 to regOpcode)
		a.emit(rex, regOpcode+0x02)
		
		// ModR/M
		if offset == 0 && (baseReg&7) != 5 {
			modrm := byte(0x00) | byte((dstReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 {
				a.emit(0x24)
			}
		} else if offset >= -128 && offset <= 127 {
			modrm := byte(0x40) | byte((dstReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 {
				a.emit(0x24)
			}
			a.emit(byte(offset))
		} else {
			modrm := byte(0x80) | byte((dstReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 {
				a.emit(0x24)
			}
			a.emitInt32(offset)
		}
		return nil
	}
	
	return fmt.Errorf("unsupported ALU operands")
}

func (a *Assembler) encodeCall(operands []string) error {
	if len(operands) != 1 {
		return fmt.Errorf("call requires 1 operand")
	}
	
	target := operands[0]
	
	// Direct call
	if !strings.HasPrefix(target, "*") {
		a.emit(0xE8)
		
		if addr, ok := a.labelTargets[target]; ok {
			offset := int32(addr - (len(a.code) + 4))
			a.emitInt32(offset)
		} else {
			a.relocations = append(a.relocations, Relocation{
				Type:   R_X86_64_PC32,
				Offset: uint64(len(a.code)),
				Symbol: target,
				Addend: -4,
			})
			a.emitInt32(0)
		}
		return nil
	}
	
	return fmt.Errorf("indirect call not yet supported")
}

func (a *Assembler) encodeJmp(operands []string) error {
	return a.encodeConditionalJump(0xE9, operands)
}

func (a *Assembler) encodeJe(operands []string) error {
	return a.encodeConditionalJump(0x84, operands)
}

func (a *Assembler) encodeJne(operands []string) error {
	return a.encodeConditionalJump(0x85, operands)
}

func (a *Assembler) encodeJl(operands []string) error {
	return a.encodeConditionalJump(0x8C, operands)
}

func (a *Assembler) encodeJle(operands []string) error {
	return a.encodeConditionalJump(0x8E, operands)
}

func (a *Assembler) encodeJg(operands []string) error {
	return a.encodeConditionalJump(0x8F, operands)
}

func (a *Assembler) encodeJge(operands []string) error {
	return a.encodeConditionalJump(0x8D, operands)
}

func (a *Assembler) encodeConditionalJump(opcode byte, operands []string) error {
	if len(operands) != 1 {
		return fmt.Errorf("jump requires 1 operand")
	}
	
	target := operands[0]
	
	if opcode == 0xE9 {
		a.emit(0xE9)
	} else {
		a.emit(0x0F, opcode)
	}
	
	if addr, ok := a.labelTargets[target]; ok {
		offset := int32(addr - (len(a.code) + 4))
		a.emitInt32(offset)
	} else {
		a.emitInt32(0)
	}
	
	return nil
}

func (a *Assembler) encodeSetCC(opcode byte, operands []string) error {
	if len(operands) != 1 {
		return fmt.Errorf("setCC requires 1 operand")
	}
	
	reg := parseRegister(operands[0])
	if reg == -1 {
		return fmt.Errorf("invalid register: %s", operands[0])
	}
	
	// SETcc instructions use 0x0F opcode prefix
	if reg >= 8 {
		a.emit(0x41)  // REX.B for R8-R15
	}
	a.emit(0x0F, opcode)
	a.emit(0xC0 + byte(reg&7))
	return nil
}

func (a *Assembler) encodeMovzbq(operands []string) error {
	if len(operands) != 2 {
		return fmt.Errorf("movzbq requires 2 operands")
	}
	
	src := strings.TrimSuffix(strings.TrimSpace(operands[0]), ",")
	dst := strings.TrimSpace(operands[1])
	
	srcReg := parseRegister(src)
	dstReg := parseRegister(dst)
	
	if srcReg == -1 || dstReg == -1 {
		return fmt.Errorf("movzbq requires register operands")
	}
	
	// MOVZX with REX.W for 64-bit destination
	rex := byte(0x48)
	if dstReg >= 8 {
		rex |= 0x04
	}
	if srcReg >= 8 {
		rex |= 0x01
	}
	a.emit(rex)
	
	a.emit(0x0F, 0xB6)
	modrm := byte(0xC0) | byte((dstReg&7)<<3) | byte(srcReg&7)
	a.emit(modrm)
	return nil
}

func (a *Assembler) encodeTest(operands []string) error {
	if len(operands) != 2 {
		return fmt.Errorf("test requires 2 operands")
	}
	
	src := strings.TrimSuffix(strings.TrimSpace(operands[0]), ",")
	dst := strings.TrimSpace(operands[1])
	
	srcReg := parseRegister(src)
	dstReg := parseRegister(dst)
	
	if srcReg != -1 && dstReg != -1 {
		rex := byte(0x48)
		if srcReg >= 8 {
			rex |= 0x04
		}
		if dstReg >= 8 {
			rex |= 0x01
		}
		a.emit(rex)
		
		a.emit(0x85)
		modrm := byte(0xC0) | byte((srcReg&7)<<3) | byte(dstReg&7)
		a.emit(modrm)
		return nil
	}
	
	// Check for test mem, mem (typically same operand)
	srcIsMem := strings.Contains(src, "(%") && strings.HasSuffix(src, ")")
	dstIsMem := strings.Contains(dst, "(%") && strings.HasSuffix(dst, ")")
	
	if srcIsMem && dstIsMem {
		// For test mem, mem we convert to: load mem into reg, test reg, reg
		// This is typically used for testing if a value is zero
		return fmt.Errorf("test mem, mem not directly supported - use cmpq or load to register first")
	}
	
	return fmt.Errorf("unsupported test operands")
}

func (a *Assembler) emit(bytes ...byte) {
	a.code = append(a.code, bytes...)
}

func (a *Assembler) encodeLea(operands []string) error {
	// leaq label(%rip), %reg or leaq offset(%base), %reg
	if len(operands) != 2 {
		return fmt.Errorf("lea requires 2 operands")
	}
	
	src := strings.TrimSpace(strings.TrimSuffix(operands[0], ","))
	dst := strings.TrimSpace(operands[1])
	
	dstReg := parseRegister(dst)
	if dstReg < 0 {
		return fmt.Errorf("invalid destination register: %s", dst)
	}
	
	// Check for RIP-relative addressing: label(%rip)
	if strings.Contains(src, "(%rip)") {
		label := strings.TrimSuffix(src, "(%rip)")
		label = strings.TrimSpace(label)
		
		// leaq label(%rip), reg
		// Opcode: REX.W + 0x8D /r with ModR/M 00 reg 101
		// REX.W = 0x48
		rex := byte(0x48)
		if dstReg >= 8 {
			rex |= 0x04 // REX.R
			dstReg -= 8
		}
		
		a.emit(rex, 0x8D)
		// ModR/M: 00 reg 101 (RIP-relative)
		modrm := byte(0x05) | byte(dstReg<<3)
		a.emit(modrm)
		
		// Add relocation for the displacement (4 bytes)
		a.relocations = append(a.relocations, Relocation{
			Type:   R_X86_64_PC32,
			Offset: uint64(len(a.code)),
			Symbol: label,
			Addend: -4,
		})
		a.emitInt32(0) // Placeholder for displacement
		
		return nil
	}
	
	// Check for regular addressing: offset(%base)
	if strings.Contains(src, "(%") && strings.HasSuffix(src, ")") {
		// Parse: offset(%base)
		memStr := src
		lastPct := strings.LastIndex(memStr, "%")
		if lastPct == -1 {
			return fmt.Errorf("invalid memory operand: %s", src)
		}
		baseRegStr := memStr[lastPct:]
		baseRegStr = strings.TrimSuffix(baseRegStr, ")")
		baseReg := parseRegister(baseRegStr)
		if baseReg == -1 {
			return fmt.Errorf("invalid base register in: %s", src)
		}
		
		offsetStr := strings.TrimPrefix(memStr[:lastPct], "(")
		offsetStr = strings.TrimSuffix(offsetStr, "(")
		offset := int32(0)
		if offsetStr != "" {
			val, err := strconv.ParseInt(offsetStr, 10, 32)
			if err != nil {
				return fmt.Errorf("invalid offset in: %s", src)
			}
			offset = int32(val)
		}
		
		// LEA offset(%base), %dst
		rex := byte(0x48)
		if dstReg >= 8 {
			rex |= 0x04 // REX.R
			dstReg -= 8
		}
		if baseReg >= 8 {
			rex |= 0x01 // REX.B
			baseReg -= 8
		}
		
		a.emit(rex, 0x8D)
		
		// ModR/M
		if offset == 0 && (baseReg&7) != 5 { // Not RBP
			modrm := byte(0x00) | byte((dstReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 { // RSP needs SIB
				a.emit(0x24)
			}
		} else if offset >= -128 && offset <= 127 {
			modrm := byte(0x40) | byte((dstReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 { // RSP needs SIB
				a.emit(0x24)
			}
			a.emit(byte(offset))
		} else {
			modrm := byte(0x80) | byte((dstReg&7)<<3) | byte(baseReg&7)
			a.emit(modrm)
			if (baseReg & 7) == 4 { // RSP needs SIB
				a.emit(0x24)
			}
			a.emitInt32(offset)
		}
		return nil
	}
	
	return fmt.Errorf("unsupported lea addressing mode: %s", src)
}

func (a *Assembler) encodeShift(modrmExt byte, operands []string) error {
	// shift $imm, %reg or shift %cl, %reg
	if len(operands) != 2 {
		return fmt.Errorf("shift requires 2 operands")
	}
	
	src := strings.TrimSpace(strings.TrimSuffix(operands[0], ","))
	dst := strings.TrimSpace(operands[1])
	
	dstReg := parseRegister(dst)
	if dstReg < 0 {
		return fmt.Errorf("invalid destination register: %s", dst)
	}
	
	rex := byte(0x48)
	if dstReg >= 8 {
		rex |= 0x01 // REX.B
		dstReg -= 8
	}
	
	// Check for immediate shift
	if strings.HasPrefix(src, "$") {
		imm, err := parseImmediate(src)
		if err != nil {
			return err
		}
		
		a.emit(rex)
		if imm == 1 {
			// Special encoding for shift by 1
			a.emit(0xD1)
			a.emit(modrmExt | byte(dstReg&7))
		} else {
			// Shift by immediate byte
			a.emit(0xC1)
			a.emit(modrmExt | byte(dstReg&7))
			a.emit(byte(imm))
		}
		return nil
	}
	
	// Check for shift by CL register
	if src == "%cl" || src == "%rcx" {
		a.emit(rex, 0xD3)
		a.emit(modrmExt | byte(dstReg&7))
		return nil
	}
	
	return fmt.Errorf("unsupported shift operands: %s, %s", src, dst)
}

func (a *Assembler) encodeNeg(operands []string) error {
	// negq %reg or negq offset(%base)
	if len(operands) != 1 {
		return fmt.Errorf("neg requires 1 operand")
	}
	
	dst := strings.TrimSpace(operands[0])
	dstReg := parseRegister(dst)
	
	if dstReg != -1 {
		// negq %reg
		rex := byte(0x48)
		if dstReg >= 8 {
			rex |= 0x01 // REX.B
			dstReg -= 8
		}
		a.emit(rex, 0xF7)
		a.emit(0xD8 | byte(dstReg&7)) // ModR/M for NEG /3
		return nil
	}
	
	// Check for memory operand
	if strings.Contains(dst, "(%") && strings.HasSuffix(dst, ")") {
		// Parse memory operand
		memStr := dst
		lastPct := strings.LastIndex(memStr, "%")
		if lastPct == -1 {
			return fmt.Errorf("invalid memory operand: %s", dst)
		}
		baseRegStr := memStr[lastPct:]
		baseRegStr = strings.TrimSuffix(baseRegStr, ")")
		baseReg := parseRegister(baseRegStr)
		if baseReg == -1 {
			return fmt.Errorf("invalid base register in: %s", dst)
		}
		
		offsetStr := strings.TrimPrefix(memStr[:lastPct], "(")
		offsetStr = strings.TrimSuffix(offsetStr, "(")
		offset := int32(0)
		if offsetStr != "" {
			val, err := strconv.ParseInt(offsetStr, 10, 32)
			if err != nil {
				return fmt.Errorf("invalid offset in: %s", dst)
			}
			offset = int32(val)
		}
		
		rex := byte(0x48)
		if baseReg >= 8 {
			rex |= 0x01 // REX.B
			baseReg -= 8
		}
		a.emit(rex, 0xF7)
		
		// ModR/M for memory (opcode extension /3 for NEG)
		if offset == 0 && (baseReg&7) != 5 {
			modrm := byte(0x18) | byte(baseReg&7) // /3 = 011 in bits 5-3
			a.emit(modrm)
			if (baseReg & 7) == 4 {
				a.emit(0x24)
			}
		} else if offset >= -128 && offset <= 127 {
			modrm := byte(0x58) | byte(baseReg&7) // mod=01, /3
			a.emit(modrm)
			if (baseReg & 7) == 4 {
				a.emit(0x24)
			}
			a.emit(byte(offset))
		} else {
			modrm := byte(0x98) | byte(baseReg&7) // mod=10, /3
			a.emit(modrm)
			if (baseReg & 7) == 4 {
				a.emit(0x24)
			}
			a.emitInt32(offset)
		}
		return nil
	}
	
	return fmt.Errorf("invalid neg operand: %s", dst)
}

func (a *Assembler) emitInt32(val int32) {
	a.emit(byte(val), byte(val>>8), byte(val>>16), byte(val>>24))
}

func (a *Assembler) emitInt64(val int64) {
	for i := 0; i < 8; i++ {
		a.emit(byte(val >> (i * 8)))
	}
}

func parseRegister(s string) int {
	s = strings.TrimPrefix(s, "%")
	s = strings.ToLower(s)
	
	if code, ok := regNameToCode[s]; ok {
		return code
	}
	return -1
}

func parseImmediate(s string) (int64, error) {
	s = strings.TrimPrefix(s, "$")
	
	if strings.HasPrefix(s, "0x") {
		val, err := strconv.ParseInt(s[2:], 16, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid hex immediate: %s", s)
		}
		return val, nil
	}
	
	// Try parsing as integer first
	val, err := strconv.ParseInt(s, 10, 64)
	if err == nil {
		return val, nil
	}
	
	// If it has a decimal point, try parsing as float and truncate
	if strings.Contains(s, ".") {
		fval, ferr := strconv.ParseFloat(s, 64)
		if ferr == nil {
			return int64(fval), nil
		}
	}
	
	return 0, fmt.Errorf("invalid immediate: %s", s)
}

func (a *Assembler) GetCode() []byte {
	return a.code
}

func (a *Assembler) GetRelocations() []Relocation {
	return a.relocations
}

func (a *Assembler) GetSymbols() map[string]uint64 {
	return a.symbols
}
