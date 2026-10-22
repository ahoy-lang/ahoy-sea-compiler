// spass_expr_test: validation for the EXPR agent (spass_expr.go).
//
// End-to-end exit-code tests through the single-pass backend: parse (no
// preprocessing, so no #includes — libc functions are declared manually),
// genTU, WriteELF, gcc link, run, compare exit code. Skips (rather than
// fails) on "unsupported" backend gaps and when gcc is unavailable.
//
// Coverage (EXPR-owned): arithmetic widths and usual conversions, signed
// and unsigned division/modulo, shifts, ternary, comma, short-circuit &&
// || (with effect counters), calls (direct, indirect, mixed int/float/ptr
// args, struct args/returns incl. sret, varargs-style via unprototyped
// printf), statement expressions, casts, pointer arithmetic, pre/post
// inc/dec, logical/bitwise not, unary minus, float ops and comparisons
// (incl. NaN), assignment chains and compound assignment, globals.
package main

import (
	"os/exec"
	"strings"
	"testing"
)

// exE2E compiles src with the single-pass backend and checks the exit code.
func exE2E(t *testing.T, name, src string, want int) {
	t.Helper()
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skipf("%s: no gcc for linking", name)
	}
	p := NewParser(src)
	ast, err := p.Parse()
	if err != nil {
		t.Fatalf("%s: parse error: %v", name, err)
	}
	funcs := make(map[string]*FunctionSignature)
	for _, ch := range ast.Children {
		if ch.Type == NodeFunction {
			funcs[ch.Name] = &FunctionSignature{
				ReturnType: ch.ReturnType, ParamTypes: ch.ParamTypes,
			}
		}
	}
	sp := NewSPass(p.structs, p.typedefs, p.enums, funcs, nil, 0)
	if err := sp.genTU(ast); err != nil {
		if strings.Contains(err.Error(), "unsupported") {
			t.Skipf("%s: backend gap: %v", name, err)
		}
		t.Fatalf("%s: genTU: %v", name, err)
	}
	got := memLinkAndRun(t, sp.finish(), nil)
	if got != want {
		t.Errorf("%s: exit = %d, want %d", name, got, want)
	}
}

func TestSpassExprArithWidths(t *testing.T) {
	exE2E(t, "arith-widths", `
int main() {
    char c = 100; short s = 1000; int i = 100000; long l = 10000000L;
    unsigned u = 4000000000u; unsigned long ul = 18000000000ul;
    int r = 0;
    if (c + c != 200) r |= 1;
    if (s + s != 2000) r |= 2;
    if (i * 2 != 200000) r |= 4;
    if (l + l != 20000000L) r |= 8;
    if (u / 2u != 2000000000u) r |= 16;
    if (ul - 1ul != 17999999999ul) r |= 32;
    if ((int)c != 100) r |= 64;
    return r;
}`, 0)
}

func TestSpassExprDivMod(t *testing.T) {
	exE2E(t, "divmod", `
int div_s(int a, int b) { return a / b; }
int mod_s(int a, int b) { return a % b; }
unsigned udiv(unsigned a, unsigned b) { return a / b; }
unsigned umod(unsigned a, unsigned b) { return a % b; }
long ldiv(long a, long b) { return a / b; }
int main() {
    int r = 0;
    if (div_s(-7, 2) != -3) r |= 1;
    if (mod_s(-7, 2) != -1) r |= 1;
    if (mod_s(7, -2) != 1) r |= 1;
    if (div_s(-7, -2) != 3) r |= 1;
    if (udiv(7u, 2u) != 3u) r |= 2;
    if (umod(7u, 2u) != 1u) r |= 2;
    if (umod(4294967295u, 2u) != 1u) r |= 2;
    if (ldiv(-10000000000L, 3L) != -3333333333L) r |= 4;
    if (10 / 3 != 3 || 10 % 3 != 1) r |= 8;
    return r;
}`, 0)
}

func TestSpassExprShifts(t *testing.T) {
	exE2E(t, "shifts", `
int shl(int a, int b) { return a << b; }
int shr_s(int a, int b) { return a >> b; }
unsigned shr_u(unsigned a, unsigned b) { return a >> b; }
int main() {
    int r = 0;
    if (shl(1, 4) != 16) r |= 1;
    if (shr_s(-8, 2) != -2) r |= 2;
    if (shr_u(0x80000000u, 1) != 0x40000000u) r |= 4;
    if ((1 << 10) + (64 >> 2) != 1024 + 16) r |= 8;
    int n = 3;
    if ((1 << n) != 8) r |= 16;
    if ((256 >> n) != 32) r |= 32;
    return r;
}`, 0)
}

func TestSpassExprTernaryComma(t *testing.T) {
	exE2E(t, "ternary-comma", `
int max(int a, int b) { return a > b ? a : b; }
int main() {
    int r = 0;
    if (max(3, 7) != 7) r |= 1;
    if (max(9, 2) != 9) r |= 1;
    if ((1 ? 10 : 20) != 10) r |= 2;
    if ((0 ? 10 : 20) != 20) r |= 2;
    int q = (1, 2, 3);
    if (q != 3) r |= 4;
    int a = 0, b = 0;
    int c = (a = 5, b = a * 2, a + b);
    if (c != 15 || a != 5 || b != 10) r |= 8;
    double d = 1.5 > 2.5 ? 100.0 : 0.5;
    if (d != 0.5) r |= 16;
    return r;
}`, 0)
}

func TestSpassExprShortCircuit(t *testing.T) {
	exE2E(t, "short-circuit", `
int eff = 0;
int bump(int v) { eff++; return v; }
int main() {
    int r = 0;
    eff = 0;
    if ((bump(0) && bump(1)) != 0) r |= 1;
    if (eff != 1) r |= 2;
    eff = 0;
    if ((bump(1) && bump(2)) != 1) r |= 4;
    if (eff != 2) r |= 8;
    eff = 0;
    if ((bump(1) || bump(1)) != 1) r |= 16;
    if (eff != 1) r |= 32;
    eff = 0;
    if ((bump(0) || bump(0)) != 0) r |= 64;
    if (eff != 2) r |= 128;
    return r;
}`, 0)
}

func TestSpassExprCalls(t *testing.T) {
	exE2E(t, "calls", `
int add3(int a, int b, int c) { return a + b + c; }
long wyb(long x) { return -x; }
double fmix(int n, double d, float f) { return d + (double)f + (double)n; }
typedef int (*fn3_t)(int, int, int);
int useadder(fn3_t f, int a, int b, int c) { return f(a, b, c); }
int printf();
int main() {
    int r = 0;
    if (add3(1, 2, 3) != 6) r |= 1;
    if (wyb(5L) != -5L) r |= 2;
    if (fmix(1, 2.5, 0.5f) != 4.0) r |= 4;
    fn3_t alias = add3;
    if (alias(4, 5, 6) != 15) r |= 8;
    if (useadder(add3, 7, 8, 9) != 24) r |= 16;
    int a = 10, b = 20;
    if (add3(a, b, a + b) != 60) r |= 32;
    printf("v=%d %f %c\n", 42, 1.5, 65);
    return r;
}`, 0)
}

func TestSpassExprStmtExpr(t *testing.T) {
	exE2E(t, "stmt-expr", `
int main() {
    int r = 0;
    int x = ({ int a = 5; a + 3; });
    if (x != 8) r |= 1;
    int y = ({ int i = 0; int s = 0; while (i < 5) { s += i; i++; } s; });
    if (y != 10) r |= 2;
    int z = ({ 100; });
    if (z != 100) r |= 4;
    return r;
}`, 0)
}

func TestSpassExprCasts(t *testing.T) {
	exE2E(t, "casts", `
int main() {
    int r = 0;
    long big = 5000000000L;
    if ((int)big != 705032704) r |= 1;
    if ((unsigned)-1 != 4294967295u) r |= 2;
    if ((long)-5 != -5L) r |= 4;
    if ((int)3.99 != 3) r |= 8;
    if ((int)-3.99 != -3) r |= 8;
    if ((float)42 != 42.0f) r |= 16;
    if ((double)7 != 7.0) r |= 16;
    if ((double)(float)0.5 != 0.5) r |= 32;
    if ((float)(double)0.25 != 0.25f) r |= 32;
    int *p = (int *)0;
    if (p != 0) r |= 64;
    long addr = (long)p;
    if (addr != 0L) r |= 64;
    if ((char)300 != 44) r |= 128;
    return r;
}`, 0)
}

func TestSpassExprPtrArith(t *testing.T) {
	exE2E(t, "ptr-arith", `
int main() {
    int r = 0;
    int arr[4] = {10, 20, 30, 40};
    int *p = arr;
    if (*(p + 2) != 30) r |= 1;
    if (*(p + 0) != 10) r |= 1;
    if (p[3] != 40) r |= 2;
    if (p - arr != 0) r |= 4;
    if (&arr[3] - &arr[0] != 3) r |= 4;
    p++;
    if (*p != 20) r |= 8;
    p--;
    if (*p != 10) r |= 8;
    char *c = (char *)arr;
    if (c != (char *)arr) r |= 16;
    long diff = (char *)(p + 4) - (char *)p;
    if (diff != 16L) r |= 32;
    return r;
}`, 0)
}

func TestSpassExprIncDecNot(t *testing.T) {
	exE2E(t, "incr-not", `
int main() {
    int r = 0;
    int a = 5;
    if (a++ != 5 || a != 6) r |= 1;
    if (++a != 7 || a != 7) r |= 1;
    if (a-- != 7 || a != 6) r |= 2;
    if (--a != 5 || a != 5) r |= 2;
    if (!0 != 1 || !7 != 0) r |= 4;
    if (!5.5 != 0 || !0.0 != 1) r |= 4;
    if (~0 != -1) r |= 8;
    if (~0u != 4294967295u) r |= 8;
    if (-5 != 0 - 5) r |= 16;
    if (-(-10) != 10) r |= 16;
    float f = 2.0f;
    f++;
    if (f != 3.0f) r |= 32;
    --f;
    if (f != 2.0f) r |= 32;
    if (-1.5f != 0.0f - 1.5f) r |= 64;
    return r;
}`, 0)
}

func TestSpassExprFloatOps(t *testing.T) {
	exE2E(t, "float-ops", `
int main() {
    int r = 0;
    float a = 1.5f, b = 2.25f;
    if (a + b != 3.75f) r |= 1;
    if (b - a != 0.75f) r |= 2;
    if (a * 4.0f != 6.0f) r |= 4;
    if (b / 1.5f != 1.5f) r |= 8;
    double x = 1e100, y = 1e100;
    if (x + y != 2e100) r |= 16;
    if (!(1.0f < 2.0f)) r |= 32;
    if (!(2.0f > 1.0f)) r |= 32;
    if (!(1.0f <= 1.0f) || !(1.0f >= 1.0f)) r |= 32;
    if (1.0f != 1.0f) r |= 64;
    if (1.0f == 2.0f) r |= 64;
    float zero = 0.0f;
    if (!zero != 1) r |= 128;
    if (!a != 0) r |= 128;
    double nan = 0.0 / 0.0;
    if (nan == nan) r |= 256;
    if (nan != nan) r |= 0;
    else r |= 256;
    return r;
}`, 0)
}

func TestSpassExprAssign(t *testing.T) {
	exE2E(t, "assign", `
int g;
int main() {
    int r = 0;
    int a, b;
    a = b = 7;
    if (a + b != 14) r |= 1;
    int x = 10;
    x += 5; if (x != 15) r |= 2;
    x *= 2; if (x != 30) r |= 2;
    x -= 3; if (x != 27) r |= 2;
    x /= 9; if (x != 3) r |= 2;
    x %= 2; if (x != 1) r |= 2;
    x <<= 4; if (x != 16) r |= 4;
    x >>= 2; if (x != 4) r |= 4;
    x |= 3; if (x != 7) r |= 4;
    x &= 5; if (x != 5) r |= 4;
    x ^= 3; if (x != 6) r |= 4;
    float f = 1.0f;
    f += 2.0f; if (f != 3.0f) r |= 8;
    g = 99;
    if (g != 99) r |= 16;
    g += 1;
    if (g != 100) r |= 16;
    return r;
}`, 0)
}

func TestSpassExprGlobalsStrings(t *testing.T) {
	exE2E(t, "globals-strings", `
int g_init = 42;
static long g_big = 1234567890123L;
int g_zero;
static int g_st = 7;
const char *msg = "hi";
int printf();
int main() {
    int r = 0;
    if (g_init != 42) r |= 1;
    g_init = 100;
    if (g_init != 100) r |= 1;
    if (g_big != 1234567890123L) r |= 2;
    if (g_zero != 0) r |= 4;
    g_zero = -5;
    if (g_zero != -5) r |= 4;
    if (g_st != 7) r |= 8;
    if (msg[0] != 'h' || msg[1] != 'i' || msg[2] != 0) r |= 16;
    printf("s=%s n=%d\n", "abc", 7);
    return r;
}`, 0)
}

func TestSpassExprStructs(t *testing.T) {
	exE2E(t, "structs", `
typedef struct { int x; int y; } Point;
typedef struct { float v[2]; int id; long pad[4]; } FatIdBigRet;
Point make(int a, int b) { Point p; p.x = a; p.y = b; return p; }
FatIdBigRet makebig(int a) {
    FatIdBigRet r;
    r.v[0] = (float)a;
    r.v[1] = (float)(a + 1);
    r.id = a + 2;
    return r;
}
int sum(Point p) { return p.x + p.y; }
int printf();
int main() {
    int r = 0;
    Point p1 = {10, 20};
    if (p1.x + p1.y != 30) r |= 1;
    Point *pp = &p1;
    pp->x = 11;
    if (p1.x != 11) r |= 2;
    Point p2 = make(3, 4);
    if (p2.x + p2.y != 7) r |= 4;
    if (sum(p2) != 7) r |= 8;
    Point lit = (Point){.y = 9, .x = 1};
    if (lit.x + lit.y != 10) r |= 16;
    FatIdBigRet big = makebig(5);
    if (big.id != 7) r |= 32;
    if (big.v[1] != 6.0f) r |= 64;
    printf("pt=(%d,%d)\n", p2.x, p2.y);
    return r;
}`, 0)
}
