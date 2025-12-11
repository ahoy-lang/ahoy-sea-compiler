# Quick Reference - C Compiler

## Quick Start

```bash
# Build the compiler
go build -o ccompiler

# Compile a C program
./ccompiler program.c

# Compile and run
./ccompiler program.c -run

# See compilation details
./ccompiler program.c -v
```

## Command Line Options

| Option | Description |
|--------|-------------|
| `-run` | Compile and execute immediately |
| `-v` | Verbose output with timing for each phase |
| `-S` | Output assembly only (no linking) |
| `-o <file>` | Specify output filename |
| `-O0` to `-O3` | Optimization level (0=none, 3=max) |
| `-linear-scan` | Use linear scan register allocator |

## Compilation Phases

1. **Parsing** - C source → AST (~50µs)
2. **Instruction Selection** - AST → IR (~15µs)
3. **Register Allocation** - IR optimization (~35µs)
4. **Code Emission** - IR → x86-64 assembly (~25µs)
5. **Assembly & Linking** - Assembly → executable (~17ms)

## Supported C Features

### ✅ Fully Supported

- Integer types (int)
- Arithmetic: `+`, `-`, `*`, `/`, `%`
- Comparisons: `==`, `!=`, `<`, `<=`, `>`, `>=`
- Logical: `&&`, `||`, `!` (with short-circuit)
- Bitwise: `&`, `|`, `^`, `~`, `<<`, `>>`
- Assignment: `=`, `+=`, `-=`, `*=`, `/=`
- Increment/Decrement: `++`, `--` (pre and post)
- Function definitions and calls
- Recursion
- Local and global variables
- Control flow: `if`, `else`, `while`, `for`
- Return statements
- Ternary operator: `? :`
- Compound expressions

### ⚠️ Partial Support

- Preprocessor directives (skipped)
- Multiple variable declarations
- Type keywords (parsed but not fully checked)

### ❌ Not Yet Supported

- Pointers and pointer arithmetic
- Arrays
- Structs/Unions
- Floating point
- Strings (literals recognized, not full support)
- Switch statements
- Standard library (link with libc manually)

## Example Programs

### Hello World (via return code)
```c
int main() {
    return 42;
}
```

### Arithmetic
```c
int calculate(int a, int b) {
    return (a + b) * 2;
}

int main() {
    return calculate(10, 5);  // Returns 30
}
```

### Factorial (Recursion)
```c
int factorial(int n) {
    if (n <= 1) return 1;
    return n * factorial(n - 1);
}

int main() {
    return factorial(5);  // Returns 120
}
```

### Fibonacci
```c
int fib(int n) {
    if (n <= 1) return n;
    return fib(n - 1) + fib(n - 2);
}

int main() {
    return fib(7);  // Returns 13
}
```

### Loops
```c
int sum(int n) {
    int total = 0;
    int i = 1;
    while (i <= n) {
        total = total + i;
        i = i + 1;
    }
    return total;
}

int main() {
    return sum(10);  // Returns 55
}
```

### For Loop
```c
int main() {
    int sum = 0;
    for (int i = 0; i < 10; i = i + 1) {
        sum = sum + i;
    }
    return sum;  // Returns 45
}
```

## Debugging

### View Generated Assembly
```bash
./ccompiler program.c -S -o program.s
cat program.s
```

### Failed Compilation
If compilation fails, assembly is saved to `/tmp/failed_output.s`

### Verbose Mode
```bash
./ccompiler program.c -v
```

Shows:
- Parse time
- IR instruction count
- Registers used
- Variables spilled
- Assembly line count
- Each phase timing

## Architecture

### Registers Used
- RAX - Return value, scratch
- RBX, RCX, RDX - General purpose
- RSI, RDI - Argument passing (1st, 2nd args)
- R8-R15 - Extended registers
- RBP - Frame pointer
- RSP - Stack pointer (reserved)

### Calling Convention
System V AMD64 ABI:
- Arguments: RDI, RSI, RDX, RCX, R8, R9
- Return: RAX
- Callee-saved: RBX, R12-R15
- Caller-saved: R10-R11

### Stack Layout
```
Higher addresses
+------------------+
| Arguments 7+     |
+------------------+
| Return address   |
+------------------+
| Old RBP          | <- RBP points here
+------------------+
| Local variables  |
+------------------+
| Spilled temps    |
+------------------+
| ...              | <- RSP points here
Lower addresses
```

## Performance Tips

1. **Use linear scan for faster compilation:**
   ```bash
   ./ccompiler program.c -linear-scan
   ```

2. **For large files:** Consider splitting into multiple compilation units

3. **Optimization levels:** Currently parsed but not implemented

## Common Issues

### "bad register name"
- Internal compiler bug, likely in register allocation
- Check `/tmp/failed_output.s` for details

### Segmentation fault when running
- Stack alignment issue
- Missing function prologue/epilogue
- Check with: `objdump -d a.out`

### Wrong return value
- Integer overflow (exit codes are modulo 256)
- Example: return 256 → exit code 0

## Files

| File | Purpose |
|------|---------|
| `lexer.go` | Tokenization |
| `parser.go` | AST generation |
| `instruction_selection.go` | IR generation |
| `register_allocator.go` | Register allocation |
| `code_emitter.go` | Assembly generation |
| `compiler_pipeline.go` | Pipeline orchestration |
| `main.go` | Entry point |

## Build Information

```bash
# Build
go build -o ccompiler

# Build with optimizations
go build -ldflags="-s -w" -o ccompiler

# Run tests
./ccompiler testfiles/simple_test.c -run
./ccompiler testfiles/math_test.c -run
```

## Next Steps for Development

1. Add array support
2. Implement pointers
3. Add struct/union support
4. Implement preprocessor
5. Add type checking
6. Optimization passes
7. Better error messages
8. Debug info generation
