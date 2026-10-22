package main

import (
	"os"
	"testing"
	"time"
)

func TestProfilePP(t *testing.T) {
	src, err := os.ReadFile("/tmp/opencode/bench/main-3m.c")
	if err != nil {
		t.Skip(err)
	}
	source := string(src)
	step := func(name string, fn func()) time.Duration {
		start := time.Now()
		fn()
		d := time.Since(start)
		t.Logf("%-22s %v", name, d)
		return d
	}
	var s0, s1, s2 string
	step("joinContinuedLines", func() { s0 = joinContinuedLines(source) })
	step("stripComments", func() { s1 = stripComments(s0) })
	step("joinLogicalLines", func() { s2 = joinLogicalLines(s1) })
	s0, s1 = "", ""
	p := NewPreprocessor()
	for _, inc := range []string{
		"../bell/repos",
		"../bell/repos/box2d/include",
		"../bell/repos/box3d/include",
		"../bell/repos/SDL3/include",
		"../bell/output",
	} {
		p.AddIncludePath(inc)
	}
	var out string
	step("Process(total)", func() {
		var err error
		out, err = p.Process(s2)
		if err != nil {
			t.Fatal(err)
		}
	})
	_ = out
	t.Logf("defines=%d funcMacros=%d", len(p.defines), len(p.funcMacros))
}
