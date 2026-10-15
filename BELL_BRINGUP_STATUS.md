# Sea compiler — bell bring-up status (2026-10-16)

Goal: make `ahoy-sea-compiler` (a tcc-like fast C compiler, NOT an
optimizer) compile real Ahoy-generated C: gridstone (done) then bell
(in progress). Stay inside `ahoy-sea-compiler/`; do not touch `ahoy/`,
`bell/`, gridstone (other agent active there).

Committed as `711962d` (10 files, +4483/-538). Everything below is on
top of `50fa2e2` (25/25 tests, gridstone).

## Where things stand

- `test_cases/*.c`: 25/25 exit codes match gcc (also ~40 micro-tests in
  /tmp, all matching gcc at time of writing).
- Bell `/home/lee/Documents/ahoy-lang/bell/output/main.c` (34880 lines):
  compiles + links in ~46s with `-linear-scan` to an 11MB binary.
- The sea-built bell runs headless (`xvfb-run`) through renderer init,
  settings load, font init, and into glyph rasterization, where it dies
  in stb_truetype (currently heap corruption in the
  FlattenCurves/Rasterize path; see §3).
- gcc-built bell from the same TU runs clean end-to-end
  (`lit2d demo done`), so all remaining failures are sea miscompiles,
  not environmental. Gcc is the behavioral oracle for everything.

## What was fixed this session (all validated vs gcc)

Preprocessor (`preprocessor.go`):
- Include-cycle OOM (same file via different spellings) via abspath
  canonicalization.
- Real `#if` expression evaluator (`&& || ! == != < <= > >=`,
  parens, hex/octal/U-L suffixes, macro refs, undefined→0). The old
  code only looked at a leading `defined()` — SDL took C23 branches.
- Predefined macros: `__STDC__`, `__STDC_VERSION__=201710L`,
  `__STDC_HOSTED__`, `__LP64__`, `_LP64` (deliberately NOT
  `__x86_64__`/`__GNUC__`). Plus libc stub macros from skipped headers:
  `NULL/true/false/EOF/SEEK_*/INT*_MIN-MAX/UINT*_MAX/SIZE_MAX/...`,
  `st_atime/st_mtime/st_ctime`, `S_ISDIR/S_ISREG/...`, function macros
  `assert`, `SDL_STATIC_CAST`-style helpers resolve naturally.
- `#define` correctness: `NAME (body)` (space) and `NAME OTHER(...)`
  are OBJECT macros (was: misread as function macros — broke
  `SDL_WINDOW_HIDDEN` etc.); `# direct` spacing; `#` stringification;
  `##` pasting; fixpoint expansion (nested macro calls);
  string/char-literal-aware expansion (no substitution inside `"..."`);
  multiline macro invocations via paren-join (line numbers preserved);
  phase-3 comment stripping before expansion (apostrophes in comments
  no longer corrupt masking).
- Opaque/system types: `FILE/DIR/pthread_*`, SDL `Uint*/Sint*`,
  `struct stat` + `struct timespec` (exact glibc layout),
  `struct dirent` + `DT_*`, `_Thread_local`/`__thread` (parsed, ignored).

Parser (`parser.go`, `lexer.go`):
- `do-while` (with working `break`/`continue` via label stacks —
  previously break/continue in loops were silently ignored!);
  `_Static_assert`/`static_assert` (folded, error on false);
  plain function typedefs `typedef void f(...)` (as void*);
  global multi-declarators (`int a, b;` at file scope);
  unsized array inference (`int a[] = {...}`);
  float `f/F` suffixes; `struct S func()` no longer swallowed as a
  definition (lookahead for `{`); unified struct-body parser with
  anonymous struct/union member inlining, real union (all-zero) offsets,
  and `__anon` naming shared with parseType.

Selection/codegen (ABI + model):
- SysV struct passing: ≤8 by value, 9–16 chunked (int/sse classified),
  >16 by hidden pointer (+copy into real local); stack overflow for all;
  temp-free prologue (allocated temps could alias live param regs);
  float params in xmm (bits for float, double for double) with
  per-signature routing + libc math table; external float/double
  returns read from xmm0 (defined functions keep rax convention);
  int→float/double conversion at call sites (in slot loop, not SetArg,
  so cvtsi2sd scratch doesn't clobber earlier xmm args).
- Struct values: returns (large via hidden ptr with width-correct copy;
  small via rax/rdx), init/assignment copies (var/global/member/deref),
  nested compound literals, addresses via recursive lvalueAddrSize +
  unified `structTypeOf` resolver; short-circuit `&&`/`||`;
  float compares via `ucomisd` (int/float mixed, NaN-correct);
  comparison results always int; oversized immediates via movabs;
  exponent floats (`1e30`) recognized; octal literals; scalar globals in
  `.data`; global array initializers in `.data`.
- Correctness: sign/zero extension everywhere it matters (int→long
  `i2l/u2l`, char/short→int `s82i/u82i`, narrowing casts retype,
  short/char returns narrow, deref loads/stores width+signedness,
  32-bit `cmpl` for 32-bit compares, pointer scaling by element size
  incl. void*=1, float negation flips sign bit not integer neg).
- Infra: callee-saved regs actually saved (usedRegisters now populated
  for both allocators); prologue pushes before sub with alignment math;
  epilogue resets rsp to push area before pops; 40-byte callee-save
  reserve below locals; negative-offset alignment rounds down (was
  rounding up and overlapping slots); `OpLoadAddr` to spilled dst via
  rax bounce; bare `.a/.o/.so` + `-L` forwarded to link.

## Known remaining problems

1. **stb_truetype glyph rasterization (current blocker).** Bell dies
   here; minimal repro is `rasttest.c`-shape (triangle → Rasterize) and
   `fontbake`/`bakeone`. Symptoms moved as bugs were fixed (assert →
   segfault → heap abort); latest is heap corruption detected at malloc
   inside/after rasterize (`FlattenCurves`/`rasterize_sorted_edges`/
   `handle_clipped_edge` area). Small cases (1–2 verts, single glyph
   shape) PASS; triangle rasterization FAILS. Next step: instrument
   edge/vertex counts (`n`, `m`, `num_vertices`) in FlattenCurves for
   the triangle vs gcc (values, not code shape, are suspect — e.g. a
   count off by one overflowing the windings buffer).
2. **Default (graph-coloring) allocator hangs on bell** (646k IR;
   preprocessing 44s + selection <1s, then hang in allocation). Only
   `-linear-scan` (46s total) is usable for bell. Investigate after
   correctness (likely superlinear liveness/interference).
3. **Preprocessing is slow on bell** (~44s of the 46s). Suspects:
   fixpoint expansion (≤50 iters × hundreds of macros × maskStrings on
   100KB joined lines). Profile before optimizing; keep output identical.
4. **Known unhandled gaps** (loud errors, not silent): float stack
   params beyond regs in exotic signatures (basic case works);
   indirect-call float returns (assumed rax convention);
   >16-byte struct args on stack overflow (regs only);
   `long`→double precision loss >2^53 (uses q-form, correct for bell
   magnitudes); unsigned 32-bit compares use signed cc (wrong only for
   values >2GB); 8/16-bit→64-bit direct widening (goes via 32-bit);
   `_Alignas/_Generic/_Atomic` (error if met).
5. **Debug scaffolding left in tree** (all env-gated, keep or remove):
   `SEAPP` (macro expand trace), `SEAPARSE` (funcdefs),
   `SEADBG` (memberArrayInfo etc.), `-v` pipeline timings.

## How to work here (learned this session)

- Oracle everything against gcc: `test_cases/*.c` exit codes, plus
  targeted `/tmp/*.c` micro-tests printing values (`diff` sea vs gcc
  output). Suite must stay green after every change.
- Bell loop (fast iteration): build to `/tmp/ccompiler_test`
  (`go build`), compile bell with `-linear-scan` (~46s), run headless
  `xvfb-run -a /tmp/bell_sea` from `bell/`, compare vs `/tmp/bell_gcc`
  (prebuilt gcc baseline, same flags + libs:
  `libraylib.a libSDL3.a -lGL -lm -lpthread -ldl`).
- gdb on sea binaries: no debug symbols — use addresses/disasm;
  `xvfb-run -a gdb -batch -ex run -ex bt`.
- `ulimit -v 6291456` + `timeout` around every compiler run (host has
  crashed before; never run unbounded).
- Check asm (`objdump -d`) before theorizing — most root causes were
  visible in 5–10 instructions (wrong reg, missing conversion, bad
  offset). Watch for: clobbered live temps, unextended ints, truncated
  struct copies, overlapping stack slots.
- tcc philosophy holds: dumb/fast fixes (stack slots, recompute,
  short live ranges), no optimizer passes. Keep it that way.
