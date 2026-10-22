package main

import (
	"os"
	"testing"
	"time"
)

// TestProfileProcess times each sub-step of Preprocessor.Process on the real
// generated main.c so we can see where the wall-clock goes.
func TestProfileProcess(t *testing.T) {
	src, err := os.ReadFile("../bell/output/main.c")
	if err != nil {
		t.Skip("no main.c:", err)
	}
	source := string(src)

	step := func(name string, fn func()) {
		start := time.Now()
		fn()
		t.Logf("%-22s %v", name, time.Since(start))
	}

	var s0, s1, s2 string
	step("joinContinuedLines", func() { s0 = joinContinuedLines(source) })
	step("stripComments", func() { s1 = stripComments(s0) })
	step("joinLogicalLines", func() { s2 = joinLogicalLines(s1) })
	_ = s2

	p := NewPreprocessor()
	// Mirror the real bell-sea -I paths (relative to the bell dir).
	for _, inc := range []string{
		"../bell/repos",
		"../bell/repos/box2d/include",
		"../bell/repos/box3d/include",
		"../bell/repos/SDL3/include",
		"../bell/output",
	} {
		p.AddIncludePath(inc)
	}
	step("Process(total)", func() { p.Process(source) })

	// Report macro table sizes (the suspected O(#macros) scan).
	t.Logf("defines=%d funcMacros=%d", len(p.defines), len(p.funcMacros))
}
