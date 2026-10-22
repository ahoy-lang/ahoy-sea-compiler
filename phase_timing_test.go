package main

import (
	"os"
	"testing"
	"time"
)

// TestPhaseTiming times each pipeline phase on the pinned 3MB snapshot.
func TestPhaseTiming(t *testing.T) {
	src, err := os.ReadFile("/tmp/opencode/bench/main-3m.c")
	if err != nil {
		t.Skip("no snapshot:", err)
	}
	options := CompilerOptions{
		OptimizationLevel: 0,
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
	step := func(name string, fn func()) time.Duration {
		start := time.Now()
		fn()
		d := time.Since(start)
		t.Logf("%-12s %v", name, d)
		return d
	}
	cp := NewCompilerPipeline(string(src), options)
	cp.sourceFile = "/tmp/opencode/bench/main-3m.c"
	var pp, ast *ASTNode
	_ = ast
	var pre string
	step("preprocess", func() {
		var err error
		pre, err = cp.runPreprocessor()
		if err != nil {
			t.Fatal(err)
		}
	})
	_ = pp
	step("parse", func() {
		if err := cp.runParser(pre); err != nil {
			t.Fatal(err)
		}
		pre = ""
	})
	var structs map[string]*StructDef
	var typedefs map[string]string
	var enums map[string]int
	var funcs map[string]*FunctionSignature
	step("symtables", func() {
		structs, typedefs, enums, funcs = cp.symbolTables()
	})
	step("select", func() {
		cp.selector = NewInstructionSelector()
		cp.selector.structs = structs
		cp.selector.typedefs = typedefs
		cp.selector.enums = enums
		cp.selector.functions = funcs
		cp.selector.funcDefined = definedFuncSet(cp.ast)
		if err := cp.selector.SelectInstructionsParallel(cp.ast); err != nil {
			t.Fatal(err)
		}
		cp.ir = cp.selector.instructions
	})
	cp.ast = nil
	step("regalloc", func() {
		lsAlloc := NewLinearScanAllocator(cp.ir)
		lsAlloc.PreSizeTemps(cp.selector.MaxTemp())
		if err := lsAlloc.Allocate(); err != nil {
			t.Fatal(err)
		}
		cp.usedRegs = lsAlloc.GetUsedRegisters()
	})
	step("emit", func() {
		cp.emitter = NewCodeEmitter(cp.ir, cp.selector.stringLits, cp.selector.globalVars, cp.selector.structs, cp.selector.funcIsLocal)
		cp.emitter.usedRegisters = cp.usedRegs
		cp.assembly = cp.emitter.EmitParallel()
	})
	step("asm", func() {
		for i := 0; i < 3; i++ {
			st := time.Now()
			if _, err := AssembleObject(cp.assembly); err != nil {
				t.Fatal(err)
			}
			t.Logf("asm-rep%d %v", i, time.Since(st))
		}
	})
	t.Logf("IR=%d asmBytes=%d", len(cp.ir), len(cp.assembly))
}
