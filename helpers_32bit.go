package main

import "fmt"

// formatOperand32 formats operands for 32-bit operations (int types)
func (ce *CodeEmitter) formatOperand32(op *Operand) string {
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
		return fmt.Sprintf("%d(%%rbp)", op.Offset)
	case "var":
		if op.IsGlobal {
			return op.Value + "(%rip)"
		}
		return fmt.Sprintf("%d(%%rbp)", op.Offset)
	case "array":
		// arr[index] where index is in IndexTemp
		if op.IndexTemp != nil {
			indexReg := ce.formatOperand32(op.IndexTemp)
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
			ptrReg := ce.formatOperand32(op.IndexTemp)
			return fmt.Sprintf("(%s)", ptrReg)
		}
		if op.SourcePtr != nil {
			// Use the source pointer
			ptrReg := ce.formatOperand32(op.SourcePtr)
			return fmt.Sprintf("(%s)", ptrReg)
		}
		return fmt.Sprintf("(%%%s)", op.Value)
	}
	
	return ""
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
