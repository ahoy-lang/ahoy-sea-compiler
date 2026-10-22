package main

import (
	"os"
	"testing"
	"time"
)

// BenchmarkAsmOnly times AssembleObject on the saved 31MB editor assembly.
func BenchmarkAsmOnly(b *testing.B) {
	asm, err := os.ReadFile("/tmp/opencode/bench/editor.s")
	if err != nil {
		b.Skip(err)
	}
	s := string(asm)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		if _, err := AssembleObject(s); err != nil {
			b.Fatal(err)
		}
		b.Logf("rep %v", time.Since(start))
	}
}
