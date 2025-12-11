# Fast C Compiler - Quick Start

## What is This?

A **fast C compiler** targeting x86-64, written in Go. Goal: be **faster than Tiny C Compiler (TCC)**.

## Current Status

✅ **Working:** Compiles simple C programs to executables  
🚧 **In Progress:** Building integrated assembler and linker  
🎯 **Goal:** Compile complex programs like gridstone (123K lines)  

## Quick Test

```bash
# Build compiler
go build -o ccompiler

# Compile and run a C program
./ccompiler testfiles/simple_test.c -run

# View assembly output
./ccompiler testfiles/simple_test.c -S

# Verbose mode (see timing)
./ccompiler testfiles/simple_test.c -v
```

## Current Performance

```
Parsing:              ~70 µs
Instruction Select:   ~15 µs
Register Allocation:  ~35 µs
Code Emission:        ~15 µs
GCC Linking:         ~15 ms   ← bottleneck
─────────────────────────────
Total:               ~15 ms
```

**Target Performance (after Phase 2):** < 0.5 ms total (30x faster!)

## What Works Now

✅ Functions with parameters  
✅ Local and global variables  
✅ Arithmetic: `+`, `-`, `*`, `/`, `%`  
✅ Comparisons: `<`, `>`, `<=`, `>=`, `==`, `!=`  
✅ Control flow: `if/else`, `while`, `for`  
✅ Function calls and recursion  
✅ Bitwise operations  
✅ Compound assignments: `+=`, `-=`, etc.  

## What's Missing

❌ Preprocessor (`#include`, `#define`)  
❌ Structs  
❌ Arrays  
❌ Pointers  
❌ Standard library headers  

## Roadmap

### Phase 1: ✅ Core Compiler (DONE)
- Lexer, Parser, IR, Register Allocation, Code Gen

### Phase 2: 🚧 Built-in Assembler (In Progress)
- Machine code generation (no GCC)
- ELF file generation
- Integrated linker
- **Target:** < 1ms compilation

### Phase 3: 🔜 Preprocessor
- `#include` support
- `#define` macros
- Conditional compilation

### Phase 4: 🔜 Extended C Features
- Structs
- Arrays
- Pointers
- sizeof, casts

### Phase 5: 🎯 Gridstone Goal
- Compile 123K line program
- External library linking (raylib)
- Be faster than TCC

## Architecture

**Current (5 phases with GCC):**
```
C Source → Parser → IR → Registers → Assembly → [GCC] → Binary
```

**Target (3 phases, no GCC):**
```
C Source → Parser → Machine Code → ELF → Binary
```

## File Structure

| File | Lines | Purpose |
|------|-------|---------|
| `lexer.go` | 435 | Tokenization |
| `parser.go` | 1,018 | AST generation |
| `instruction_selection.go` | 600+ | IR generation |
| `register_allocator.go` | 450+ | Register allocation |
| `code_emitter.go` | 600+ | x86-64 assembly |
| `compiler_pipeline.go` | 350+ | Orchestration |
| **To Add:** | | |
| `assembler.go` | ~1,500 | Machine code encoding |
| `elf_generator.go` | ~1,000 | ELF file format |
| `linker.go` | ~1,000 | Symbol resolution |

## Example

**Input (test.c):**
```c
int add(int a, int b) {
    return a + b;
}

int main() {
    return add(5, 10);  // Returns 15
}
```

**Compile:**
```bash
./ccompiler test.c -run
```

**Output:**
```
✓ Compilation successful!
  Time: 15.234ms
  Output: a.out

[Compiled and ran in 15.234ms]
```

## Comparison to Other Compilers

| Feature | Our Compiler | TCC | GCC |
|---------|--------------|-----|-----|
| **Speed (target)** | <0.5ms | ~5ms | ~100ms |
| **Integrated Tools** | Yes (planned) | Yes | No |
| **Optimizations** | None | Minimal | Heavy |
| **Use Case** | Fast dev/test | Fast dev/test | Production |
| **Size** | ~9K lines Go | ~70K lines C | ~15M lines |

## Next Steps

1. **Week 1-2:** Implement x86-64 instruction encoder
2. **Week 3:** Create ELF file generator
3. **Week 4:** Build basic linker
4. **Week 5-6:** Add preprocessor support
5. **Week 7+:** Extended C features (structs, arrays)

## Resources

- 📖 **Detailed docs:** See `COMPILER.md`
- 🗺️ **Development plan:** See `ROADMAP.md`
- 📚 **Intel Manual:** [x86-64 Architecture](https://software.intel.com/content/www/us/en/develop/articles/intel-sdm.html)
- 🔗 **TCC Reference:** [Tiny C Compiler](https://bellard.org/tcc/)

## License

MIT - Use freely!

---

*Fast compilation is the goal. Runtime optimization is not our job.*
