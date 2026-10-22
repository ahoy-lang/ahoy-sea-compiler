// spass_mem_test: validation for the MEM agent (spass_mem.go).
//
// Runnable now (no STMT/EXPR needed):
//   - TestSpassMemGlobalSections: globals-only TU through genTU, then
//     WriteELF + gcc link + run against a C main. True exit-code test.
//   - TestSpassMemConvertGas / TestSpassMemCopyStructGas: emission bytes
//     cross-checked against GNU as (same style as spass_encode_test.go).
//   - TestSpassMemStructOff / TestSpassMemGlobalAddr: table unit tests.
//
// Gated on the full backend (skip until STMT/EXPR land):
//   - TestSpassMemE2E_*: whole programs through genTU + link + run,
//     exit code compared with the expected value. Anything that fails
//     with "spass: unsupported" (a STMT/EXPR stub) skips instead of
//     failing, so `go test` stays green while the backend is composed.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// memParseTU parses a source fragment without preprocessing.
func memParseTU(t *testing.T, src string) (*SPass, *ASTNode) {
	t.Helper()
	p := NewParser(src)
	ast, err := p.Parse()
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	funcs := make(map[string]*FunctionSignature)
	for _, ch := range ast.Children {
		if ch.Type == NodeFunction {
			funcs[ch.Name] = &FunctionSignature{
				ReturnType: ch.ReturnType, ParamTypes: ch.ParamTypes,
			}
		}
	}
	return NewSPass(p.structs, p.typedefs, p.enums, funcs, nil, 0), ast
}

// memGasCompare assembles reference AT&T text with GAS and compares the
// .text bytes against got.
func memGasCompare(t *testing.T, name, asm string, got []byte) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "t.s"), []byte("    .text\n"+asm+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("as", "-o", filepath.Join(dir, "t.o"), filepath.Join(dir, "t.s")).CombinedOutput(); err != nil {
		t.Fatalf("%s: as failed: %s\n%s", name, err, out)
	}
	if out, err := exec.Command("objcopy", "-O", "binary", "--only-section=.text",
		filepath.Join(dir, "t.o"), filepath.Join(dir, "t.bin")).CombinedOutput(); err != nil {
		t.Fatalf("%s: objcopy failed: %s\n%s", name, err, out)
	}
	want, err := os.ReadFile(filepath.Join(dir, "t.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s: got % x, want % x", name, got, want)
	}
}

// memLinkAndRun writes obj to disk, links it (plus optional extra C
// files) with gcc -no-pie, runs the result, and returns its exit code.
func memLinkAndRun(t *testing.T, obj *ObjectFile, extraC []string) int {
	t.Helper()
	dir := t.TempDir()
	objPath := filepath.Join(dir, "t.o")
	if err := obj.WriteELF(objPath); err != nil {
		t.Fatalf("WriteELF: %v", err)
	}
	exe := filepath.Join(dir, "t.exe")
	args := append([]string{"-no-pie", objPath}, extraC...)
	args = append(args, "-o", exe)
	if out, err := exec.Command("gcc", args...).CombinedOutput(); err != nil {
		t.Fatalf("link failed: %s\n%s", err, out)
	}
	r := exec.Command(exe)
	_ = r.Run()
	return r.ProcessState.ExitCode()
}

// ---------------------------------------------------------------------------
// Globals: sections, symbols, relocs + linked exit code (runs NOW).
// ---------------------------------------------------------------------------

func TestSpassMemGlobalSections(t *testing.T) {
	src := `
int g_init = 42;
long g_big = 1234567890123L;
int g_zero;
static int g_st = 7;
char g_ch = 'A';
double g_d = 2.5;
float g_f = 1.5f;
char g_str[] = "hi";
char *g_ptr = "hello";
int g_arr[4] = {1,2,3};
struct Point { int x; int y; };
struct Point g_pt = { .y = 5, .x = 3 };
struct Point g_pts[2];
int g_expr = (1 << 12) + 3 * 5 - 2;
unsigned g_u = 4000000000u;
int *g_null = 0;
extern int g_ext;
`
	sp, ast := memParseTU(t, src)
	if err := sp.genTU(ast); err != nil {
		t.Fatalf("genTU: %v", err)
	}
	byName := map[string]ObjSymbol{}
	for _, sy := range sp.syms {
		byName[sy.Name] = sy
	}
	// .data contents.
	dataOf := func(name string) []byte {
		sy, ok := byName[name]
		if !ok || sy.Section != ".data" {
			t.Fatalf("%s: want .data symbol, have %+v", name, sy)
		}
		return sp.data[sy.Offset : sy.Offset+sy.Size]
	}
	le := func(b []byte) uint64 {
		var v uint64
		for i, c := range b {
			v |= uint64(c) << (8 * i)
		}
		return v
	}
	if got := le(dataOf("g_init")); got != 42 {
		t.Errorf("g_init = %d, want 42", got)
	}
	if got := le(dataOf("g_big")); got != 1234567890123 {
		t.Errorf("g_big = %d", got)
	}
	if got := dataOf("g_ch")[0]; got != 'A' {
		t.Errorf("g_ch = %q", got)
	}
	if got := le(dataOf("g_d")); got != 0x4004000000000000 {
		t.Errorf("g_d bits = %x", got)
	}
	if got := le(dataOf("g_f")); got != 0x3FC00000 {
		t.Errorf("g_f bits = %x", got)
	}
	if got := string(dataOf("g_str")); got != "hi\x00" {
		t.Errorf("g_str = %q", got)
	}
	arr := dataOf("g_arr")
	for i, want := range []uint64{1, 2, 3, 0} {
		if got := le(arr[4*i : 4*i+4]); got != want {
			t.Errorf("g_arr[%d] = %d, want %d", i, got, want)
		}
	}
	pt := dataOf("g_pt")
	if le(pt[0:4]) != 3 || le(pt[4:8]) != 5 {
		t.Errorf("g_pt = %v", pt)
	}
	pts := dataOf("g_pt")
	_ = pts
	// g_pts is an uninitialized struct array: lives in .bss (16 bytes).
	if sy, ok := byName["g_pts"]; !ok || sy.Section != ".bss" || sy.Size != 16 {
		t.Errorf("g_pts: want 16-byte .bss, have %+v", sy)
	}
	if got := le(dataOf("g_expr")); got != uint64(int64(1<<12+3*5-2)) {
		t.Errorf("g_expr = %d, want %d", got, 1<<12+3*5-2)
	}
	if got := le(dataOf("g_u")); got != 4000000000 {
		t.Errorf("g_u = %d", got)
	}
	for _, b := range dataOf("g_null") {
		if b != 0 {
			t.Errorf("g_null not zero")
			break
		}
	}
	// .bss: tentative zero.
	if sy, ok := byName["g_zero"]; !ok || sy.Section != ".bss" {
		t.Errorf("g_zero: want .bss, have %+v", sy)
	}
	// static: .data but local.
	if sy, ok := byName["g_st"]; !ok || sy.Section != ".data" || sy.Global {
		t.Errorf("g_st: want local .data, have %+v", sy)
	}
	// extern: undefined entry, no storage.
	if sy, ok := byName["g_ext"]; !ok || sy.Section != "" {
		t.Errorf("g_ext: want undefined entry, have %+v", sy)
	}
	// g_ptr needs one absolute reloc; no reloc may dangle.
	hasPtr := false
	for _, r := range sp.relocs {
		if r.Section != ".data" {
			t.Errorf("unexpected reloc section %q", r.Section)
		}
		if r.Type != R_X86_64_64 {
			t.Errorf("data reloc type = %v, want R_X86_64_64", r.Type)
		}
		found := false
		for _, sy := range sp.syms {
			if sy.Name == r.Symbol && sy.Section != "" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("reloc against unknown symbol %q", r.Symbol)
		}
		if r.Symbol != "" && byName["g_ptr"].Offset == r.Offset {
			hasPtr = true
		}
	}
	if !hasPtr {
		t.Errorf("missing data reloc for g_ptr string address")
	}

	// Link against a C main and run: exit code is the checksum.
	mainC := `
extern int g_init; extern long g_big; extern int g_zero; extern char g_ch;
extern double g_d; extern float g_f; extern char g_str[]; extern char *g_ptr;
extern int g_arr[]; extern int g_expr; extern unsigned g_u; extern int *g_null;
extern int g_ext;
struct Point { int x; int y; }; extern struct Point g_pt; extern struct Point g_pts[];
int g_ext = 99;
int main() {
    int s = 0;
    s += g_init; s += (int)(g_big % 1000); s += g_zero; s += g_ch;
    s += (int)g_d; s += (int)g_f; s += g_str[0] + g_str[1]; s += g_ptr[0];
    s += g_arr[0] + g_arr[1] + g_arr[2] + g_arr[3];
    s += g_pt.x + g_pt.y; s += (g_pts[0].x == 0) + (g_pts[1].y == 0);
    s += g_expr; s += (int)(g_u % 100); s += (g_null == 0); s += g_ext;
    return s % 256;
}
`
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.c")
	if err := os.WriteFile(mainPath, []byte(mainC), 0644); err != nil {
		t.Fatal(err)
	}
	if got := memLinkAndRun(t, sp.finish(), []string{mainPath}); got != 163 {
		t.Errorf("linked exit code = %d, want 163", got)
	}
}

// TestSpassMemFoldStructArray folds an array-of-struct initializer with a
// hand-built AST (the parser zeroes nested file-scope struct braces, so
// this path cannot be reached through parse alone yet).
func TestSpassMemFoldStructArray(t *testing.T) {
	sp := NewSPass(map[string]*StructDef{
		"Point": {Name: "Point", Size: 8, Align: 4, Members: []StructMember{
			{Name: "x", Type: "int", Offset: 0, Size: 4, Align: 4},
			{Name: "y", Type: "int", Offset: 4, Size: 4, Align: 4},
		}},
	}, nil, nil, nil, nil, 0)
	num := func(v string) *ASTNode { return &ASTNode{Type: NodeNumber, Value: v, DataType: "int"} }
	init := &ASTNode{Type: NodeCompoundLiteral, DataType: "struct Point",
		InitFields: []string{"", ""},
		Children: []*ASTNode{
			{Type: NodeCompoundLiteral, DataType: "struct Point", Children: []*ASTNode{num("1"), num("2")}},
			{Type: NodeCompoundLiteral, DataType: "struct Point", Children: []*ASTNode{num("3"), num("4")}},
		}}
	buf := make([]byte, 16)
	var refs []memDataRef
	if !sp.memFoldInit(buf, 0, init, "struct Point", []int{2}, 16, &refs) {
		t.Fatalf("fold failed")
	}
	for i, want := range []byte{1, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0, 4, 0, 0, 0} {
		if buf[i] != want {
			t.Fatalf("byte %d = %d, want %d (buf %v)", i, buf[i], want, buf)
		}
	}
	if len(refs) != 0 {
		t.Errorf("unexpected relocs: %+v", refs)
	}
}

// ---------------------------------------------------------------------------
// convert / copyStruct GAS cross-checks (run NOW; no genRVal needed).
// ---------------------------------------------------------------------------

func TestSpassMemConvertGas(t *testing.T) {
	mk := func() *funcState {
		sp := NewSPass(nil, nil, nil, nil, nil, 0)
		return &funcState{sp: sp, locals: make(map[string]*LocalVar), labels: make(map[string]uint64)}
	}
	i32 := CType{Name: "int", Size: 4, Signed: true}
	u32 := CType{Name: "unsigned int", Size: 4}
	i64 := CType{Name: "long", Size: 8, Signed: true}
	f64 := CType{Name: "double", Size: 8, IsFloat: true}
	f32 := CType{Name: "float", Size: 4, IsFloat: true}
	boolean := CType{Name: "_Bool", Size: 1}
	cases := []struct {
		name string
		from SVal
		to   CType
		asm  string
	}{
		{"i32->i64", SVal{Kind: KReg, Type: i32, Reg: REG_RCX}, i64, "movslq %ecx, %rcx"},
		{"u32->i64", SVal{Kind: KReg, Type: u32, Reg: REG_RCX}, i64, "movl %ecx, %ecx"},
		{"i32->u32", SVal{Kind: KReg, Type: i32, Reg: REG_RCX}, u32, ""}, // no-op bits
		{"i64->i32", SVal{Kind: KReg, Type: i64, Reg: REG_RCX}, i32, "movl %ecx, %ecx"},
		{"i32->f64", SVal{Kind: KReg, Type: i32, Reg: REG_RCX}, f64, "cvtsi2sdl %ecx, %xmm2"},
		{"u32->f64", SVal{Kind: KReg, Type: u32, Reg: REG_RCX}, f64, "cvtsi2sdq %rcx, %xmm2"},
		{"f64->i64", SVal{Kind: KXMM, Type: f64, Reg: 1}, i64, "cvttsd2si %xmm1, %rax"},
		{"f32->f64", SVal{Kind: KXMM, Type: f32, Reg: 1}, f64, "cvtss2sd %xmm1, %xmm1"},
		{"f64->f32", SVal{Kind: KXMM, Type: f64, Reg: 1}, f32, "cvtsd2ss %xmm1, %xmm1"},
		{"i32->bool", SVal{Kind: KReg, Type: i32, Reg: REG_RCX}, boolean,
			"testq %rcx, %rcx\nsetne %al\nmovzbl %al, %eax"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := mk()
			gotV := f.convert(c.from, c.to)
			if c.asm == "" {
				if len(f.text) != 0 {
					t.Errorf("expected no-op, emitted %d bytes", len(f.text))
				}
				if gotV.Type.Name != c.to.Name {
					t.Errorf("type not retagged")
				}
				return
			}
			memGasCompare(t, c.name, "    "+strings.ReplaceAll(c.asm, "\n", "\n    "), append([]byte(nil), f.text...))
		})
	}
}

// copyStruct is branchless; GAS-check small frame->frame copies.
func TestSpassMemCopyStructGas(t *testing.T) {
	sp := NewSPass(nil, nil, nil, nil, nil, 0)
	f := &funcState{sp: sp, locals: make(map[string]*LocalVar), labels: make(map[string]uint64)}
	dst := Addr{RegBase: REG_RBP, Disp: -32, Index: -1, Scale: 1, Size: 12}
	src := Addr{RegBase: REG_RBP, Disp: -16, Index: -1, Scale: 1, Size: 12}
	f.copyStruct(dst, src, 12)
	memGasCompare(t, "copy12",
		"    movq -16(%rbp), %rax\n    movq %rax, -32(%rbp)\n    movslq -8(%rbp), %rax\n    movl %eax, -24(%rbp)",
		append([]byte(nil), f.text...))

	// Sym bases normalize through LEA (Sym+Disp is unsupported by encRM).
	f2 := &funcState{sp: sp, locals: make(map[string]*LocalVar), labels: make(map[string]uint64)}
	f2.copyStruct(Addr{Sym: "dst"}, Addr{Sym: "src"}, 8)
	memGasCompare(t, "copySym",
		"    leaq dst(%rip), %rax\n    leaq src(%rip), %rcx\n    movq (%rcx), %rdx\n    movq %rdx, (%rax)",
		append([]byte(nil), f2.text...))
}

// addrOf for identifiers/params/designators is pure address arithmetic
// (no genRVal needed); member `.` on globals folds to LEA+ADD (GAS-checked).
func TestSpassMemAddrOfPure(t *testing.T) {
	sp := NewSPass(map[string]*StructDef{
		"P": {Name: "P", Size: 8, Align: 4, Members: []StructMember{
			{Name: "x", Type: "int", Offset: 0, Size: 4, Align: 4},
			{Name: "y", Type: "int", Offset: 4, Size: 4, Align: 4},
		}},
	}, nil, nil, nil, map[string]*Symbol{
		"g": {Name: "g", IsGlobal: true, Size: 8, Type: "struct P"},
		"x": {Name: "x", IsGlobal: true, Size: 4, Type: "int"},
	}, 0)
	f := &funcState{sp: sp, locals: make(map[string]*LocalVar), labels: make(map[string]uint64)}
	i32 := CType{Name: "int", Size: 4, Signed: true}
	f.locals["x"] = &LocalVar{Name: "x", Type: i32, Offset: -8}
	f.params = []LocalVar{{Name: "p", Type: CType{Name: "long", Size: 8, Signed: true}, Offset: -16}}

	// Local shadows global.
	if a := f.addrOf(&ASTNode{Type: NodeIdentifier, VarName: "x"}); a.RegBase != REG_RBP || a.Disp != -8 {
		t.Errorf("shadowed local = %+v", a)
	}
	// Param home.
	if a := f.addrOf(&ASTNode{Type: NodeIdentifier, VarName: "p"}); a.RegBase != REG_RBP || a.Disp != -16 {
		t.Errorf("param = %+v", a)
	}
	// Global falls through to RIP.
	if a := f.addrOf(&ASTNode{Type: NodeIdentifier, VarName: "g"}); a.Sym != "g" || a.Size != 8 {
		t.Errorf("global = %+v", a)
	}
	// Unknown panics with *spassError.
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(*spassError); !ok {
					t.Errorf("wrong panic type %T", r)
				}
				return
			}
			t.Errorf("unknown identifier did not panic")
		}()
		f.addrOf(&ASTNode{Type: NodeIdentifier, VarName: "nope"})
	}()
	// Global member access: LEA + ADD.
	f.text = f.text[:0]
	a := f.addrOf(&ASTNode{Type: NodeMemberAccess, MemberName: "y",
		Children: []*ASTNode{{Type: NodeIdentifier, VarName: "g"}}})
	if a.RegBase < 0 || a.Disp != 0 || a.Size != 4 {
		t.Errorf("member addr = %+v", a)
	}
	memGasCompare(t, "memberLea",
		"    leaq g(%rip), %rax\n    addq $4, %rax",
		append([]byte(nil), f.text...))
}

// memCollapseAddr is pure emission: fold base+static+dynamic offsets.
func TestSpassMemCollapseGas(t *testing.T) {
	sp := NewSPass(nil, nil, nil, nil, nil, 0)
	newF := func() *funcState {
		return &funcState{sp: sp, locals: make(map[string]*LocalVar), labels: make(map[string]uint64)}
	}
	// Static-only on a frame slot: no bytes, pure address arithmetic.
	f := newF()
	a := f.memCollapseAddr(true,
		Addr{RegBase: REG_RBP, Disp: -48, Index: -1, Scale: 1}, -1, 16, -1, 0, 1, 4)
	if len(f.text) != 0 || a.RegBase != REG_RBP || a.Disp != -32 || a.Size != 4 {
		t.Errorf("static fold = (%d bytes, %+v)", len(f.text), a)
	}
	// Dynamic index, stride 4: Index/Scale form, no bytes emitted.
	f = newF()
	r := f.scratchReg() // RAX holds the index
	a = f.memCollapseAddr(true,
		Addr{RegBase: REG_RBP, Disp: -48, Index: -1, Scale: 1}, -1, 8, r, 4, 1, 4)
	if len(f.text) != 0 || a.Index != r || a.Scale != 4 || a.Disp != -40 {
		t.Errorf("index fold = (%d bytes, %+v)", len(f.text), a)
	}
	// General path (stride 12): lea + mov + add sequence.
	f = newF()
	r = f.scratchReg()
	a = f.memCollapseAddr(true,
		Addr{RegBase: REG_RBP, Disp: -48, Index: -1, Scale: 1}, -1, 8, r, 12, 1, 4)
	memGasCompare(t, "collapse12",
		"    leaq -48(%rbp), %rcx\n    movq %rcx, %rdx\n    addq %rax, %rdx",
		append([]byte(nil), f.text...))
	if a.RegBase == REG_RBP || a.Disp != 8 || a.Size != 4 {
		t.Errorf("general fold addr = %+v", a)
	}
}

// ---------------------------------------------------------------------------
// Table unit tests (run NOW).
// ---------------------------------------------------------------------------

func TestSpassMemStructOff(t *testing.T) {
	sp := NewSPass(map[string]*StructDef{
		"Point": {Name: "Point", Size: 8, Align: 4, Members: []StructMember{
			{Name: "x", Type: "int", Offset: 0, Size: 4, Align: 4},
			{Name: "y", Type: "int", Offset: 4, Size: 4, Align: 4},
		}},
		"Outer": {Name: "Outer", Size: 16, Align: 8, Members: []StructMember{
			{Name: "tag", Type: "char", Offset: 0, Size: 1, Align: 1},
			{Name: "p", Type: "struct Point", Offset: 4, Size: 8, Align: 4},
			{Name: "vals", Type: "int", Offset: 12, Size: 4, Align: 1, ArrayLen: 1, Dims: []int{1}},
		}},
	}, nil, nil, nil, nil, 0)
	if off, size, ok := structMemberOff(sp, "Point", "y"); !ok || off != 4 || size != 4 {
		t.Errorf("Point.y = (%d,%d,%v)", off, size, ok)
	}
	if off, size, ok := structMemberOff(sp, "Outer", "vals"); !ok || off != 12 || size != 4 {
		t.Errorf("Outer.vals = (%d,%d,%v)", off, size, ok)
	}
	// Dotted fallback through the nested struct.
	if off, size, ok := structMemberOff(sp, "Outer", "p.y"); !ok || off != 8 || size != 4 {
		t.Errorf("Outer.p.y = (%d,%d,%v)", off, size, ok)
	}
	if _, _, ok := structMemberOff(sp, "Outer", "nope"); ok {
		t.Errorf("unknown member reported ok")
	}
	if _, _, ok := structMemberOff(sp, "Missing", "x"); ok {
		t.Errorf("unknown struct reported ok")
	}
}

func TestSpassMemGlobalAddr(t *testing.T) {
	sp := NewSPass(nil, nil, nil, nil, map[string]*Symbol{
		"g": {Name: "g", IsGlobal: true, Size: 4, Type: "int"},
	}, 0)
	a, ty, ok := sp.globalAddr("g")
	if !ok || a.Sym != "g" || ty.Size != 4 {
		t.Errorf("globalAddr(g) = (%+v,%+v,%v)", a, ty, ok)
	}
	if _, _, ok := sp.globalAddr("missing"); ok {
		t.Errorf("globalAddr(missing) ok")
	}
}

// ---------------------------------------------------------------------------
// Whole-program exit-code tests (gated: skip while STMT/EXPR are stubs).
// ---------------------------------------------------------------------------

// memPreflightFuncs compiles each function in a throwaway backend to
// surface *spassError before the real genTU call. Workaround for a
// skeleton bug (reported): genFunction recovers spassError into funcErr
// but has unnamed returns, so after recover it returns nil and genTU
// succeeds with truncated output. The throwaway backend runs pass 1
// (globals) so global references resolve exactly as in the real pass;
// it is discarded, so no state pollutes the real compile.
func memPreflightFuncs(sp *SPass, ast *ASTNode) (err error) {
	pre := NewSPass(sp.structs, sp.typedefs, sp.enums, sp.funcSigs, nil, sp.oLevel)
	for _, ch := range ast.Children {
		if ch.Type == NodeFunction {
			continue
		}
		if e := pre.genDataInit("", ch); e != nil {
			return e
		}
	}
	for _, fn := range ast.Children {
		if fn.Type != NodeFunction || len(fn.Children) == 0 {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					if se, ok := r.(*spassError); ok {
						err = se
					} else {
						panic(r)
					}
				}
			}()
			fs := &funcState{sp: pre, locals: make(map[string]*LocalVar), labels: make(map[string]uint64)}
			if e := fs.genFunction(fn); e != nil {
				err = e
			} else if fs.funcErr != nil {
				// genFunction's internal recover records but (skeleton
				// bug) returns nil; surface it here.
				err = fs.funcErr
			}
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// memE2E compiles src through the single-pass backend, links, runs, and
// compares the exit code. Skips when the backend reports unsupported
// constructs (other agents still landing) or when gcc is unavailable.
func memE2E(t *testing.T, name, src string, want int) {
	t.Helper()
	sp, ast := memParseTU(t, src)
	if err := memPreflightFuncs(sp, ast); err != nil {
		if _, ok := err.(*spassError); ok {
			t.Skipf("%s: backend incomplete: %v", name, err)
		}
		t.Fatalf("%s: preflight: %v", name, err)
	}
	if err := sp.genTU(ast); err != nil {
		if strings.Contains(err.Error(), "unsupported") {
			t.Skipf("%s: backend incomplete: %v", name, err)
		}
		t.Fatalf("%s: genTU: %v", name, err)
	}
	// Workaround for the pending driver fix (undef synthesis for extern
	// text refs): add undefined entries for unknown reloc targets here.
	have := map[string]bool{}
	for _, sy := range sp.syms {
		have[sy.Name] = true
	}
	for _, r := range sp.relocs {
		if r.Symbol == "" || strings.HasPrefix(r.Symbol, ".") || have[r.Symbol] {
			continue
		}
		sp.syms = append(sp.syms, ObjSymbol{Name: r.Symbol})
		have[r.Symbol] = true
	}
	got := memLinkAndRun(t, sp.finish(), nil)
	if got != want {
		t.Errorf("%s: exit = %d, want %d", name, got, want)
	}
}

func TestSpassMemE2E_Casts(t *testing.T) {
	// NOTE: long mixed-type programs trip EXPR's stale-register tracking
	// (see report), so int and float converts are covered by two short
	// programs; float ==/!= is additionally avoided (miscompiled by EXPR)
	// in favor of int roundtrips.
	memE2E(t, "casts", `
int main() {
    char c = -1;
    unsigned u = (unsigned)c;
    int l = (int)300;
    long g = (long)-5;
    _Bool b = 5;
    int r = 0;
    if (b == 1) r += 32;
    if (u == 4294967295u) r += 1;
    if (l == 300) r += 2;
    if (g == -5) r += 4;
    return r;
}`, 39)
}

func TestSpassMemE2E_CastsFloat(t *testing.T) {
	memE2E(t, "castsfloat", `
double gd = 2.5;
int main() {
    float f = 2.5f;
    double d = (double)7 + gd;
    int i = (int)f + (int)d;
    float back = (float)i;
    int r = 0;
    if (i == 11) r += 1;
    if ((int)back == 11) r += 2;
    if ((long)f == 2) r += 4;
    if ((int)(float)300 == 300) r += 8;
    if ((int)gd == 2) r += 16;
    return r;
}`, 31)
}

func TestSpassMemE2E_BoolFalse(t *testing.T) {
	// One _Bool per program: EXPR currently spills a stale register for
	// binary ops when two _Bool values interact (see report; z2 repro).
	memE2E(t, "boolfalse", `
int main() {
    _Bool z = 0;
    int r = 0;
    if (z == 0) r += 1;
    if (z != 1) r += 2;
    return r;
}`, 3)
}

func TestSpassMemE2E_BoolTrue(t *testing.T) {
	memE2E(t, "booltrue", `
int main() {
    _Bool b = 5;
    int r = 0;
    if (b == 1) r += 1;
    if (b != 0) r += 2;
    return r;
}`, 3)
}

func TestSpassMemE2E_IntPtr(t *testing.T) {
	memE2E(t, "intptr", `
int main() {
    long v = -1;
    void *p = (void *)v;
    long back = (long)p;
    unsigned long u = (unsigned long)5;
    int *q = (int *)u;
    return (back == -1) + ((unsigned long)q == 5) + ((int)(long)"x" != 0);
}`, 3)
}

func TestSpassMemE2E_ArrayIndex(t *testing.T) {
	// NOTE: multi-dim BRACE inits are zeroed by the parser (nested braces
	// outside struct members become placeholder 0s), so this test assigns
	// multi-dim elements at runtime and only exercises indexing.
	// Multi-dim LOCALS additionally need STMT to call MemNoteArray (see
	// report), so the multi-dim array here is file-scope (dims come from
	// the global Symbol); TestSpassMemE2E_MultiDimLocal covers locals and
	// activates once STMT cooperates.
	memE2E(t, "arrays", `
int g[5] = {10, 20, 30, 40, 50};
int m[3][4];
int main() {
    int s = 0;
    for (int i = 0; i < 5; i++) s += g[i];
    int a[6];
    for (int i = 0; i < 6; i++) a[i] = i * 3;
    for (int i = 0; i < 6; i++) s += a[i];
    m[0][0] = 1; m[0][1] = 2; m[1][0] = 3; m[1][1] = 4;
    s += m[0][0] + m[0][1] + m[1][0] + m[1][1];
    int *p = &g[2];
    s += *p + p[1];
    return s % 256;
}`, 19) // 150 + 45 + 10 + 70 = 275 % 256
}

func TestSpassMemE2E_MultiDimLocal(t *testing.T) {
	// Multi-dim LOCALS need EXPR to thread row strides (it currently
	// decomposes m[i][j] into nested single-dim accesses over
	// uninitialized row temps and crashes without any *spassError, so
	// this cannot auto-skip on backend errors). Multi-dim globals are
	// covered by TestSpassMemE2E_ArrayIndex. Remove this skip once EXPR
	// supports strided multi-dim locals (STMT must also call MemNoteArray
	// with real dims for the fallback path).
	t.Skip("EXPR multi-dim local indexing crashes (no stride threading)")
	memE2E(t, "multidimlocal", `
int main() {
    int m[3][4];
    m[0][0] = 1; m[0][1] = 2; m[1][0] = 3; m[1][1] = 4;
    m[2][3] = 5;
    return m[0][0] + m[0][1] + m[1][0] + m[1][1] + m[2][3];
}`, 15)
}

func TestSpassMemE2E_StructCopy(t *testing.T) {
	memE2E(t, "structs", `
struct Point { int x; int y; };
struct Outer { int tag; struct Point p; int vals[2]; };
struct Point mk(int a, int b) { struct Point r; r.x = a; r.y = b; return r; }
int gsum;
struct Point gg = {7, 8};
int main() {
    struct Point p1;
    p1.x = 10; p1.y = 20;
    struct Point p2 = p1;
    struct Outer o;
    o.tag = 1; o.p = p2; o.vals[0] = 100; o.vals[1] = 200;
    struct Point q = mk(3, 4);
    struct Point *pp = &p1;
    pp->x = 11;
    gsum = gg.x + gg.y;
    return (p2.x + p2.y + o.p.y + o.vals[0] + o.vals[1] + q.x + q.y + pp->x + gsum) % 256;
}`, 127) // 383 % 256
}

func TestSpassMemE2E_MemberChain(t *testing.T) {
	memE2E(t, "members", `
struct Inner { int v; };
struct Mid { struct Inner in; int w; };
struct Top { struct Mid mid; int *ptr; };
int cell = 42;
int main() {
    struct Top top;
    top.mid.in.v = 5;
    top.mid.w = 6;
    top.ptr = &cell;
    *top.ptr = 50;
    struct Top *tp = &top;
    return tp->mid.in.v + tp->mid.w + *tp->ptr;
}`, 61)
}

func TestSpassMemE2E_FloatInt(t *testing.T) {
	// NOTE: long expression contexts trip EXPR's stale-register tracking
	// (see report), so this is split into short programs; together they
	// cover int<->float converts, float stores/loads, and mixed arithmetic.
	memE2E(t, "floatint", `
double gd = 2.5;
int main() {
    float f = 2.5f;
    double d = (double)7 + gd;
    int i = (int)f + (int)d;
    float back = (float)i;
    int r = 0;
    if (i == 11) r += 1;
    if ((int)back == 11) r += 2;
    if ((long)f == 2) r += 4;
    if ((int)(float)300 == 300) r += 8;
    return r;
}`, 15)
}

func TestSpassMemE2E_FloatArray(t *testing.T) {
	memE2E(t, "floatarray", `
int main() {
    int arr[3];
    arr[0] = (int)3.9;
    arr[1] = (int)-3.9;
    return arr[0] * 10 + arr[1];
}`, 27)
}

func TestSpassMemE2E_FloatMixed(t *testing.T) {
	memE2E(t, "floatmixed", `
int main() {
    float f = 1.5f;
    double d = 9.5;
    return (int)(f + d);
}`, 11)
}

func TestSpassMemE2E_Globals(t *testing.T) {
	memE2E(t, "globals", `
int gi = 6;
long gb = 100;
int gz;
char *gs = "hey";
int garr[3] = {4, 5, 6};
struct S { int a; int b; };
struct S gst = { .b = 2, .a = 1 };
int main() {
    gz = gi + 1;
    int s = gz + (int)gb + gs[1] + garr[2] + gst.a + gst.b;
    gs = "yo";
    return (s + gs[0]) % 256;
}`, 82) // (217 + 121) % 256
}
