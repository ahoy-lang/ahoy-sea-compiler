# Parser Bug Notes - Line 1267

## Issue
Complex nested cast + statement expression causes parser to fail.

## Pattern
```c
((AhoyArray*)({ int __idx = hover_r; AhoyArray* __arr = grid; ... ((AhoyArray*)(intptr_t)__arr->data[__idx]); }))->data[hover_c]
```

## Root Cause
Cast detection with backtracking has edge case when:
1. Outer paren expression contains nested casts
2. Inner statement expression contains casts
3. Parser position gets corrupted during backtrack/restore

## Token Positions
- lparen 12215: Outer `(` of double cast inside statement expr
- After parseExpression: stuck at 12223 (`__arr`) instead of advancing

## Attempted Fixes
1. ✅ Added typedef resolution for Texture2D, AhoyArray
2. ✅ Added backtracking when cast detection fails  
3. ❌ Backtracking has position corruption bug

## Next Steps
- Need deeper parser refactoring to handle complex nesting
- Consider: Remove backtracking, make cast detection more explicit
- OR: Simplify gridstone code generation to avoid this pattern

## Workaround
Compile with GCC backend for now:
```bash
gcc /home/lee/Documents/gridstone/output/main.c -o gridstone -lraylib ...
```
