# Sea compiler speed work (2026-10-18)

Target: tcc compiles the bell editor TU (`bell/output/main.c`) in ~135ms.
Start: sea took ~4.2s end to end. Now: ~2.5s (6.1MB TU) / ~1.5s (3MB TU).

## Session 2 (2026-10-18, continued)

NOTE: bell/output/main.c flips between ~3.0MB and ~6.1MB as bell rebuilds;
all Session-2 numbers below are on a pinned 3.0MB snapshot
(`/tmp/opencode/bench/main-3m.c`, 1.03M IR, 31MB asm) unless noted.
Medians of 3 `TestPhaseTiming` runs (Go 1.27, Ryzen 9 5900X, 24 threads).

| Phase              | Before (s2) | After (s2) | Change |
|--------------------|-------------|------------|--------|
| Preprocess         | ~155ms      | ~110ms     | maskStrings fast path, single-pass paren depth, Builder args |
| Lex + parse        | ~150ms      | ~115ms     | match1/curType/step (no Token copies, no variadic slices) |
| Instruction select | ~190ms      | ~85ms      | removed O(F×C) funcDefined scan, Sprintf-free temps, alloc-free emit, cached debug env, batched temp ids |
| Register alloc     | ~260ms      | ~105ms     | temp-id-indexed slots (no name maps), reflection-free sort, clobber-fit cache, no per-call maps |
| Code emission      | ~225ms      | ~160ms     | Sprintf→writes (scripted), reg spelling tables, value caches, presized Builders/maps, frameMax in segments |
| Assemble (fastasm) | ~430ms      | ~285ms     | parse-time mn cache, per-instr enc, parallel finish, parallel layout sizing (memo removed) |
| **Compile-only sum** | **~1.2s** | **~0.86s** | **~1.4x on top of session 1** |

Final check (end of session, same 3MB snapshot):
`BenchmarkCompileEditor` (compile-only, no assemble): 1027ms / 9.2M allocs
→ **471ms / 5.7M allocs (2.2x, -38% allocs)**. Phase medians: PP ~105,
Parse ~100, Select ~80, RA ~90, Emit ~85, Asm ~280 ≈ 740ms + ~140ms link.
(Live bell headers drift during measurement; back-to-back A/B only.)

Correctness: `go test -count=1 ./...` green throughout;
`TestFastAsmSmall/Editor` pass (byte-identity vs GNU as holds);
new `.text` verified identical (modulo numbering) old-vs-new emitter;
`go test -race` clean on both fastasm tests.

### Incident: dropped funcDefined pre-pass (caught by differential testing)

Removing the pipeline's O(F×C) `isFuncDefinedInAST` loop as "redundant"
was WRONG for sequential select: the selector only records defined-ness
in visit order there, so forward references misclassified as extern
(wrong ABI). It showed as a .text diff in `floorf`-style extern-float
call codegen. The suite stayed green because it runs parallel (default)
mode, which has its own pre-pass. Fix: `definedFuncSet()` — the same
complete set in one O(toplevel) pass. Verified: sequential and parallel
select now produce structurally identical `.text` (modulo numbering).
Lesson: validate performance deletions differentially (old-vs-new
assembly), not just via the suite — especially when the suite exercises
only the default flags.

## Measured baseline (6.1MB TU, 88k lines C, 1.6M IR, 2.1M asm lines)

| Phase              | Before | After  | Change |
|--------------------|--------|--------|--------|
| Preprocess         | 208ms  | 195ms  | —      |
| Lex + parse        | 380ms  | 310ms  | lexer prealloc + byte ranges |
| Instruction select | 532ms  | 190ms  | per-function goroutine pool |
| Register alloc     | 468ms  | ~350ms | no per-instr slices, insertion sort, presized maps |
| Code emission      | 585ms  | ~380ms | operand cache, Sprintf-free hot paths, per-function pool |
| Assemble           | 1670ms | ~750ms | built-in assembler (was GNU `as` subprocess) |
| Link (`ld`)        | 140ms  | 140ms  | unchanged (system linker) |
| **Total wall**     | **~4.2s** | **~2.5s** | **~1.7x** |

Full-compile allocs: 21.8M objects / 1.83GB → ~14M / ~2.0GB (6MB TU).
(`go test -bench BenchmarkCompileEditor`.)

## What was built

- `fastasm.go` + `elf_object.go` (new): in-process x86-64 AT&T assembler
  writing ELF64 relocatable objects. Validated: `.text` byte-identical to
  GNU `as` on the full editor output, all 177K relocs identical, links
  with raylib/SDL3/box2d/box3d. ~2.3x faster than `as` (0.78s vs 1.78s).
- Pipeline `UseFastAsm` (default on, `-fno-fast-asm` falls back to GNU
  `as`; temp `.o` via `CreateTemp`, removed after link; `cp.assembly`
  released before link fork).
- Parallel instruction selection (`-fno-parallel-select` opt-out) and
  parallel code emission (`-fno-parallel-emit` opt-out). Both merge in
  function order; output correct (assemble+link+run) though label
  numbering may vary run to run (pre-existing map-order nondeterminism).
- `-O<level>` documented: default `0` = no optimizing passes, and none
  exist yet — all work above is lean-path speedup, not optimization.
- Side fix: parallel select exposed a latent sequential miscompile on
  forward-declared arrays (confirmed `gcc → 64, parallel → 64,
  sequential → SIGSEGV` on a reproducer).

## Validation

- `go test -count=1 ./...` passes (exit-code-vs-gcc behavioral suite).
- `TestFastAsmEditor`: `.text` byte-identical to GNU `as`, link succeeds.
- Small cases (`test_addr.c` etc.) run correctly on all four
  flag combinations (default / `-fno-fast-asm` / `-fno-parallel-select`).

## Flags

- `-fno-fast-asm` — use GNU `as` instead of the built-in assembler.
- `-fno-parallel-select` — sequential instruction selection.
- `-fno-parallel-emit` — sequential code emission.
- `-O0` (default) — no optimizing passes. `-O1..3` parsed, reserved.

## Roadmap to 150ms (tcc parity)

Phase 1+2 lands at ~2.5s. The remaining floor is structural:

1. **Single-pass codegen (the big one).** Today: AST → 1.6M IR →
   regalloc pass → 49MB text → re-parse → encode (~1.5s combined).
   TCC parses once and emits machine code into section buffers via a
   value stack (spill at statement boundaries, no regalloc). This
   deletes select+regalloc+emit+fastasm as separate passes.
2. **Preprocessor + lexer.** 300–500ms on headers (SDL/box2/raylib).
   Needs incremental header caching (content-hash the include set) and
   a zero-copy lexer (offsets, not substrings).
3. **Built-in linker.** System `ld` is only ~140ms, so this is about
   zero-dependency/`-run` JIT, not speed. Port `tccelf.c` section/
   reloc handling to Go (started: `linker.go`/`elf_generator.go` exist
   but are not wired to real inputs).
4. **Goroutines** already cover select + emit; parsing needs a
   signature pre-pass before function bodies can go parallel.

Realistic Phase-3 landing: PP ~50ms + single pass ~150ms + link ~140ms
≈ 350ms; sub-150ms needs the linker + `-run` (no link) too.

## Session 3 (2026-10-19): miscompile fixes + 3d demo verified

Speed: 3MB snapshot compile-only now **471ms / 5.7M allocs** (session 2:
1027ms / 9.2M). Live 3d-demo TU (`demos/3d/main.c`, 3.0MB) compiles
end-to-end in **~1.9s** including assemble + link.

Correctness — five miscompiles found via bell end-to-end testing, all fixed:

1. **Preprocessor signature scraping** (`preprocessor.go`): non-declaration
   lines like raymath.h's `float z1 = sinf(roll*0.5f);` were recorded as
   "`sinf` returns `float z1 =`", shadowing libc math tables → every
   `sinf`/`cosf` call used the wrong registers → all trig evaluated to 0.0
   → NaN lighting → every lit mesh black (3D viewport blank). Fix: strip
   comments, reject `=` before the paren, validate identifier tokens.
2. **Deref-through-cast struct writes** (`instruction_selection.go`:
   `derefHomeType`): `*(Node*)p = n` emitted a single 8-byte store,
   dropping the struct tail. Added the `NodeCast` case (cast target `T*`
   names the pointee).
3. **Small-struct args by reference** (`instruction_selection.go` call-arg
   loop): ≤8-byte structs (Vector2/Color) were passed by address instead of
   by value. Now the slot loop loads the value bits for the int path.
4. **Missing `RTLD_*` builtins** (`preprocessor.go`): `<dlfcn.h>` is stubbed
   but the Auto-LOD code uses `RTLD_LAZY` etc. — hard compile error on the
   current TU. Added glibc-exact values (LAZY=1, NOW=2, BINDING_MASK=3,
   NOLOAD=4, DEEPBIND=8, GLOBAL=256, LOCAL=0, NODELETE=4096).
5. **imm/imm comparison** (`code_emitter.go` `emitComparison`):
   `if (CONST != CONST)` collapsed to `cmpl $1, $1` (illegal — both sides
   immediate). Fix: stage the LHS into `%r11[d]` when both sides are `$imm`.

Verification: `go test -count=1 -vet=off ./...` green (two pre-existing
environmental failures — TestPhaseTiming needs a fresh
`/tmp/opencode/bench/main-3m.c` snapshot, TestDumpAsm needs `DUMP_TO` —
both fail identically without these changes). Editor 3D viewport
pixel-identical to gcc oracle (MSE 0.0 over the viewport region). 3d demo:
model/shaders load exactly once (no spam), 90-frame DEMO_SHOT run matches
gcc (2px/345k timing epsilon on the bouncing sphere edge).
