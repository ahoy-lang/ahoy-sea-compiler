# Compiler Architecture

## Correct Preprocessor Design

### What a Preprocessor Should Do
The preprocessor performs **simple, lexical transformations** on source code:

1. **File inclusion** (#include): Copy/paste the entire content of header files
2. **Macro expansion** (#define): Replace defined macros with their values
3. **Conditional compilation** (#ifdef, #ifndef, #else, #endif)
4. **Comment removal**

### What a Preprocessor Should NOT Do
The preprocessor is **unaware of C language syntax**:
- Does NOT understand variables, functions, scopes, or data structures
- Does NOT parse struct definitions or calculate sizes
- Does NOT extract function signatures
- Does NOT understand types
- Operates purely on text, without knowledge of language structure

## Our Implementation

### Phase 0: Preprocessing
- **Input**: Source code with #include directives
- **Process**: Simple text substitution preprocessor
  - Recursively copy/pastes included header files
  - Replaces #define macros
  - Handles #ifdef conditionals
- **Output**: Expanded source code (just C text)

### Phase 1: Parsing  
- **Input**: Preprocessed source code
- **Process**: Lexer tokenizes, Parser builds AST
  - Parses struct definitions and calculates member sizes/offsets
  - Parses function declarations (including return types and parameters)
  - Parses typedefs
  - Builds complete AST
- **Output**: Abstract Syntax Tree with all type information

### Phase 2: Instruction Selection
- **Input**: AST from parser
- **Process**: Convert AST to intermediate representation
  - Uses struct definitions from parser
  - Uses function signatures from parser
  - Implements x86-64 calling conventions:
    - **Large struct returns** (>16 bytes): Pass hidden pointer in %rdi
    - **Regular returns** (≤16 bytes): Use %rax
- **Output**: IR instructions

### Phases 3-5: Register Allocation, Code Emission, Assembly/Linking
- Standard compiler backend phases

## Key Fixes Implemented

### 1. Large Struct Return ABI (x86-64 System V)
When a function returns a struct larger than 16 bytes:
- Caller allocates space on stack for return value
- Caller passes pointer to this space as hidden first argument in %rdi
- Regular arguments shift right (%rsi, %rdx, %rcx, ...)
- Callee writes result to the provided address

Example:
```c
RenderTexture2D LoadRenderTexture(int width, int height); // Returns 44 bytes
```

Generated assembly:
```asm
leaq -80(%rbp), %rdi    # Hidden pointer to return slot
movq $800, %rsi         # First actual arg (width)
movq $600, %rdx         # Second actual arg (height)
call LoadRenderTexture
```

### 2. Struct Size Calculation
Parser now properly calculates struct sizes based on member types:
```c
typedef struct Texture {
    unsigned int id;    // 4 bytes, offset 0
    int width;          // 4 bytes, offset 4
    int height;         // 4 bytes, offset 8
    int mipmaps;        // 4 bytes, offset 12
    int format;         // 4 bytes, offset 16
} Texture;              // Total: 20 bytes

typedef struct RenderTexture {
    unsigned int id;    // 4 bytes, offset 0
    Texture texture;    // 20 bytes, offset 4
    Texture depth;      // 20 bytes, offset 24
} RenderTexture;        // Total: 44 bytes
```

### 3. Function Signature Tracking
During parsing, we extract function declarations to track:
- Return type (important for calling convention)
- Parameter types

This allows the instruction selector to know when to use the large struct return convention.

## Results

✅ Correct preprocessor: Just copies headers, doesn't parse C
✅ Parser handles struct definitions from included headers
✅ Parser calculates correct struct sizes
✅ Proper x86-64 calling convention for large struct returns
✅ Simple raylib programs compile and link correctly
✅ Game initializes and runs (though may have runtime issues)

## Current Limitations

⚠️ Parser doesn't support all advanced C syntax yet
⚠️ Some complex raylib constructs may fail to parse
⚠️ Full gridstone compilation requires more robust parser

The architecture is now correct - the preprocessor just does text substitution, and all C language understanding happens in the parser.
