# Compiler Status - Gridstone Parity Effort

## Completed Fixes

### 1. Struct Member Assignment Segfault ✅
- **Issue**: Pointer register was overwritten with value during `ptr->member = value`
- **Fix**: Use separate registers (%r11 for pointer, %rax/%r10 for value)
- **File**: code_emitter.go, lines 778-809
- **Impact**: Gridstone no longer crashes on struct assignments

### 2. Compound Assignment Operators ✅  
- **Issue**: `+=`, `-=`, `*=`, etc. not supported
- **Fix**: Expand compound assignments early in instruction selection
- **Operators**: `+=`, `-=`, `*=`, `/=`, `%=`, `&=`, `|=`, `^=`, `<<=`, `>>=`
- **File**: instruction_selection.go, lines 1262-1468
- **Impact**: Gridstone code with `arr->capacity *= 2` now compiles

### 3. Struct Initialization - Partial ✅
- **Issue**: Compound literals stored with wrong sizes, causing overlapping writes
- **Fix**: Track member sizes and use size-appropriate move instructions
  - Stores: Use `movl` (4-byte) for ints instead of `movq` (8-byte)
  - Loads: Use `movl` for member access instead of `movq`
- **Files**: 
  - instruction_selection.go: Added Size field to member store/load operands
  - code_emitter.go: Size-aware mov instructions (movl/movw/movb)
- **Status**: Data is stored/loaded correctly, but register allocation bug affects printf

## Current Status

**Gridstone**: Compiles ✅, Runs ⚠️ (hangs after shader loading)

### What Works:
- Full compilation without errors
- Raylib initialization  
- OpenGL setup
- Shader loading (CRT, wobble shaders)
- Framebuffer creation

### Remaining Issues:

#### 1. Register Allocation for Function Calls
- **Symptom**: When passing struct members to printf, wrong values printed
- **Root Cause**: Format string address clobbers argument registers
- **Example**: `printf("y=%d", g.b)` - %rdi gets y, then format string overwrites it
- **Impact**: Affects any multi-argument function calls

#### 2. Gridstone Hang
- **Location**: After "SHADER: [ID 8] Program shader loaded successfully"
- **Likely Cause**: HashMap initialization or similar complex struct operations
- **Next Step**: Need to debug the specific line causing the hang

## Test Results

### Simple Struct Tests:
- Single member: ✅ Works
- Direct assignment: ✅ `g.a = 10; g.b = 20;` works
- Compound literal direct access: ✅ `printf("%d", g.a)` works  
- Compound literal + variable: ❌ `int y = g.b; printf("%d", y)` - wrong value due to register allocation

### Binary Comparison:
- **TCC**: 1.2M, runs completely, loads all assets
- **Ours**: 1.1M (stripped), hangs during initialization

## Code Quality:
- Size-aware loads/stores implemented
- Proper 4-byte int handling
- Compound assignment expansion working
- Struct member offset calculation correct

## Next Steps to Achieve Parity:

1. Fix register allocation for function arguments (preserve across loads)
2. Debug gridstone hang (likely infinite loop or null pointer)
3. Verify HashMap operations compile correctly
4. Test full asset loading sequence
5. Compare runtime behavior in detail

