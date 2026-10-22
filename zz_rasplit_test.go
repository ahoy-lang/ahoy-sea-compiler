package main

import (
	"os"
	"sort"
	"testing"
	"time"
)

func TestRASplit(t *testing.T) {
	pre, err := os.ReadFile("/tmp/opencode/bench/pp.c")
	if err != nil {
		t.Skip(err)
	}
	s := string(pre)
	p := NewParser(s)
	ast, err := p.Parse()
	if err != nil {
		t.Fatal(err)
	}
	structs, typedefs, enums, funcs := func() (map[string]*StructDef, map[string]string, map[string]int, map[string]*FunctionSignature) {
		cp0 := NewCompilerPipeline("", CompilerOptions{})
		cp0.parser = p
		cp0.ast = ast
		return cp0.symbolTables()
	}()
	sel := NewInstructionSelector()
	sel.structs = structs
	sel.typedefs = typedefs
	sel.enums = enums
	sel.functions = funcs
	sel.funcDefined = definedFuncSet(ast)
	if err := sel.SelectInstructionsParallel(ast); err != nil {
		t.Fatal(err)
	}
	ir := sel.instructions
	ast = nil
	step := func(name string, fn func()) {
		start := time.Now()
		fn()
		t.Logf("%-16s %v", name, time.Since(start))
	}
	lsa := NewLinearScanAllocator(ir)
	lsa.PreSizeTemps(sel.MaxTemp())
	step("computeIntervals", func() { lsa.computeIntervals() })
	step("sort", func() { sort.Sort(intervalByStart(lsa.intervals)) })
	step("argBusy", func() { lsa.collectArgRegBusy() })
	step("forbidMasks", func() { lsa.computeForbiddenMasks() })
	step("clobberMasks", func() { lsa.computeEmitterClobberMasks() })
	step("scan", func() {
		for _, interval := range lsa.intervals {
			lsa.expireOldIntervals(interval)
			pick := -1
			for fi, r := range lsa.freeRegs {
				if interval.Forbidden&(1<<uint(r)) == 0 {
					pick = fi
					break
				}
			}
			if pick < 0 {
				lsa.spillAtInterval(interval)
			} else {
				reg := lsa.freeRegs[pick]
				lsa.freeRegs = append(lsa.freeRegs[:pick], lsa.freeRegs[pick+1:]...)
				interval.Reg = reg
				if id, ok := tempID(interval.VarName); ok {
					lsa.growTemp(id)
					lsa.slots[id].reg = int32(reg)
				}
				lsa.insertActive(interval)
			}
		}
	})
	step("rewrite", func() { lsa.rewriteInstructions() })
	t.Logf("intervals=%d", len(lsa.intervals))
}
