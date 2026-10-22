package main

import (
	"os"
	"testing"
)

// BenchmarkCompileSnap mirrors BenchmarkCompileEditor on the pinned 3MB
// snapshot (bell/output/main.c flips size as bell rebuilds).
func BenchmarkCompileSnap(b *testing.B) {
	src, err := os.ReadFile("/tmp/opencode/bench/main-3m.c")
	if err != nil {
		b.Skip("no snapshot:", err)
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
		cp.sourceFile = "/tmp/opencode/bench/main-3m.c"
		if err := cp.Compile(); err != nil {
			b.Fatal(err)
		}
	}
}
