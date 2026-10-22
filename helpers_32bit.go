package main

import (
	"strconv"
	"strings"
)

// Precomputed "%reg" spellings for every standard register name, so the
// hot reg/freg format path avoids the per-operand cache map lookup AND the
// "%"+Value allocation (shared strings, no per-format garbage).
// Initialized once at startup; concurrent reads need no locking. Any name
// missing here (shouldn't happen) falls back to the regular format path.
var regSpell64 = buildRegSpell64()

// regSpell32 maps a 64-bit register name to its "%32-bit" spelling,
// mirroring reg64to32 exactly (unknown names map to "%"+identity, same as
// the default branch there). Identity entries cover already-narrow Values.
var regSpell32 = buildRegSpell32()

func buildRegSpell64() map[string]string {
	names := []string{
		"rax", "rbx", "rcx", "rdx", "rsi", "rdi", "rbp", "rsp",
		"r8", "r9", "r10", "r11", "r12", "r13", "r14", "r15",
		"eax", "ebx", "ecx", "edx", "esi", "edi", "ebp", "esp",
		"r8d", "r9d", "r10d", "r11d", "r12d", "r13d", "r14d", "r15d",
		"ax", "bx", "cx", "dx", "si", "di", "bp", "sp",
		"r8w", "r9w", "r10w", "r11w", "r12w", "r13w", "r14w", "r15w",
		"al", "bl", "cl", "dl", "sil", "dil", "bpl", "spl",
		"r8b", "r9b", "r10b", "r11b", "r12b", "r13b", "r14b", "r15b",
		"ah", "ch", "dh", "bh",
	}
	m := make(map[string]string, len(names)+16)
	for _, n := range names {
		m[n] = "%" + n
	}
	for i := 0; i < 16; i++ {
		n := "xmm" + strconv.Itoa(i)
		m[n] = "%" + n
	}
	return m
}

func buildRegSpell32() map[string]string {
	m := make(map[string]string, 80)
	for i, n64 := range regNames {
		m[n64] = "%" + reg32Names[i]
	}
	// Identity entries reproduce reg64to32's default branch ("%"+as-is)
	// for already-narrow Values.
	for _, n := range reg32Names {
		m[n] = "%" + n
	}
	for _, n := range []string{
		"ax", "bx", "cx", "dx", "si", "di", "bp", "sp",
		"r8w", "r9w", "r10w", "r11w", "r12w", "r13w", "r14w", "r15w",
		"al", "bl", "cl", "dl", "sil", "dil", "bpl", "spl",
		"r8b", "r9b", "r10b", "r11b", "r12b", "r13b", "r14b", "r15b",
		"ah", "ch", "dh", "bh",
	} {
		m[n] = "%" + n
	}
	return m
}

// formatOperand32 formats operands for 32-bit operations (int types)
func (ce *CodeEmitter) formatOperand32(op *Operand) string {
	if op == nil {
		return ""
	}
	if op.Type == "reg" {
		if s, ok := regSpell32[op.Value]; ok {
			return s
		}
	}
	if op.Type == "freg" {
		if s, ok := regSpell64[op.Value]; ok {
			return s
		}
	}
	if op.Type == "label" {
		return op.Value
	}
	if op.Type == "imm" {
		if s, ok := ce.immCache[op.Value]; ok {
			return s
		}
		s := ce.formatOperand32Cold(op)
		if ce.immCache == nil {
			ce.immCache = make(map[string]string)
		}
		ce.immCache[op.Value] = s
		return s
	}
	if (op.Type == "var" || op.Type == "addr") && op.IsGlobal {
		k := globKey{val: op.Value, off: int32(op.Offset)}
		if s, ok := ce.globCache[k]; ok {
			return s
		}
		s := ce.formatOperand32Cold(op)
		if ce.globCache == nil {
			ce.globCache = make(map[globKey]string)
		}
		ce.globCache[k] = s
		return s
	}
	if offCacheable(op) {
		k := offKey{typ: offTypeCode(op.Type), wide: true, off: int32(op.Offset)}
		if s, ok := ce.offCache[k]; ok {
			return s
		}
		s := ce.formatOperand32Cold(op)
		if ce.offCache == nil {
			ce.offCache = make(map[offKey]string)
		}
		ce.offCache[k] = s
		return s
	}
	if ce.opFmtCache != nil {
		if e, ok := ce.opFmtCache[op]; ok && e.ok32 {
			return e.a32
		}
	}
	s := ce.formatOperand32Cold(op)
	if ce.opFmtCache == nil {
		ce.opFmtCache = make(map[*Operand]opFmt)
	}
	e := ce.opFmtCache[op]
	e.a32, e.ok32 = s, true
	ce.opFmtCache[op] = e
	return s
}

func (ce *CodeEmitter) formatOperand32Cold(op *Operand) string {
	if op == nil {
		return ""
	}
	
	switch op.Type {
	case "reg":
		// Convert 64-bit register to 32-bit equivalent
		return "%" + ce.reg64to32(op.Value)
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
			if op.Offset != 0 {
				return ce.globalRef(op)
			}
			return op.Value + "(%rip)"
		}
		return frameRef(op.Offset, "%rbp")
	case "array":
		// arr[index] where index is in IndexTemp
		if op.IndexTemp != nil {
			indexReg := ce.formatOperand32(op.IndexTemp)
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
			ptrReg := ce.formatOperand32(op.IndexTemp)
			return "(" + ptrReg + ")"
		}
		if op.SourcePtr != nil {
			// Use the source pointer
			ptrReg := ce.formatOperand32(op.SourcePtr)
			return "(" + ptrReg + ")"
		}
		return "(%" + op.Value + ")"
	}
	
	return ""
}

// reg64to16 converts 64-bit register names to 16-bit equivalents.
func (ce *CodeEmitter) reg64to16(reg64 string) string {
	reg := strings.TrimPrefix(reg64, "%")
	switch reg {
	case "rax":
		return "ax"
	case "rbx":
		return "bx"
	case "rcx":
		return "cx"
	case "rdx":
		return "dx"
	case "rsi":
		return "si"
	case "rdi":
		return "di"
	case "rbp":
		return "bp"
	case "rsp":
		return "sp"
	case "r8":
		return "r8w"
	case "r9":
		return "r9w"
	case "r10":
		return "r10w"
	case "r11":
		return "r11w"
	case "r12":
		return "r12w"
	case "r13":
		return "r13w"
	case "r14":
		return "r14w"
	case "r15":
		return "r15w"
	}
	return reg
}

// reg64to32 converts 64-bit register names to 32-bit equivalents
func (ce *CodeEmitter) reg64to32(reg64 string) string {
	switch reg64 {
	case "rax": return "eax"
	case "rbx": return "ebx"
	case "rcx": return "ecx"
	case "rdx": return "edx"
	case "rsi": return "esi"
	case "rdi": return "edi"
	case "rsp": return "esp"
	case "rbp": return "ebp"
	case "r8": return "r8d"
	case "r9": return "r9d"
	case "r10": return "r10d"
	case "r11": return "r11d"
	case "r12": return "r12d"
	case "r13": return "r13d"
	case "r14": return "r14d"
	case "r15": return "r15d"
	default: return reg64 // Return as-is if not recognized
	}
}
