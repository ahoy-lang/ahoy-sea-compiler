# Shader Loading Fix - Large Struct Return Argument Corruption

## Problem

The gridstone game was failing to load shaders with errors:
```
WARNING: FILEIO: [] Failed to open text file
WARNING: FILEIO: [shaders/crt.fs] Failed to open text file  
WARNING: SHADER: Shader files provided are not valid, using default shader
```

Additionally, LoadRenderTexture was creating textures with corrupted dimensions:
```
INFO: TEXTURE: [ID 3] Texture loaded successfully (908733040x800 | R8G8B8A8 | 1 mipmaps)
```
Instead of the correct 1200x800.

## Root Cause

When a function returns a large struct (>16 bytes), the x86-64 calling convention requires:
1. Caller allocates stack space for return value
2. Caller passes pointer to this space as FIRST argument in RDI  
3. Actual function arguments shift right to RSI, RDX, RCX, etc.

Our instruction selector was emitting IR in the wrong order:
```
1. OpLoadAddr %rdi, retSlot     # Load return pointer to RDI
2. OpMov %rsi, arg1              # Move first arg to RSI  
3. OpMov %rdx, arg2              # Move second arg to RDX
4. OpCall
```

But the register allocator would generate:
```assembly
mov %r13,%rdi           # Move arg1 from temp to RDI (WRONG!)
lea -0x60(%rbp),%rdi    # OpLoadAddr overwrites it
mov %rdi,%rsi           # Then tries to copy arg1 to RSI (but RDI is now the pointer!)
mov %r9,%rdx            # Move arg2 to RDX
call LoadRenderTexture
```

Result: Arguments were corrupted because RDI was used as both a temp register for arg1 AND the hidden return pointer.

## The Fix

**File**: instruction_selection.go, lines 1372-1417

Changed the order of IR emission to move arguments to their registers BEFORE emitting the hidden pointer load:

```go
// Move arguments to calling convention registers
// Do this BEFORE OpLoadAddr to avoid register conflicts
for i, arg := range args {
    regIdx := i + argStartIdx
    if regIdx < len(argRegs) {
        regOp := &Operand{Type: "reg", Value: argRegs[regIdx]}
        is.emit(OpMov, regOp, arg, nil)
    }
}

// NOW emit the hidden pointer load (after args are in place)
if retSlot != nil {
    is.emit(OpLoadAddr, &Operand{Type: "reg", Value: "rdi"}, retSlot, nil)
}
```

This generates correct assembly:
```assembly
mov %r13,%rsi           # Move arg1 to RSI (correct position)
mov %r9,%rdx            # Move arg2 to RDX (correct position)  
lea -0x60(%rbp),%rdi    # THEN load hidden pointer to RDI
call LoadRenderTexture
```

## Results

### Before Fix
```
WARNING: FILEIO: [shaders/crt.fs] Failed to open text file
WARNING: SHADER: Shader files provided are not valid, using default shader
INFO: TEXTURE: [ID 3] Texture loaded successfully (908733040x800 | ...)
WARNING: FBO: [ID 1] Framebuffer has incomplete attachment
```

### After Fix
```
INFO: FILEIO: [shaders/crt.fs] Text file loaded successfully ✅
INFO: SHADER: [ID 5] Program shader loaded successfully ✅
INFO: FILEIO: [shaders/crt_ui.fs] Text file loaded successfully ✅
INFO: SHADER: [ID 6] Program shader loaded successfully ✅
INFO: FILEIO: [shaders/wobble.vs] Text file loaded successfully ✅
INFO: FILEIO: [shaders/wobble.fs] Text file loaded successfully ✅
INFO: SHADER: [ID 8] Program shader loaded successfully ✅
INFO: TEXTURE: [ID 3] Texture loaded successfully (1200x800 | ...) ✅
INFO: FBO: [ID 1] Framebuffer object created successfully ✅
```

All shaders now load correctly! ✅

## Impact

- **LoadShader**: Now works correctly, all 3 shader programs load
- **LoadRenderTexture**: First call works correctly (1200x800)
- **Game initialization**: Progresses much further, can create framebuffers

## Remaining Issues

The second LoadRenderTexture call still has some parameter corruption (height=0 instead of 800). This suggests there may be additional edge cases with consecutive large struct return calls that need investigation. However, the core issue is resolved and the game progresses significantly further.

## Testing

Tested with gridstone/output/main.c (2,024 lines):
- All 3 shader files load successfully
- RenderTexture dimensions mostly correct  
- Game initializes graphics subsystem
- No more shader-related warnings (except the intentional empty string for vertex shaders)
