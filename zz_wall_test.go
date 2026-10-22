package main

import (
	"os"
	"testing"
	"time"
)

func TestCompileWall(t *testing.T) {
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
	for i := 0; i < 4; i++ {
		start := time.Now()
		cp := NewCompilerPipeline(string(src), options)
		cp.sourceFile = "/tmp/opencode/bench/main-3m.c"
		if err := cp.Compile(); err != nil {
			t.Fatal(err)
		}
		t.Logf("iter%d %v ir=%d", i, time.Since(start), len(cp.ir))
	}
}
