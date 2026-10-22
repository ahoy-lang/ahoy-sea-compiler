package main

import (
	"os"
	"testing"
)

func TestDumpAsm(t *testing.T) {
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
	if err := cp.Compile(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("DUMP_TO"), []byte(cp.assembly), 0644); err != nil {
		t.Fatal(err)
	}
	t.Logf("asm bytes=%d", len(cp.assembly))
}
