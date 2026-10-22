package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestSpassEncode validates the structured encoder against GNU as.
// Each case: spass bytes must equal GAS bytes for the same instruction.
func TestSpassEncode(t *testing.T) {
	sp := NewSPass(nil, nil, nil, nil, nil, 0)
	f := &funcState{sp: sp, locals: make(map[string]*LocalVar), labels: make(map[string]uint64)}

	intTy := CType{Name: "long", Size: 8, Signed: true}
	int32Ty := CType{Name: "int", Size: 4, Signed: true}

	type tc struct {
		name string
		emit func()
		asm  string
	}
	cases := []tc{
		{"movrr", func() { f.encMovRegReg(REG_RAX, REG_RBX, 8) }, "movq %rbx, %rax"},
		{"movrl", func() { f.encMovRegReg(REG_RAX, REG_RBX, 4) }, "movl %ebx, %eax"},
		{"movrm", func() {
			f.encMovRegMem(REG_RAX, Addr{RegBase: REG_RBP, Disp: -48, Index: -1, Scale: 1, Size: 8}, intTy)
		}, "movq -48(%rbp), %rax"},
		{"movmr", func() {
			f.encMovMemReg(Addr{RegBase: REG_RBP, Disp: -48, Index: -1, Scale: 1, Size: 8}, REG_RCX, intTy)
		}, "movq %rcx, -48(%rbp)"},
		{"movri", func() { f.encMovRegImm(REG_RAX, "42", intTy) }, "movq $42, %rax"},
		{"movri-big", func() { f.encMovRegImm(REG_R10, "0x123456789ABCDEF", intTy) }, "movabs $0x123456789ABCDEF, %r10"},
		{"cmpri8", func() { f.encALURegImm("cmp", REG_RCX, 5) }, "cmpq $5, %rcx"},
		{"addrax", func() { f.encALURegImm("add", REG_RAX, 1000) }, "addq $1000, %rax"},
		{"movmi", func() {
			f.encMovMemImm(Addr{RegBase: REG_RBP, Disp: -8, Index: -1, Scale: 1, Size: 8}, "7", intTy)
		}, "movq $7, -8(%rbp)"},
		{"movslq", func() {
			f.encMovRegMem(REG_RAX, Addr{RegBase: REG_RBP, Disp: -4, Index: -1, Scale: 1, Size: 4}, int32Ty)
		}, "movslq -4(%rbp), %rax"},
		{"pushpop", func() { f.encPush(REG_R13); f.encPop(REG_R13) }, "pushq %r13\npopq %r13"},
		{"addrr", func() { f.encALURegReg("add", REG_RAX, REG_RCX) }, "addq %rcx, %rax"},
		{"subrm", func() {
			f.encALURegMem("sub", REG_RAX, Addr{RegBase: REG_RBP, Disp: -16, Index: -1, Scale: 1, Size: 8})
		}, "subq -16(%rbp), %rax"},
		{"cmpri", func() { f.encALURegImm("cmp", REG_RAX, 0) }, "cmpq $0, %rax"},
		{"imul", func() { f.encImulRegReg(REG_RAX, REG_RCX) }, "imulq %rcx, %rax"},
		{"imulimm", func() { f.encImulRegImm(REG_RAX, REG_RCX, 4) }, "imulq $4, %rcx, %rax"},
		{"neg", func() { f.encNeg(REG_RBX) }, "negq %rbx"},
		{"shift", func() { c := uint64(3); f.encShift("sal", REG_RAX, &c, true) }, "salq $3, %rax"},
		{"shiftcl", func() { f.encShift("sar", REG_RCX, nil, true) }, "sarq %cl, %rcx"},
		{"setcc", func() { f.encSetcc(0x9C, REG_RAX) }, "setl %al"},
		{"lea", func() {
			f.encLeaRegMem(REG_RAX, Addr{RegBase: REG_RBP, Disp: -32, Index: -1, Scale: 1, Size: 8})
		}, "leaq -32(%rbp), %rax"},
		{"leasib", func() {
			f.encLeaRegMem(REG_RDX, Addr{RegBase: REG_RBP, Disp: -8, Index: REG_RDI, Scale: 8, Size: 8})
		}, "leaq -8(%rbp,%rdi,8), %rdx"},
		{"callreg", func() { f.encCallReg(REG_RAX) }, "call *%rax"},
		{"cqo", func() { f.encCqo() }, "cqto"},
		{"div", func() { f.encDiv(REG_RCX, true, true) }, "idivq %rcx"},
		{"test", func() { f.encTestRegReg(REG_RAX, REG_RAX) }, "testq %rax, %rax"},
		{"movsil", func() {
			f.encMovMemReg(Addr{RegBase: REG_RBP, Disp: -1, Index: -1, Scale: 1, Size: 1}, REG_RSI, CType{Name: "char", Size: 1})
		}, "movb %sil, -1(%rbp)"},
		{"movsd", func() {
			f.encMovXMMMem(0, Addr{RegBase: REG_RBP, Disp: -8, Index: -1, Scale: 1, Size: 8}, true)
		}, "movsd -8(%rbp), %xmm0"},
		{"addsd", func() { f.encSSE(0xF2, 0x58, 0, 1) }, "addsd %xmm1, %xmm0"},
		{"ucomisd", func() { f.encUcomisd(0, 1) }, "ucomisd %xmm1, %xmm0"},
		{"cvtsi2sd", func() { f.encCvtSi2SdReg(0, REG_RCX, true) }, "cvtsi2sdq %rcx, %xmm0"},
		{"cvttsd2si", func() { f.encCvtSd2Si(REG_RAX, 0, true) }, "cvttsd2si %xmm0, %rax"},
	}

	for _, c := range cases {
		f.text = f.text[:0]
		f.relocs = f.relocs[:0]
		f.roFixups = f.roFixups[:0]
		f.fixups = f.fixups[:0]
		c.emit()
		got := append([]byte(nil), f.text...)

		// Assemble reference with GAS.
		dir, err := os.MkdirTemp("", "spassenc")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)
		asm := "    .text\n" + "    " + strings.ReplaceAll(c.asm, "\n", "\n    ") + "\n"
		if err := os.WriteFile(dir+"/t.s", []byte(asm), 0644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("as", "-o", dir+"/t.o", dir+"/t.s")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: as failed: %s\n%s", c.name, err, out)
		}
		cmd = exec.Command("objcopy", "-O", "binary", "--only-section=.text", dir+"/t.o", dir+"/t.bin")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: objcopy failed: %s\n%s", c.name, err, out)
		}
		want, err := os.ReadFile(dir + "/t.bin")
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s: got % x, want % x", c.name, got, want)
		}
	}
}
