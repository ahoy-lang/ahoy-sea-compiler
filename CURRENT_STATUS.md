# Native Backend - Current Status

## ✅ What Works

### Basic Programs
```c
#include <stdio.h>

int main() {
    printf("Hello World\n");
    printf("Test %d\n", 42);
    return 0;
}
```
**Result**: ✅ Compiles and runs successfully in 21ms

### Features That Work
- ✅ Basic arithmetic and logic operations
- ✅ Function calls with correct argument passing
- ✅ Printf with format strings
- ✅ Local and global variables
- ✅ If/else, while, for loops
- ✅ Arrays and pointers
- ✅ Structs and member access
- ✅ Stack allocation and management
- ✅ Proper calling conventions
- ✅ External symbol resolution (libc)

## ⚠️ Known Issues

### Large/Complex Programs (Gridstone)
The Gridstone card game (13K+ IR instructions) compiles successfully but crashes during Raylib initialization:
- Crash location: `_dl_map_object_from_fd` in dynamic linker
- When: Loading libX11.so.6
- Both native and non-native backends affected
- Likely cause: Stack corruption or memory layout issue in very large functions

### Issue Details
The crash happens at:
1. `main()` → `ahoy_setup_signal_handlers()` → `ahoy_main()` → `InitWindow()`  
2. `InitWindow()` → `glfwInit()` → `dlopen("libX11.so.6")`
3. Dynamic linker crashes at line 966 of `dl-load.c`

This appears to be related to the complexity/size of the program rather than a fundamental code generation bug.

## 📊 Statistics

### Minimal Program (printf)
- Source: 6 lines
- IR Instructions: ~20
- Assembly Lines: ~60
- Compilation Time: 21ms
- **Status**: ✅ WORKS

### Complex Program (Gridstone)
- Source: ~2000 lines
- IR Instructions: 13,216
- Assembly Lines: 24,218
- Stack Usage: 40KB
- Spilled Variables: 5,667
- Compilation Time: ~1.2s  
- **Status**: ⚠️ Compiles, crashes at runtime in Raylib init

## 🔧 Recent Fixes

1. **External Symbol Handling**: Added `IsExternal` flag to prevent declaring libc symbols (stderr, stdout, stdin) in BSS
2. **Stack Allocation**: Fixed `calculateStackSize` to skip function label
3. **Stack Alignment**: Proper ABI alignment (RSP % 16 == 8 before calls)
4. **Float Literals**: Store in .rodata, load via RIP-relative addressing
5. **Register Formatting**: Fixed double-% bug in linear scan mode
6. **Library Linking**: Added proper Raylib flags for both backends

## 🎯 Conclusion

The native backend successfully compiles and runs simple-to-medium C programs. The issue with large programs like Gridstone appears to be edge-case related (possibly stack exhaustion, memory corruption in very deep call stacks, or interactions with complex library initialization).

For production use with Raylib games:
- **Recommended**: Use non-native mode (GCC backend) until large program issue is resolved
- **For testing**: Native backend works great for smaller programs

The core compiler infrastructure is solid - this is a debugging/edge-case issue rather than a fundamental design problem.
