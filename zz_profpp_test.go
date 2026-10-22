package main

import (
	"os"
	"testing"
)

func TestProfPPOnly(t *testing.T) {
	src, err := os.ReadFile("/tmp/opencode/bench/main-3m.c")
	if err != nil {
		t.Skip(err)
	}
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
	s2 := joinLogicalLines(stripComments(joinContinuedLines(string(src))))
	if _, err := p.Process(s2); err != nil {
		t.Fatal(err)
	}
}
