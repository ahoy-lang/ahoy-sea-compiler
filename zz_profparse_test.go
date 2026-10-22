package main

import (
	"os"
	"testing"
)

func TestProfParseOnly(t *testing.T) {
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
	t.Logf("toplevel=%d", len(ast.Children))
}
