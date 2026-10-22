package main

import (
	"os"
	"testing"
)

// BenchmarkCompileEditor runs the full front/mid/back pipeline on the real
// bell editor C output (88k lines, 6.1MB). It is the end-to-end measure for
// the tcc-parity work: tcc does the same input in ~135ms.
func BenchmarkCompileEditor(b *testing.B) {
	src, err := os.ReadFile("../bell/output/main.c")
	if err != nil {
		b.Skip("no main.c:", err)
	}
	options := CompilerOptions{
		OptimizationLevel: 0,
		Verbose:           false,
		IncludePaths: []string{
			"../bell/repos",
			"../bell/repos/box2d/include",
			"../bell/repos/box3d/include",
			"../bell/repos/SDL3/include",
			"../bell/output",
			"../ahoy/repos/raylib/src",
		},
		Defines: []string{"BACKEND_RAYLIB"},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cp := NewCompilerPipeline(string(src), options)
		cp.sourceFile = "../bell/output/main.c"
		if err := cp.Compile(); err != nil {
			b.Fatal(err)
		}
	}
}

// TestIRStats reports how many IR instructions each opcode accounts for, so we
// can see what dominates the 1.6M-instruction stream.
func TestIRStats(t *testing.T) {
	src, err := os.ReadFile("../bell/output/main.c")
	if err != nil {
		t.Skip("no main.c:", err)
	}
	options := CompilerOptions{
		IncludePaths: []string{
			"../bell/repos",
			"../bell/repos/box2d/include",
			"../bell/repos/box3d/include",
			"../bell/repos/SDL3/include",
			"../bell/output",
			"../ahoy/repos/raylib/src",
		},
		Defines: []string{"BACKEND_RAYLIB"},
	}
	cp := NewCompilerPipeline(string(src), options)
	cp.sourceFile = "../bell/output/main.c"
	if err := cp.Compile(); err != nil {
		t.Fatal(err)
	}
	counts := map[OpCode]int{}
	for _, ir := range cp.ir {
		counts[ir.Op]++
	}
	total := len(cp.ir)
	t.Logf("total IR = %d", total)
	for op := OpNop; op <= OpSetAl; op++ {
		if counts[op] > 0 {
			t.Logf("  %-12s %8d", opName(op), counts[op])
		}
	}
}

func opName(op OpCode) string {
	switch op {
	case OpNop:
		return "Nop"
	case OpAdd:
		return "Add"
	case OpSub:
		return "Sub"
	case OpMul:
		return "Mul"
	case OpDiv:
		return "Div"
	case OpMod:
		return "Mod"
	case OpNeg:
		return "Neg"
	case OpAnd:
		return "And"
	case OpOr:
		return "Or"
	case OpXor:
		return "Xor"
	case OpNot:
		return "Not"
	case OpShl:
		return "Shl"
	case OpShr:
		return "Shr"
	case OpEq:
		return "Eq"
	case OpNe:
		return "Ne"
	case OpLt:
		return "Lt"
	case OpLe:
		return "Le"
	case OpGt:
		return "Gt"
	case OpGe:
		return "Ge"
	case OpMov:
		return "Mov"
	case OpMovFloat:
		return "MovFloat"
	case OpLoad:
		return "Load"
	case OpStore:
		return "Store"
	case OpLoadAddr:
		return "LoadAddr"
	case OpCall:
		return "Call"
	case OpRet:
		return "Ret"
	case OpJmp:
		return "Jmp"
	case OpJz:
		return "Jz"
	case OpJnz:
		return "Jnz"
	case OpLabel:
		return "Label"
	case OpPush:
		return "Push"
	case OpPop:
		return "Pop"
	case OpParam:
		return "Param"
	case OpSetArg:
		return "SetArg"
	case OpStackAlloc:
		return "StackAlloc"
	case OpStackFree:
		return "StackFree"
	case OpCvt:
		return "Cvt"
	case OpSetAl:
		return "SetAl"
	}
	return "?"
}
