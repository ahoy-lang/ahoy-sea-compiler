// fastasm_test validates the in-process assembler against GNU as:
// .text bytes must match exactly, and the object must link with GNU ld.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func faHaveTool(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func faSectionBytes(t *testing.T, objPath, section, outPath string) []byte {
	t.Helper()
	cmd := exec.Command("objcopy", "-O", "binary", "--only-section="+section, objPath, outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("objcopy %s %s: %v\n%s", section, objPath, err, out)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read %s: %v", outPath, err)
	}
	return b
}

// TestFastAsmSmall checks exact encodings of tricky forms against bytes
// verified with GNU as + objdump.
func TestFastAsmSmall(t *testing.T) {
	asm := `    .section .rodata
.str_1:
    .string "hi"
    .align 8
.FC1:
    .double 1.5
    .text
    .globl myfunc
    .type myfunc, @function
myfunc:
    pushq %rbp
    movq %rsp, %rbp
    andq $0170000, %rsi
    andq $0xC0, %r9
    addq $8, %rax
    addq $1000, %rax
    subq $1000, %rax
    cmpq $5, %rax
    andq $1000, %rax
    orq $1000, %rax
    xorq $1000, %rax
    movq $5, %rax
    movq $0x123456789, %rax
    movq $18446744073709551615, %rax
    movq $0775, %rax
    cmpq $0040000, %rsi
    movabs $5, %rax
    leaq .str_1(%rip), %rax
    movq .FC1(%rip), %xmm0
    call myfunc
    call printf
    call *%r10
    jmp .L_lab
.L_lab:
    jz .L_lab2
    nop
.L_lab2:
    salq $1, %rbx
    sarq $1, %rsi
    salq $8, %r10
    sarq %cl, %rdi
    movzbq %al, %rsi
    movzbq %r10b, %r13
    movzbl (%rdx, %rdi, 1), %r14d
    movzwl -458(%rbp), %r12d
    movzwq %cx, %r9
    movsbl (%rcx), %r9d
    movswl %r14w, %ecx
    movswq %r10w, %rsi
    movslq %r9d, %rsi
    movq %r12, %xmm1
    movq %xmm0, %rcx
    movq (%r12), %xmm0
    movq %xmm0, (%r10)
    movd %ebx, %xmm0
    movd %xmm0, %r9d
    cvtsi2sd %r13d, %xmm0
    cvtsi2sdq %rsi, %xmm0
    cvttsd2si %xmm0, %eax
    cvtss2sd -76(%rbp), %xmm0
    ucomisd %xmm1, %xmm0
    addsd %xmm1, %xmm0
    subsd %xmm1, %xmm0
    mulsd %xmm1, %xmm0
    divsd %xmm1, %xmm0
    movsd %xmm0, -48(%rbp)
    movsd -224(%rbp), %xmm0
    movsd %xmm0, (%r10)
    movb %al, (%rdx, %r10, 1)
    movb %sil, (%r8)
    movw %ax, (%r10)
    movw %r12w, (%r10)
    movl %eax, (%rdx, %r10, 1)
    movl (%rdx, %rbx, 1), %r9d
    movl %eax, (%rsp)
    movq %rax, 8(%rsp)
    cmpl %esi, %r15d
    cmpl $3, %r8d
    xorl %edx, %edx
    testq %rsi, %rsi
    cdq
    cqto
    idivl %r11d
    divl %ebx
    divq %r15
    idivq %r11
    negq %rbx
    negq %r9
    imulq $4, %r10
    imulq $60000, %r10
    imulq %rsi, %rdx
    shlq $2, %rax
    shrq %cl, %rax
    mov $1, %eax
    movb (%rax), %cl
    movw (%rax), %cx
    movl $5, (%rax)
    movq $5, (%rax)
    leaq (%rdx, %rcx, 1), %rax
    seta %al
    setb %al
    setbe %al
    setg %al
    setae %al
    setp %dl
    setne %al
    setle %al
    ja .L_back
    jb .L_back
    jbe .L_back
    jae .L_back
    jl .L_back
    jle .L_back
    jg .L_back
    jge .L_back
.L_back:
    nop
    call static_fn
    movl glob_d(%rip), %ecx
    movl stat_d(%rip), %ecx
    movq glob_d+4(%rip), %rax
    ret
    .size myfunc, .-myfunc
    .type static_fn, @function
static_fn:
    ret
    .size static_fn, .-static_fn
    .data
    .globl glob_d
    .align 4
glob_d:
    .long 7
    .long 8
    .local stat_d
    .align 4
stat_d:
    .long 9
    .text
    .globl farfunc
    .type farfunc, @function
farfunc:
    jmp .L_far
` + strings.Repeat("    nop\n", 200) + `.L_far:
    ret
    .size farfunc, .-farfunc
    .comm mycommon,32,8
`
	obj, err := AssembleObject(asm)
	if err != nil {
		t.Fatalf("AssembleObject: %v", err)
	}
	if err := obj.WriteELF("/tmp/fa_small_fast.o"); err != nil {
		t.Fatalf("WriteELF: %v", err)
	}
	if !faHaveTool("as") {
		t.Skip("no as")
	}
	if err := os.WriteFile("/tmp/fa_small.s", []byte(asm), 0644); err != nil {
		t.Fatalf("write asm: %v", err)
	}
	if out, err := exec.Command("as", "-o", "/tmp/fa_small_gnu.o", "/tmp/fa_small.s").CombinedOutput(); err != nil {
		t.Fatalf("as: %v\n%s", err, out)
	}
	fast := faSectionBytes(t, "/tmp/fa_small_fast.o", ".text", "/tmp/fa_small_f.bin")
	gnu := faSectionBytes(t, "/tmp/fa_small_gnu.o", ".text", "/tmp/fa_small_g.bin")
	if !bytes.Equal(fast, gnu) {
		for i := 0; i < len(fast) && i < len(gnu); i++ {
			if fast[i] != gnu[i] {
				lo := i - 16
				if lo < 0 {
					lo = 0
				}
				hi := i + 16
				if hi > len(fast) {
					hi = len(fast)
				}
				hi2 := i + 16
				if hi2 > len(gnu) {
					hi2 = len(gnu)
				}
				t.Fatalf("first .text mismatch at byte %d (len fast=%d gnu=%d): fast=%02x gnu=%02x\nfast: % x\ngnu:  % x",
					i, len(fast), len(gnu), fast[i], gnu[i], fast[lo:hi], gnu[lo:hi2])
			}
		}
		t.Fatalf("length mismatch: fast=%d gnu=%d", len(fast), len(gnu))
	}
	t.Logf(".text match, %d bytes", len(fast))
	// Relocation sanity: both objects should expose the same reloc count.
	fastR := faReadelfRelocs(t, "/tmp/fa_small_fast.o")
	gnuR := faReadelfRelocs(t, "/tmp/fa_small_gnu.o")
	if fastR != gnuR {
		t.Fatalf("relocation count mismatch: fast=%d gnu=%d", fastR, gnuR)
	}
	t.Logf("relocs: %d (both)", fastR)
}

func faReadelfRelocs(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command("readelf", "-r", path).CombinedOutput()
	if err != nil {
		t.Fatalf("readelf -r %s: %v", path, err)
	}
	n := 0
	for _, ln := range bytes.Split(out, []byte{'\n'}) {
		if bytes.Contains(ln, []byte("R_X86_64_")) {
			n++
		}
	}
	return n
}

// TestFastAsmEditor assembles the full 49MB compiler output and compares
// .text bytes against GNU as, then links the result.
func TestFastAsmEditor(t *testing.T) {
	if !faHaveTool("as") || !faHaveTool("objcopy") {
		t.Skip("need as and objcopy")
	}
	data, err := os.ReadFile("/tmp/editor.s")
	if err != nil {
		t.Skipf("no /tmp/editor.s: %v", err)
	}
	t0 := time.Now()
	obj, err := AssembleObject(string(data))
	if err != nil {
		t.Fatalf("AssembleObject: %v", err)
	}
	tAsm := time.Since(t0)
	t.Logf("AssembleObject: %d text bytes, %d rodata, %d data, %d syms, %d relocs in %v",
		len(obj.Text), len(obj.Rodata), len(obj.Data), len(obj.Symbols), len(obj.Relocs), tAsm)
	if err := obj.WriteELF("/tmp/editor_fast.o"); err != nil {
		t.Fatalf("WriteELF: %v", err)
	}
	t1 := time.Now()
	cmd := exec.Command("as", "-o", "/tmp/editor_gnu.o", "/tmp/editor.s")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("GNU as failed: %v\n%s", err, out)
	}
	tAs := time.Since(t1)
	t.Logf("AssembleObject time: %v, GNU as time: %v", tAsm, tAs)
	fast := faSectionBytes(t, "/tmp/editor_fast.o", ".text", "/tmp/editor_fast_text.bin")
	gnu := faSectionBytes(t, "/tmp/editor_gnu.o", ".text", "/tmp/editor_gnu_text.bin")
	if len(fast) != len(gnu) {
		t.Fatalf(".text length mismatch: fast=%d gnu=%d", len(fast), len(gnu))
	}
	for i := 0; i < len(fast); i++ {
		if fast[i] != gnu[i] {
			lo := i - 16
			if lo < 0 {
				lo = 0
			}
			hi := i + 48
			if hi > len(fast) {
				hi = len(fast)
			}
			t.Fatalf("first .text mismatch at byte %d/%d: fast=%02x gnu=%02x\nfast: % x\ngnu:  % x",
				i, len(fast), fast[i], gnu[i], fast[lo:hi], gnu[lo:hi])
		}
	}
	t.Logf(".text MATCHES GNU as exactly (%d bytes)", len(fast))

	if !faHaveTool("gcc") {
		t.Skip("no gcc for link test")
	}
	// The object must be accepted by the linker. Undefined symbols are
	// allowed for this check only.
	cmd = exec.Command("gcc", "-no-pie", "/tmp/editor_fast.o", "-o", "/tmp/editor_fast_exe",
		"-Wl,--unresolved-symbols=ignore-all")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("link failed: %v\n%s", err, out)
	}
	t.Logf("link succeeded: /tmp/editor_fast_exe")
	// Best effort: link with the real backend libraries if present.
	var libs []string
	for _, l := range []string{
		"/home/lee/Documents/ahoy-lang/bell/repos/SDL3/build/libSDL3.a",
		"/home/lee/Documents/ahoy-lang/bell/repos/box2d/lib/linux/libbox2d.a",
		"/home/lee/Documents/ahoy-lang/bell/repos/box3d/lib/linux/libbox3d.a",
		"/home/lee/Documents/clibs/raylib/src/libraylib.a",
		"/home/lee/Documents/ahoy/repos/raylib/src/libraylib.a",
	} {
		if _, err := os.Stat(l); err == nil {
			libs = append(libs, l)
		}
	}
	if len(libs) > 0 {
		args := append([]string{"-no-pie", "/tmp/editor_fast.o", "-o", "/tmp/editor_fast_exe_libs"},
			libs...)
		args = append(args, "-lm", "-lpthread", "-ldl", "-Wl,--unresolved-symbols=ignore-all")
		cmd = exec.Command("gcc", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("link with backend libs failed (non-fatal): %v\n%s", err, out)
		} else {
			t.Logf("link with backend libs succeeded")
		}
	}
	fmt.Println("fastasm editor validation complete")
}
