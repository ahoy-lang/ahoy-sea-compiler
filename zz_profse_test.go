package main

import (
	"os"
	"testing"
)

func TestProfSelectEmit(t *testing.T) {
	pre, err := os.ReadFile("/tmp/opencode/bench/pp.c")
	if err != nil {
		t.Skip(err)
	}
	cp := NewCompilerPipeline(string(pre), CompilerOptions{OptimizationLevel: 0})
	cp.options.NoPreprocess = true
	if err := cp.runParser(string(pre)); err != nil {
		t.Fatal(err)
	}
	structs, typedefs, enums, funcs := cp.symbolTables()
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
	cp.ast = nil
	lsAlloc := NewLinearScanAllocator(cp.ir)
	lsAlloc.PreSizeTemps(cp.selector.MaxTemp())
	if err := lsAlloc.Allocate(); err != nil {
		t.Fatal(err)
	}
	cp.usedRegs = lsAlloc.GetUsedRegisters()
	cp.emitter = NewCodeEmitter(cp.ir, cp.selector.stringLits, cp.selector.globalVars, cp.selector.structs, cp.selector.funcIsLocal)
	cp.emitter.usedRegisters = cp.usedRegs
	cp.assembly = cp.emitter.EmitParallel()
	t.Logf("IR=%d asm=%d", len(cp.ir), len(cp.assembly))
}
