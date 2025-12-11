# Code Duplication Bug - FIXED! ✅

## Problem
Assembly functions were being generated twice, causing 2x code size and incorrect execution.

## Root Cause
In `code_emitter.go`, the `emitTextSection()` function was incrementing the index `i` after calling `emitFunction()`, but `emitFunction()` was ALSO incrementing the same index. This caused the loop to skip over the return statement and process the same function again.

## The Fix
Added `continue` statement after calling `emitFunction()` to skip the normal increment:

```go
if ce.isFunctionLabel(instr.Dst.Value) {
    ce.emitFunction(instr.Dst.Value, &i)
    continue  // Don't increment - emitFunction already did
}
```

Additionally, `AssembleAndLinkNative()` was calling `Emit()` a second time (after it was already called in `Compile()`), causing the code emitter's internal buffers to accumulate. Fixed by reusing the already-generated assembly text:

```go
// Use already-generated assembly text
asmText := cp.assembly
assembler := NewAssembler()
machineCode, err := assembler.AssembleText(asmText)
```

## Result
- ✅ Code duplication eliminated
- ✅ Binary size reduced from 615 bytes to 599 bytes
- ✅ GCC backend works perfectly (returns exit code 42)
- ⚠️ Native backend still has an ELF execution issue (separate bug)

## Test Results
```bash
$ ./ccompiler /tmp/minimal.c && ./a.out; echo "Exit: $?"
Exit: 42  # ✅ Correct!
```

---
*Fixed: December 11, 2024*
