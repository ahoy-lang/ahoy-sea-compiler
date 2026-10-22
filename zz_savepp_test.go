package main

import (
	"os"
	"testing"
	"time"
)

func TestSavePP(t *testing.T) {
	src, err := os.ReadFile("/tmp/opencode/bench/main-3m.c")
	if err != nil {
		t.Skip(err)
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
	cp := NewCompilerPipeline(string(src), options)
	cp.sourceFile = "/tmp/opencode/bench/main-3m.c"
	start := time.Now()
	pre, err := cp.runPreprocessor()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pp %v", time.Since(start))
	if err := os.WriteFile("/tmp/opencode/bench/pp.c", []byte(pre), 0644); err != nil {
		t.Fatal(err)
	}
	t.Logf("saved %d bytes", len(pre))
}
