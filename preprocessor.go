package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Preprocessor handles C preprocessor directives
type Preprocessor struct {
	defines       map[string]string
	funcMacros    map[string]*FunctionMacro // Function-like macros
	includePaths  []string
	processed     map[string]bool  // Track processed files to avoid cycles
	dirStack      []string       // Including-file directories (innermost last)
	mu            sync.RWMutex      // For thread-safe define access
	typedefMap    map[string]*StructDef // External typedefs from headers
	structMap     map[string]*StructDef // External structs from headers
	functionSigs  map[string]*FunctionSignature // Function signatures from headers
}

type FunctionMacro struct {
	Params []string
	Body   string
}

func NewPreprocessor() *Preprocessor {
	p := &Preprocessor{
		defines:      make(map[string]string),
		funcMacros:   make(map[string]*FunctionMacro),
		// NOTE: system dirs (/usr/include) are deliberately absent: glibc
		// headers use extensions (attributes, asm labels, vector types)
		// this compiler cannot parse, and inlining them breaks the TU
		// (angle includes that miss every path are stubbed as before).
		// Project headers resolve via -I, the main file's dir, or the
		// includer's dir. The stale clibs path is kept harmlessly.
		includePaths: []string{".", "/home/lee/Documents/clibs/raylib/src"},
		processed:    make(map[string]bool),
		typedefMap:   make(map[string]*StructDef),
		structMap:    make(map[string]*StructDef),
		functionSigs: make(map[string]*FunctionSignature),
	}
	
	// Add standard built-in macros
	p.defines["NULL"] = "0"
	p.defines["true"] = "1"
	p.defines["false"] = "0"
	// Standard macros from skipped system headers (exact libc values).
	p.defines["EOF"] = "(-1)"
	p.defines["SEEK_SET"] = "0"
	p.defines["SEEK_CUR"] = "1"
	p.defines["SEEK_END"] = "2"
	// Integer limits (LP64) from skipped <stdint.h>/<limits.h>.
	p.defines["CHAR_BIT"] = "8"
	p.defines["INT_MAX"] = "2147483647"
	p.defines["INT_MIN"] = "(-2147483647 - 1)"
	p.defines["UINT_MAX"] = "4294967295U"
	p.defines["LONG_MAX"] = "9223372036854775807L"
	p.defines["LONG_MIN"] = "(-9223372036854775807L - 1)"
	p.defines["ULONG_MAX"] = "18446744073709551615UL"
	p.defines["LLONG_MAX"] = "9223372036854775807LL"
	p.defines["LLONG_MIN"] = "(-9223372036854775807LL - 1)"
	p.defines["ULLONG_MAX"] = "18446744073709551615ULL"
	p.defines["INT8_MIN"] = "(-128)"
	p.defines["INT8_MAX"] = "127"
	p.defines["UINT8_MAX"] = "255"
	p.defines["INT16_MIN"] = "(-32768)"
	p.defines["INT16_MAX"] = "32767"
	p.defines["UINT16_MAX"] = "65535"
	p.defines["INT32_MIN"] = "(-2147483647 - 1)"
	p.defines["INT32_MAX"] = "2147483647"
	p.defines["UINT32_MAX"] = "4294967295U"
	p.defines["INT64_MIN"] = "(-9223372036854775807L - 1)"
	p.defines["INT64_MAX"] = "9223372036854775807L"
	p.defines["UINT64_MAX"] = "18446744073709551615UL"
	p.defines["SIZE_MAX"] = "18446744073709551615UL"
	p.defines["PTRDIFF_MAX"] = "9223372036854775807L"
	p.defines["SHRT_MIN"] = "(-32768)"
	p.defines["SHRT_MAX"] = "32767"
	p.defines["USHRT_MAX"] = "65535"
	p.defines["CHAR_MIN"] = "(-128)"
	p.defines["CHAR_MAX"] = "127"
	p.defines["UCHAR_MAX"] = "255"
	p.defines["SCHAR_MIN"] = "(-128)"
	p.defines["SCHAR_MAX"] = "127"
	// glibc <sys/stat.h> compat (header skipped): st_mtime et al are
	// macros for the timespec members.
	p.defines["st_atime"] = "st_atim.tv_sec"
	p.defines["st_mtime"] = "st_mtim.tv_sec"
	p.defines["st_ctime"] = "st_ctim.tv_sec"
	// File-type test macros (glibc values; sys/stat.h skipped).
	p.funcMacros["S_ISDIR"] = &FunctionMacro{Params: []string{"m"}, Body: "(((m) & 0170000) == 0040000)"}
	p.funcMacros["S_ISREG"] = &FunctionMacro{Params: []string{"m"}, Body: "(((m) & 0170000) == 0100000)"}
	p.funcMacros["S_ISLNK"] = &FunctionMacro{Params: []string{"m"}, Body: "(((m) & 0170000) == 0120000)"}
	p.funcMacros["S_ISCHR"] = &FunctionMacro{Params: []string{"m"}, Body: "(((m) & 0170000) == 0020000)"}
	p.funcMacros["S_ISBLK"] = &FunctionMacro{Params: []string{"m"}, Body: "(((m) & 0170000) == 0060000)"}
	p.funcMacros["S_ISFIFO"] = &FunctionMacro{Params: []string{"m"}, Body: "(((m) & 0170000) == 0010000)"}
	p.funcMacros["S_ISSOCK"] = &FunctionMacro{Params: []string{"m"}, Body: "(((m) & 0170000) == 0140000)"}
	// assert from skipped <assert.h>: evaluate once, abort via libc.
	p.funcMacros["assert"] = &FunctionMacro{Params: []string{"x"}, Body: "((void)((x) ? 0 : (__assert_fail(#x, \"sea\", 0, \"sea\"), 0)))"}
	// pthread statics from skipped system headers (all-zeros on glibc,
	// so `{0}` is exact and folds through global .comm/.data paths).
	p.defines["PTHREAD_MUTEX_INITIALIZER"] = "{0}"
	p.defines["PTHREAD_COND_INITIALIZER"] = "{0}"
	p.defines["PTHREAD_ONCE_INIT"] = "0"
	// Standard predefined macros (C17 hosted-ish): headers key bool,
	// stdint and feature paths off these. Deliberately NOT defining
	// __GNUC__/__clang__ (their branches need GNU extensions we lack).
	p.defines["__STDC__"] = "1"
	p.defines["__STDC_VERSION__"] = "201710L"
	p.defines["__STDC_HOSTED__"] = "1"
	// LP64 x86-64 (SDL/stdint constant + printf-format branches key off
	// these; both spellings appear). Deliberately NOT __x86_64__ (would
	// enable SIMD paths) or __GNUC__ (needs GNU extensions).
	p.defines["__LP64__"] = "1"
	p.defines["_LP64"] = "1"
	// No SIMD intrinsics (we're a fast baseline compiler, not a vectorizer):
	// stb_image/stb_truetype gate their _simd paths on this and always
	// carry bit-identical scalar fallbacks.
	p.defines["STBI_NO_SIMD"] = "1"
	
	return p
}

// parseRaylibHeader parses raylib.h to extract enum constants
func (p *Preprocessor) parseRaylibHeader() {
	raylibPath := "/home/lee/Documents/clibs/raylib/src/raylib.h"
	content, err := os.ReadFile(raylibPath)
	if err != nil {
		// If we can't read raylib.h, add some fallback defines
		fmt.Fprintf(os.Stderr, "Warning: Could not read %s: %v\n", raylibPath, err)
		p.addFallbackDefines()
		return
	}
	
	lines := strings.Split(string(content), "\n")
	inEnum := false
	enumValue := 0
	
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		
		// Skip comments
		if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "/*") {
			continue
		}
		
		// Check for #define directives
		if strings.HasPrefix(line, "#define") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				name := parts[1]
				value := strings.Join(parts[2:], " ")
				// Clean up the value
				value = strings.TrimSpace(value)
				value = strings.Split(value, "//")[0] // Remove trailing comments
				value = strings.TrimSpace(value)
				
				// Only add simple numeric or identifier defines
				if len(value) > 0 && (isNumeric(value) || p.IsDefined(value)) {
					p.defines[name] = value
				}
			}
			continue
		}
		
		// Check for enum start
		if strings.Contains(line, "typedef enum") || (strings.Contains(line, "enum") && strings.Contains(line, "{")) {
			inEnum = true
			enumValue = 0
			continue
		}
		
		// Check for enum end
		if inEnum && strings.Contains(line, "}") {
			inEnum = false
			continue
		}
		
		// Parse enum values
		if inEnum {
			// Remove comments
			line = strings.Split(line, "//")[0]
			line = strings.TrimSpace(line)
			
			if line == "" || line == "{" {
				continue
			}
			
			// Handle enum entries
			if strings.Contains(line, "=") {
				// Explicit value: NAME = value,
				parts := strings.Split(line, "=")
				if len(parts) >= 2 {
					name := strings.TrimSpace(parts[0])
					valueStr := strings.TrimSuffix(strings.TrimSpace(parts[1]), ",")
					valueStr = strings.TrimSpace(valueStr)
					
					// Parse the value (might be hex like 0x00000040)
					if strings.HasPrefix(valueStr, "0x") || strings.HasPrefix(valueStr, "0X") {
						// Hex value
						var val int
						fmt.Sscanf(valueStr, "%x", &val)
						p.defines[name] = fmt.Sprintf("%d", val)
						enumValue = val + 1
					} else {
						// Try to parse as decimal
						var val int
						n, _ := fmt.Sscanf(valueStr, "%d", &val)
						if n == 1 {
							p.defines[name] = fmt.Sprintf("%d", val)
							enumValue = val + 1
						}
					}
				}
			} else {
				// Implicit value: NAME,
				name := strings.TrimSuffix(strings.TrimSpace(line), ",")
				if name != "" && name != "{" && name != "}" {
					p.defines[name] = fmt.Sprintf("%d", enumValue)
					enumValue++
				}
			}
		}
	}
}

// addFallbackDefines adds hardcoded fallback defines if raylib.h can't be read
func (p *Preprocessor) addFallbackDefines() {
	// Add raylib log levels
	p.defines["LOG_ALL"] = "0"
	p.defines["LOG_TRACE"] = "1"
	p.defines["LOG_DEBUG"] = "2"
	p.defines["LOG_INFO"] = "3"
	p.defines["LOG_WARNING"] = "4"
	p.defines["LOG_ERROR"] = "5"
	p.defines["LOG_FATAL"] = "6"
	p.defines["LOG_NONE"] = "7"
	
	// Add raylib window flags
	p.defines["FLAG_VSYNC_HINT"] = "64"
	p.defines["FLAG_FULLSCREEN_MODE"] = "2"
	p.defines["FLAG_WINDOW_RESIZABLE"] = "4"
	p.defines["FLAG_WINDOW_UNDECORATED"] = "8"
	p.defines["FLAG_WINDOW_HIDDEN"] = "128"
	p.defines["FLAG_WINDOW_MINIMIZED"] = "512"
	p.defines["FLAG_WINDOW_MAXIMIZED"] = "1024"
	p.defines["FLAG_WINDOW_UNFOCUSED"] = "2048"
	p.defines["FLAG_WINDOW_TOPMOST"] = "4096"
	p.defines["FLAG_WINDOW_ALWAYS_RUN"] = "256"
	p.defines["FLAG_WINDOW_TRANSPARENT"] = "16"
	p.defines["FLAG_WINDOW_HIGHDPI"] = "8192"
	p.defines["FLAG_MSAA_4X_HINT"] = "32"
	p.defines["FLAG_INTERLACED_HINT"] = "65536"
	
	// Add shader uniform types
	p.defines["SHADER_UNIFORM_FLOAT"] = "0"
	p.defines["SHADER_UNIFORM_VEC2"] = "1"
	p.defines["SHADER_UNIFORM_VEC3"] = "2"
	p.defines["SHADER_UNIFORM_VEC4"] = "3"
	p.defines["SHADER_UNIFORM_INT"] = "4"
	p.defines["SHADER_UNIFORM_IVEC2"] = "5"
	p.defines["SHADER_UNIFORM_IVEC3"] = "6"
	p.defines["SHADER_UNIFORM_IVEC4"] = "7"
	p.defines["SHADER_UNIFORM_SAMPLER2D"] = "8"
}

// Helper to check if a string is numeric
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return len(s) > 2
	}
	if strings.HasPrefix(s, "-") {
		s = s[1:]
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Define adds a preprocessor define
func (p *Preprocessor) Define(name, value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.defines[name] = value
}

func (p *Preprocessor) AddIncludePath(path string) {
	p.includePaths = append(p.includePaths, path)
}

func (p *Preprocessor) IsDefined(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.defines[name]
	if ok {
		return true
	}
	_, ok = p.funcMacros[name]
	return ok
}

// evaluateIfCondition evaluates a #if condition
func (p *Preprocessor) evaluateIfCondition(condition string) bool {
	condition = strings.TrimSpace(condition)

	// General expression evaluator first (handles defined(), !, &&, ||,
	// comparisons, parens). Legacy shortcuts below only fire when it
	// declines (they're subsumed otherwise).
	if v, ok := p.evalPPExpr(condition, 0); ok {
		return v != 0
	}

	// Handle: defined(X) or defined X
	if strings.HasPrefix(condition, "defined") {
		rest := strings.TrimSpace(condition[7:])
		
		// Check for defined(X)
		if strings.HasPrefix(rest, "(") {
			closeIdx := strings.Index(rest, ")")
			if closeIdx > 0 {
				name := strings.TrimSpace(rest[1:closeIdx])
				return p.IsDefined(name)
			}
		} else {
			// Check for defined X
			parts := strings.Fields(rest)
			if len(parts) > 0 {
				return p.IsDefined(parts[0])
			}
		}
		return false
	}
	
	// Handle: !defined(X) or !defined X
	if strings.HasPrefix(condition, "!") {
		rest := strings.TrimSpace(condition[1:])
		if strings.HasPrefix(rest, "defined") {
			return !p.evaluateIfCondition(rest)
		}
	}
	
// Handle numeric comparisons: 0, 1, etc.
	if condition == "0" {
		return false
	}
	if condition == "1" {
		return true
	}

	// Try to evaluate as a macro reference
	if p.IsDefined(condition) {
		value := p.defines[condition]
		if value == "0" {
			return false
		}
		return true
	}

	// Default: undefined macros are false
	return false
}

// evalPPExpr evaluates an #if expression to an integer (0/1 for truth).
// Supports defined(), !, &&, ||, ==, !=, <, <=, >, >=, parens, numbers
// (hex/octal/U-L suffixes), and macro references (undefined = 0, per C).
// depth guards recursive macro expansion.
func (p *Preprocessor) evalPPExpr(s string, depth int) (int64, bool) {
	if depth > 32 {
		return 0, false
	}
	t := &ppTokens{src: s}
	v, ok := t.parseOr(p, depth)
	if !ok {
		return 0, false
	}
	t.skipSpaces()
	if t.pos != len(t.src) {
		return 0, false
	}
	return v, true
}

type ppTokens struct {
	src string
	pos int
}

func (t *ppTokens) skipSpaces() {
	for t.pos < len(t.src) && (t.src[t.pos] == ' ' || t.src[t.pos] == '\t') {
		t.pos++
	}
}

func (t *ppTokens) peekOp(ops ...string) string {
	t.skipSpaces()
	for _, op := range ops {
		if strings.HasPrefix(t.src[t.pos:], op) {
			return op
		}
	}
	return ""
}

func (t *ppTokens) parseOr(p *Preprocessor, depth int) (int64, bool) {
	left, ok := t.parseAnd(p, depth)
	if !ok {
		return 0, false
	}
	for {
		if t.peekOp("||") == "" {
			return left, true
		}
		t.pos += 2
		right, ok := t.parseAnd(p, depth)
		if !ok {
			return 0, false
		}
		if left != 0 || right != 0 {
			left = 1
		} else {
			left = 0
		}
	}
}

func (t *ppTokens) parseAnd(p *Preprocessor, depth int) (int64, bool) {
	left, ok := t.parseCmp(p, depth)
	if !ok {
		return 0, false
	}
	for {
		if t.peekOp("&&") == "" {
			return left, true
		}
		t.pos += 2
		right, ok := t.parseCmp(p, depth)
		if !ok {
			return 0, false
		}
		if left != 0 && right != 0 {
			left = 1
		} else {
			left = 0
		}
	}
}

func (t *ppTokens) parseCmp(p *Preprocessor, depth int) (int64, bool) {
	left, ok := t.parseUnary(p, depth)
	if !ok {
		return 0, false
	}
	for {
		op := t.peekOp("==", "!=", ">=", "<=", ">", "<")
		if op == "" {
			return left, true
		}
		t.pos += len(op)
		right, ok := t.parseUnary(p, depth)
		if !ok {
			return 0, false
		}
		switch op {
		case "==":
			if left == right {
				left = 1
			} else {
				left = 0
			}
		case "!=":
			if left != right {
				left = 1
			} else {
				left = 0
			}
		case ">=":
			if left >= right {
				left = 1
			} else {
				left = 0
			}
		case "<=":
			if left <= right {
				left = 1
			} else {
				left = 0
			}
		case ">":
			if left > right {
				left = 1
			} else {
				left = 0
			}
		case "<":
			if left < right {
				left = 1
			} else {
				left = 0
			}
		}
	}
}

func (t *ppTokens) parseUnary(p *Preprocessor, depth int) (int64, bool) {
	t.skipSpaces()
	if t.pos < len(t.src) && t.src[t.pos] == '!' {
		t.pos++
		v, ok := t.parseUnary(p, depth)
		if !ok {
			return 0, false
		}
		if v == 0 {
			return 1, true
		}
		return 0, true
	}
	if strings.HasPrefix(t.src[t.pos:], "defined") {
		rest := t.src[t.pos+len("defined"):]
		if len(rest) > 0 && !isIdentifierChar(rest[0]) {
			t.pos += len("defined")
			t.skipSpaces()
			name := ""
			if t.pos < len(t.src) && t.src[t.pos] == '(' {
				t.pos++
				t.skipSpaces()
				start := t.pos
				for t.pos < len(t.src) && isIdentifierChar(t.src[t.pos]) {
					t.pos++
				}
				name = t.src[start:t.pos]
				t.skipSpaces()
				if t.pos < len(t.src) && t.src[t.pos] == ')' {
					t.pos++
				} else {
					return 0, false
				}
			} else {
				start := t.pos
				for t.pos < len(t.src) && isIdentifierChar(t.src[t.pos]) {
					t.pos++
				}
				name = t.src[start:t.pos]
			}
			if p.IsDefined(name) {
				return 1, true
			}
			return 0, true
		}
	}
	if t.pos < len(t.src) && t.src[t.pos] == '(' {
		t.pos++
		v, ok := t.parseOr(p, depth)
		if !ok {
			return 0, false
		}
		t.skipSpaces()
		if t.pos >= len(t.src) || t.src[t.pos] != ')' {
			return 0, false
		}
		t.pos++
		return v, true
	}
	start := t.pos
	for t.pos < len(t.src) && (isIdentifierChar(t.src[t.pos])) {
		t.pos++
	}
	if t.pos == start {
		return 0, false
	}
	tok := t.src[start:t.pos]
	stripped := tok
	for len(stripped) > 0 && (stripped[len(stripped)-1] == 'u' || stripped[len(stripped)-1] == 'U' ||
		stripped[len(stripped)-1] == 'l' || stripped[len(stripped)-1] == 'L') {
		stripped = stripped[:len(stripped)-1]
	}
	if v, err := strconv.ParseUint(stripped, 0, 64); err == nil {
		return int64(v), true
	}
	if val, defined := p.defines[tok]; defined {
		if val == "" {
			return 1, true
		}
		return p.evalPPExpr(val, depth+1)
	}
	return 0, true
}


func (p *Preprocessor) Process(source string) (string, error) {
	lines := strings.Split(joinLogicalLines(stripComments(joinContinuedLines(source))), "\n")
	var result strings.Builder
	
	// Stack for conditional compilation
	type condState struct {
		active bool  // Whether this block is active
		taken  bool  // Whether any branch was taken
	}
	condStack := []condState{{active: true, taken: false}}
	
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		
		// Handle preprocessor directives
		if strings.HasPrefix(trimmed, "#") {
			// Whitespace may appear between # and the directive
			// (`# define X` is valid C and SDL uses it).
			directive := strings.Fields("#" + strings.TrimSpace(trimmed[1:]))
			if len(directive) == 0 {
				continue
			}
			
			cmd := directive[0]
			
			switch cmd {
			case "#include":
				if !condStack[len(condStack)-1].active {
					continue
				}
				
				if len(directive) < 2 {
					return "", fmt.Errorf("line %d: #include requires filename", i+1)
				}
				
				filename := includePathToken(directive)
				originalFilename := ""
				if len(directive) > 1 {
					originalFilename = directive[1]
				}
				
				// Angle-bracket headers: try include paths first (project
				// headers like <SDL3/SDL_gpu.h> resolve via -I); only skip
				// when truly unfindable (real system headers).
				if strings.HasPrefix(originalFilename, "<") {
					if content, err := p.processInclude(filename); err == nil {
						result.WriteString(content)
						result.WriteString("\n")
						continue
					}
					result.WriteString(fmt.Sprintf("// Skipped system header: %s\n", originalFilename))
					continue
				}
				
				// Read and process included file
				content, err := p.processInclude(filename)
				if err != nil {
					// For now, just skip includes we can't find
					result.WriteString(fmt.Sprintf("// Skipped: #include %s\n", filename))
					continue
				}
				
				result.WriteString(content)
				result.WriteString("\n")
				
			case "#define":
				if !condStack[len(condStack)-1].active {
					continue
				}
				
				if len(directive) < 2 {
					return "", fmt.Errorf("line %d: #define requires name", i+1)
				}
				
				// Parse the full line to handle function-like macros
				// Format: #define NAME(params) body
				// or: #define NAME value
				restOfLine := strings.TrimSpace(line[strings.Index(line, directive[1]):])
				
				// Check if it's a function-like macro
				if len(restOfLine) > 0 && strings.Contains(restOfLine, "(") {
					// C rule: the `(` must IMMEDIATELY follow the name.
					// `NAME (body)` (space) and `NAME OTHER(...)` (parens
					// later in the body, e.g. SDL_WINDOW_HIDDEN) are
					// OBJECT macros. Take the first token, then check.
					tokEnd := 0
					for tokEnd < len(restOfLine) && isIdentifierChar(restOfLine[tokEnd]) {
						tokEnd++
					}
					name := restOfLine[:tokEnd]
					parenIdx := strings.Index(restOfLine, "(")
					
					// Check if paren is immediately after name (no space = function macro)
					if parenIdx == len(name) {
						// Function-like macro
						closeParenIdx := strings.Index(restOfLine, ")")
						if closeParenIdx > parenIdx {
							// Extract parameters
							paramsStr := restOfLine[parenIdx+1 : closeParenIdx]
							params := []string{}
							if strings.TrimSpace(paramsStr) != "" {
								for _, param := range strings.Split(paramsStr, ",") {
									params = append(params, strings.TrimSpace(param))
								}
							}
							
							// Extract body (everything after closing paren).
							// A trailing // comment is NOT part of the macro
							// (it would comment out the invocation line).
							body := stripDefineComment(strings.TrimSpace(restOfLine[closeParenIdx+1:]))
							
							if os.Getenv("SEAPP") != "" {
								fmt.Fprintf(os.Stderr, "MACRO %s(%v) = %q\n", name, params, body)
							}
							p.mu.Lock()
							p.funcMacros[name] = &FunctionMacro{
								Params: params,
								Body:   body,
							}
							p.mu.Unlock()
							continue
						}
					}
				}
				
				// Simple object-like macro (comments stripped: see above).
				name := directive[1]
				value := ""
				if len(directive) > 2 {
					value = stripDefineComment(strings.Join(directive[2:], " "))
				}
				
				p.Define(name, value)
				// Don't output the #define itself
				
			case "#ifdef":
				if len(directive) < 2 {
					return "", fmt.Errorf("line %d: #ifdef requires name", i+1)
				}
				
				name := directive[1]
				active := condStack[len(condStack)-1].active && p.IsDefined(name)
				condStack = append(condStack, condState{active: active, taken: active})
				
			case "#ifndef":
				if len(directive) < 2 {
					return "", fmt.Errorf("line %d: #ifndef requires name", i+1)
				}
				
				name := directive[1]
				active := condStack[len(condStack)-1].active && !p.IsDefined(name)
				condStack = append(condStack, condState{active: active, taken: active})
				
			case "#else":
				if len(condStack) <= 1 {
					return "", fmt.Errorf("line %d: #else without #ifdef/#ifndef", i+1)
				}
				
				parent := condStack[len(condStack)-2].active
				current := &condStack[len(condStack)-1]
				current.active = parent && !current.taken
				
			case "#endif":
				if len(condStack) <= 1 {
					return "", fmt.Errorf("line %d: #endif without #ifdef/#ifndef", i+1)
				}
				
				condStack = condStack[:len(condStack)-1]
				
			case "#if":
				// Parse #if expression
				// For now, support: #if defined(X) and #if !defined(X)
				restOfLine := strings.TrimSpace(line[strings.Index(line, "#if")+3:])
				condition := p.evaluateIfCondition(restOfLine)
				active := condStack[len(condStack)-1].active && condition
				condStack = append(condStack, condState{active: active, taken: active})
				
			case "#elif":
				if len(condStack) <= 1 {
					return "", fmt.Errorf("line %d: #elif without #if", i+1)
				}
				
				// Only evaluate if parent is active and no previous branch was taken
				parent := condStack[len(condStack)-2].active
				current := &condStack[len(condStack)-1]
				
				if parent && !current.taken {
					restOfLine := strings.TrimSpace(line[strings.Index(line, "#elif")+5:])
					condition := p.evaluateIfCondition(restOfLine)
					current.active = condition
					if condition {
						current.taken = true
					}
				} else {
					current.active = false
				}
				
			case "#undef":
				if !condStack[len(condStack)-1].active {
					continue
				}
				
				if len(directive) < 2 {
					continue
				}
				
				name := directive[1]
				p.mu.Lock()
				delete(p.defines, name)
				delete(p.funcMacros, name)
				p.mu.Unlock()
				
			case "#pragma":
				// Ignore pragmas
				
			default:
				// Unknown directive - skip
			}
			
			continue
		}
		
		// Only include line if we're in an active block
		if condStack[len(condStack)-1].active {
			// Expand macros in the line (proper text substitution)
			expanded := p.expandMacros(line)
			result.WriteString(expanded)
			result.WriteString("\n")
		}
	}
	
	return result.String(), nil
}

func (p *Preprocessor) processInclude(filename string) (string, error) {
	// Check if already processed (avoid cycles)
	if p.processed[filename] {
		return "", nil
	}
	
	// Try to find the file
	var fullPath string
	var found bool
	
	// If filename is an absolute path, try it directly first
	if filepath.IsAbs(filename) {
		if _, err := os.Stat(filename); err == nil {
			fullPath = filename
			found = true
		}
	}
	
	// Otherwise, search in include paths
	if !found {
		for _, searchPath := range p.includePaths {
			testPath := filepath.Join(searchPath, filename)
			if _, err := os.Stat(testPath); err == nil {
				fullPath = testPath
				found = true
				break
			}
		}
	}

	// Quoted includes also resolve relative to the including file first
	// (proper C behavior); do it here via the directory stack.
	if !found && len(p.dirStack) > 0 {
		testPath := filepath.Join(p.dirStack[len(p.dirStack)-1], filename)
		if _, err := os.Stat(testPath); err == nil {
			fullPath = testPath
			found = true
		}
	}
	
	if !found {
		return "", fmt.Errorf("include file not found: %s", filename)
	}

	// Canonicalize for the cycle guard: the same file requested under
	// different spellings ("x.h" vs "./x.h" vs absolute) must still
	// hit the guard, or two headers including each other recurse forever
	// (this OOMed the machine on bell's headers).
	if abs, err := filepath.Abs(fullPath); err == nil {
		fullPath = abs
	}
	if p.processed[fullPath] {
		return "", nil
	}
	
	// Read file
	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", err
	}
	
	// Mark as processed
	p.processed[fullPath] = true

	// Push this file's directory for relative nested includes.
	p.dirStack = append(p.dirStack, filepath.Dir(fullPath))
	defer func() { p.dirStack = p.dirStack[:len(p.dirStack)-1] }()
	
	// Extract types and function signatures from this header
	// (Do this BEFORE processing to catch declarations before they're preprocessed away)
	p.ExtractTypesFromHeader(fullPath)
	
	// Process all files the same way
	return p.Process(string(content))
}

// joinLogicalLines merges code lines with unbalanced open parens (multiline
// macro invocations and calls) so textual expansion sees whole invocations.
// Consumed newlines are re-emitted as blank lines, preserving line numbers.
// Directive lines never merge. Comments can't unbalance: strings are masked
// and // comments stripped for counting (matching C, where // ends at its
// own newline... which we preserve by stripping only joined-away ones).
func joinLogicalLines(source string) string {
	lines := strings.Split(source, "\n")
	var out []string
	i := 0
	for i < len(lines) {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			out = append(out, lines[i])
			i++
			continue
		}
		merged := stripLineComment(lines[i])
		consumed := 0
		for parenDepth(analysisMask(merged)) > 0 && i+consumed+1 < len(lines) {
			nxt := lines[i+consumed+1]
			if strings.HasPrefix(strings.TrimSpace(nxt), "#") {
				break
			}
			if consumed > 500 {
				break
			}
			consumed++
			merged += " " + stripLineComment(nxt)
		}
		out = append(out, merged)
		for k := 0; k < consumed; k++ {
			out = append(out, "")
		}
		i += 1 + consumed
	}
	return strings.Join(out, "\n")
}

// stripLineComment removes a // comment honoring string literals.
func stripLineComment(s string) string {
	m := maskStrings(s)
	if idx := strings.Index(m, "//"); idx >= 0 {
		return s[:idx]
	}
	return s
}

// analysisMask blanks string/char contents and /* */ comments for
// structural scanning (paren counting).
func analysisMask(s string) string {
	m := maskStrings(s)
	// Blank /* ... */ spans (may be unterminated on one line).
	for {
		start := strings.Index(m, "/*")
		if start < 0 {
			break
		}
		end := strings.Index(m[start+2:], "*/")
		if end < 0 {
			// Unterminated: blank to end of this analysis unit.
			b := []byte(m)
			for k := start; k < len(b); k++ {
				b[k] = ' '
			}
		m = string(b)
			break
		}
		b := []byte(m)
		for k := start; k < start+2+end+2 && k < len(b); k++ {
			b[k] = ' '
		}
		m = string(b)
	}
	return m
}

// parenDepth counts unbalanced open parens.
func parenDepth(s string) int {
	d := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '(' {
			d++
		} else if s[i] == ')' {
			d--
		}
	}
	return d
}

// stripComments removes /* */ and // comments (phase 3, before macro
// expansion in phase 4 — matching C). Newlines are preserved exactly so
// line numbers survive. Contents of strings/chars are untouched, so
// apostrophes in comments can no longer corrupt string-aware matching
// (e.g. `bell's` masking everything to the next quote).
func stripComments(source string) string {
	var out strings.Builder
	out.Grow(len(source))
	i := 0
	n := len(source)
	for i < n {
		c := source[i]
		if c == '"' || c == '\'' {
			q := c
			out.WriteByte(c)
			i++
			for i < n {
				out.WriteByte(source[i])
				if source[i] == '\\' {
					i++
					if i < n {
						out.WriteByte(source[i])
						i++
					}
					continue
				}
				if source[i] == q {
					i++
					break
				}
				i++
			}
			continue
		}
		if c == '/' && i+1 < n && source[i+1] == '/' {
			// Line comment: skip to newline (keep the newline).
			for i < n && source[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < n && source[i+1] == '*' {
			// Block comment: blank it, preserving newlines.
			i += 2
			for i < n {
				if source[i] == '*' && i+1 < n && source[i+1] == '/' {
					i += 2
					break
				}
				if source[i] == '\n' {
					out.WriteByte('\n')
				} else {
					out.WriteByte(' ')
				}
				i++
			}
			continue
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

// joinContinuedLines folds backslash-newline continuations (C phase 2).
// Without this, multi-line #if conditions evaluate truncated (dropping
// whole blocks) and the stray backslashes leak into code and kill the
// lexer (which used to return EOF on unknown bytes, silently ending the
// entire translation unit).
func joinContinuedLines(source string) string {
	// Handle \r\n and \n continuations.
	source = strings.ReplaceAll(source, "\\\r\n", "")
	return strings.ReplaceAll(source, "\\\n", "")
}

func (p *Preprocessor) processHeaderSimple(source string) (string, error) {
	lines := strings.Split(joinLogicalLines(stripComments(joinContinuedLines(source))), "\n")
	var result strings.Builder
	
	type condState struct {
		active bool
		taken  bool
	}
	condStack := []condState{{active: true, taken: false}}
	
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		
		if strings.HasPrefix(trimmed, "#") {
			// Whitespace may appear between # and the directive
			// (`# define X` is valid C and SDL uses it).
			directive := strings.Fields("#" + strings.TrimSpace(trimmed[1:]))
			if len(directive) == 0 {
				continue
			}
			
			cmd := directive[0]
			
			switch cmd {
			case "#include":
				if !condStack[len(condStack)-1].active {
					continue
				}
				
				if len(directive) < 2 {
					continue
				}
				
				filename := includePathToken(directive)
				
				// Recursively include
				content, err := p.processInclude(filename)
				if err != nil {
					// Skip system headers we can't find
					result.WriteString(fmt.Sprintf("// Skipped: #include %s\n", filename))
					continue
				}
				
				result.WriteString(content)
				result.WriteString("\n")
				
			case "#define":
				if !condStack[len(condStack)-1].active {
					continue
				}
				
				if len(directive) < 2 {
					continue
				}
				
				name := directive[1]
				value := ""
				if len(directive) > 2 {
					value = stripDefineComment(strings.Join(directive[2:], " "))
				}
				
				p.Define(name, value)
				// Don't output the #define itself
				
			case "#ifdef":
				if len(directive) < 2 {
					continue
				}
				
				name := directive[1]
				active := condStack[len(condStack)-1].active && p.IsDefined(name)
				condStack = append(condStack, condState{active: active, taken: active})
				
			case "#ifndef":
				if len(directive) < 2 {
					continue
				}
				
				name := directive[1]
				active := condStack[len(condStack)-1].active && !p.IsDefined(name)
				condStack = append(condStack, condState{active: active, taken: active})
				
			case "#else":
				if len(condStack) <= 1 {
					continue
				}
				
				parent := condStack[len(condStack)-2].active
				current := &condStack[len(condStack)-1]
				current.active = parent && !current.taken
				
			case "#endif":
				if len(condStack) <= 1 {
					continue
				}
				
				condStack = condStack[:len(condStack)-1]
				
			default:
				// Keep other directives as comments
				if condStack[len(condStack)-1].active {
					result.WriteString("// ")
					result.WriteString(trimmed)
					result.WriteString("\n")
				}
			}
			
			continue
		}
		
		// Copy non-directive lines if active, with macro expansion
		if condStack[len(condStack)-1].active {
			expanded := p.expandMacros(line)
			result.WriteString(expanded)
			result.WriteString("\n")
		}
	}
	
	return result.String(), nil
}

// stripDefineComment removes // and /* */ comments from a #define body.
// The comment is NOT part of the macro: keeping it would comment out the
// rest of whichever line invokes the macro (e.g. RMAPI ... -> inline //...).
// Quote-aware so "http://..." values survive.
func stripDefineComment(s string) string {
	var out strings.Builder
	inStr := false
	esc := false
	i := 0
	for i < len(s) {
		c := s[i]
		if inStr {
			out.WriteByte(c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			i++
			continue
		}
		if c == '"' {
			inStr = true
			out.WriteByte(c)
			i++
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '/' {
			break
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				break
			}
			out.WriteByte(' ')
			i += end + 4
			continue
		}
		out.WriteByte(c)
		i++
	}
	return strings.TrimSpace(out.String())
}


// includePathToken extracts the file path from a split #include directive.
// Only the first token is the path (trailing // or /* */ comments belong
// to the line, not the filename); surrounding quotes/angles are stripped.
func includePathToken(directive []string) string {
	if len(directive) < 2 {
		return ""
	}
	return strings.Trim(directive[1], `"<>`)
}

func (p *Preprocessor) expandMacros(line string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := line

	// Expand to fixpoint: macro bodies routinely contain other macro
	// calls (e.g. SDL_COMPILE_TIME_ASSERT wrapping SDL_static_cast) that
	// only become visible after the outer expansion. Cap iterations to
	// stay total (self-recursive macros stabilize or hit the cap).
	for iter := 0; iter < 50; iter++ {
		prev := result
		// First expand function-like macros
		for name, macro := range p.funcMacros {
			result = p.expandFunctionMacro(result, name, macro)
		}

		// Then expand object-like macros
		for name, value := range p.defines {
			// Use word boundary matching
			result = replaceIdentifier(result, name, value)
		}
		if result == prev {
			break
		}
	}
	
	return result
}

// maskStrings blanks the contents of "..." and '...' literals (length
// preserved) so textual macro processing never matches identifiers,
// parens, commas, # or ## inside them (e.g. "#?RADIANCE\n" or a
// single-letter param inside a message string).
func maskStrings(s string) string {
	b := []byte(s)
	i := 0
	for i < len(b) {
		if b[i] == '"' || b[i] == '\'' {
			q := b[i]
			j := i + 1
			for j < len(b) {
				if b[j] == '\\' {
					j += 2
					continue
				}
				if b[j] == q {
					break
				}
				j++
			}
			for k := i + 1; k < j && k < len(b); k++ {
				b[k] = ' '
			}
			i = j + 1
			} else {
			i++
		}
	}
	return string(b)
}

func (p *Preprocessor) expandFunctionMacro(text string, name string, macro *FunctionMacro) string {
	var result strings.Builder
	masked := maskStrings(text)
	i := 0

	for i < len(text) {
		// Check if we found the macro name (outside strings)
		if strings.HasPrefix(masked[i:], name) {
			// Check if it's a complete identifier followed by (
			// (masked: strings/chars can't start or split an invocation)
			if (i == 0 || !isIdentifierChar(masked[i-1])) {
				// Check for opening paren
				j := i + len(name)
				// Skip whitespace
				for j < len(masked) && (masked[j] == ' ' || masked[j] == '\t') {
					j++
				}

				if j < len(masked) && masked[j] == '(' {
					// This is a function macro invocation
					// Extract arguments (structure from masked text,
					// content from original to preserve strings)
					j++ // skip (
					depth := 1
					var args []string
					currentArg := ""

					for j < len(masked) && depth > 0 {
						if masked[j] == '(' {
							depth++
							currentArg += string(text[j])
						} else if masked[j] == ')' {
							depth--
							if depth == 0 {
								// End of macro invocation
								if currentArg != "" || len(args) > 0 {
									args = append(args, strings.TrimSpace(currentArg))
								}
								break
							}
							currentArg += string(text[j])
						} else if masked[j] == ',' && depth == 1 {
							// Argument separator
							args = append(args, strings.TrimSpace(currentArg))
							currentArg = ""
						} else {
							currentArg += string(text[j])
						}
						j++
					}
					
					// Substitute parameters in body (#param stringifies
					// first, so the # doesn't survive into the output).
					expansion := macro.Body
					for idx, param := range macro.Params {
						if idx < len(args) {
							expansion = stringifyParam(expansion, param, args[idx])
						}
					}
					for idx, param := range macro.Params {
						if idx < len(args) {
							expansion = replaceIdentifier(expansion, param, args[idx])
						}
					}
					// Token pasting (SDL compile-time asserts rely on it).
					expansion = pasteTokens(expansion)
					
					if os.Getenv("SEAPP") != "" {
						fmt.Fprintf(os.Stderr, "EXPAND %s args=%q -> %q\n", name, args, expansion)
					}
					result.WriteString(expansion)
					i = j + 1 // Skip past the closing )
					continue
				}
			}
		}
		
		result.WriteByte(text[i])
		i++
	}
	
	return result.String()
}

// stringifyParam replaces #param with the quoted argument text (C #
// operator). Skips ## (handled separately by pasteTokens).
func stringifyParam(body, param, arg string) string {
	// Match on masked text (strings blanked), splice from original.
	masked := maskStrings(body)
	var out strings.Builder
	i := 0
	for i < len(body) {
		if masked[i] == '#' {
			if i+1 < len(masked) && masked[i+1] == '#' {
				out.WriteString("##")
				i += 2
				continue
			}
			j := i + 1
			for j < len(masked) && (masked[j] == ' ' || masked[j] == '\t') {
				j++
			}
			if strings.HasPrefix(masked[j:], param) {
				k := j + len(param)
				if (j == 0 || !isIdentifierChar(masked[j-1])) && (k >= len(masked) || !isIdentifierChar(masked[k])) {
					out.WriteString(strconv.Quote(arg))
					i = k
					continue
				}
			}
		}
		out.WriteByte(body[i])
		i++
	}
	return out.String()
}

func pasteTokens(s string) string {
	for {
		masked := maskStrings(s)
		idx := strings.Index(masked, "##")
		if idx < 0 {
			return s
		}
		left := strings.TrimRight(s[:idx], " \t")
		right := strings.TrimLeft(s[idx+2:], " \t")
		if left == "" || right == "" {
			return s
		}
		s = left + right
	}
}

func replaceIdentifier(text, identifier, replacement string) string {
	masked := maskStrings(text)
	var result strings.Builder
	i := 0

	for i < len(text) {
		// Check if we found the identifier (outside strings)
		if strings.HasPrefix(masked[i:], identifier) {
			// Check if it's a complete identifier (not part of another word)
			if (i == 0 || !isIdentifierChar(masked[i-1])) &&
				(i+len(identifier) >= len(masked) || !isIdentifierChar(masked[i+len(identifier)])) {
				result.WriteString(replacement)
				i += len(identifier)
				continue
			}
		}

		result.WriteByte(text[i])
		i++
	}

	return result.String()
}

func isIdentifierChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

// ProcessFile is a convenience function to preprocess a file
func (p *Preprocessor) ProcessFile(filename string) (string, error) {
	content, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}
	
	return p.Process(string(content))
}

// ExtractTypesFromHeader parses a header file to extract typedef and struct definitions
func (p *Preprocessor) ExtractTypesFromHeader(filename string) error {
	content, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	
	lines := strings.Split(string(content), "\n")
	
	// First pass: collect all struct and typedef definitions
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		
		// Match: typedef struct { ... } TypeName;
		if strings.HasPrefix(line, "typedef struct") {
			// Collect multi-line struct definition
			structDef := line
			braceCount := strings.Count(line, "{") - strings.Count(line, "}")
			
			for braceCount > 0 && i+1 < len(lines) {
				i++
				nextLine := strings.TrimSpace(lines[i])
				structDef += " " + nextLine
				braceCount += strings.Count(nextLine, "{") - strings.Count(nextLine, "}")
			}
			
			// Parse the typedef
			p.parseTypedefStruct(structDef)
		} else if strings.HasPrefix(line, "typedef ") && !strings.Contains(line, "{") {
			// Simple typedef alias: typedef OldType NewType;
			p.parseSimpleTypedef(line)
		}
		// Also extract function declarations for tracking return types
		// Look for RLAPI function declarations (raylib API functions)
		if strings.Contains(line, "RLAPI ") || (strings.Contains(line, "(") && strings.Contains(line, ");")) {
			p.parseFunctionDeclaration(line)
		}
	}
	
	// Second pass: resolve struct sizes now that all structs are known
	p.resolveStructSizes()
	
	return nil
}

// parseSimpleTypedef parses a simple type alias
func (p *Preprocessor) parseSimpleTypedef(line string) {
	// Example: typedef Texture Texture2D;
	// Example: typedef struct Color Color;
	
	line = strings.TrimPrefix(line, "typedef")
	line = strings.TrimSpace(line)
	line = strings.TrimSuffix(line, ";")
	line = strings.TrimSpace(line)
	
	// Split into tokens
	tokens := strings.Fields(line)
	if len(tokens) < 2 {
		return
	}
	
	// Last token is the new type name
	newTypeName := tokens[len(tokens)-1]
	// Everything before is the old type
	oldType := strings.Join(tokens[:len(tokens)-1], " ")
	
	// For simple aliases, we just create a struct with the same definition as the original
	// If the original is in our structMap, copy it
	if existing, ok := p.structMap[oldType]; ok {
		p.typedefMap[newTypeName] = existing
		p.structMap[newTypeName] = existing
	} else {
		// Otherwise, create a placeholder struct
		// This allows the typedef to be recognized even if we don't know the full definition
		p.typedefMap[newTypeName] = &StructDef{
			Name: newTypeName,
			Members: []StructMember{},
		}
	}
}

// parseTypedefStruct parses a typedef struct definition
func (p *Preprocessor) parseTypedefStruct(def string) {
	// Example: typedef struct { float x; float y; } Vector2;
	// Example: typedef struct Color { unsigned char r, g, b, a; } Color;
	// Example: typedef struct RenderTexture { ... } RenderTexture;
	
	// Find the type name (after closing brace)
	closeBraceIdx := strings.LastIndex(def, "}")
	if closeBraceIdx == -1 {
		return
	}
	
	afterBrace := strings.TrimSpace(def[closeBraceIdx+1:])
	afterBrace = strings.TrimSuffix(afterBrace, ";")
	afterBrace = strings.TrimSpace(afterBrace)
	
	typeName := afterBrace
	if typeName == "" {
		return
	}
	
	// Extract struct name if it exists (between "struct" and "{")
	var structName string
	openBraceIdx := strings.Index(def, "{")
	if openBraceIdx != -1 {
		beforeBrace := def[:openBraceIdx]
		beforeBrace = strings.TrimPrefix(beforeBrace, "typedef")
		beforeBrace = strings.TrimPrefix(beforeBrace, "struct")
		structName = strings.TrimSpace(beforeBrace)
	}
	
	// Extract member definitions between braces
	if openBraceIdx == -1 {
		return
	}
	
	membersStr := def[openBraceIdx+1 : closeBraceIdx]
	members := p.parseStructMembers(membersStr)
	
	// Calculate total size
	totalSize := 0
	for _, member := range members {
		if member.Offset+member.Size > totalSize {
			totalSize = member.Offset + member.Size
		}
	}
	
	// Create struct type
	structType := &StructDef{
		Name:    typeName,
		Members: members,
		Size:    totalSize,
	}
	
	// Store under the typedef name
	p.typedefMap[typeName] = structType
	p.structMap[typeName] = structType
	
	// Also store under the struct name if it exists and is different
	if structName != "" && structName != typeName {
		p.structMap[structName] = structType
	}
}

// parseStructMembers parses struct member declarations
func (p *Preprocessor) parseStructMembers(membersStr string) []StructMember {
	var members []StructMember
	offset := 0
	
	// Split by semicolon to get individual member declarations
	declarations := strings.Split(membersStr, ";")
	
	for _, decl := range declarations {
		decl = strings.TrimSpace(decl)
		if decl == "" {
			continue
		}
		
		// Parse declaration like "float x, y" or "unsigned char r"
		parts := strings.Fields(decl)
		if len(parts) < 2 {
			continue
		}
		
		// Build type name from all but last part
		typeParts := parts[:len(parts)-1]
		typeStr := strings.Join(typeParts, " ")
		
		// Last part contains variable name(s), possibly comma-separated
		namesStr := parts[len(parts)-1]
		names := strings.Split(namesStr, ",")
		
		memberType := p.mapTypeString(typeStr)
		// Get size for basic types only during parsing
		// Struct sizes will be calculated later
		memberSize := p.getBasicTypeSize(memberType)
		
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name != "" {
				members = append(members, StructMember{
					Name:   name,
					Type:   memberType,
					Offset: offset,
					Size:   memberSize,
				})
				offset += memberSize
			}
		}
	}
	
	return members
}

// getBasicTypeSize returns size of basic types without recursion
func (p *Preprocessor) getBasicTypeSize(typ string) int {
	typ = strings.TrimSpace(typ)
	
	// Pointers are 8 bytes
	if strings.HasSuffix(typ, "*") {
		return 8
	}
	
	// Basic types
	switch typ {
	case "char", "signed char", "unsigned char":
		return 1
	case "short", "short int", "signed short", "unsigned short":
		return 2
	case "int", "signed int", "unsigned int":
		return 4
	case "long", "signed long", "unsigned long", "long long", "signed long long", "unsigned long long":
		return 8
	case "float":
		return 4
	case "double":
		return 8
	case "void":
		return 0
	default:
		// For unknown types (including other structs), use a placeholder
		// The actual size will be resolved later
		return 0
	}
}

// getTypeSize returns the size in bytes of a type
func (p *Preprocessor) getTypeSize(typ string) int {
	return p.getTypeSizeHelper(typ, make(map[string]bool))
}

func (p *Preprocessor) getTypeSizeHelper(typ string, visited map[string]bool) int {
	// Prevent infinite recursion
	if visited[typ] {
		return 8 // Default to break cycle
	}
	visited[typ] = true
	
	typ = strings.TrimSpace(typ)
	
	// Pointers are 8 bytes
	if strings.HasSuffix(typ, "*") {
		return 8
	}
	
	// Check if it's a known struct
	if structDef, ok := p.structMap[typ]; ok {
		return structDef.Size
	}
	
	// Check for struct prefix
	if strings.HasPrefix(typ, "struct ") {
		structName := strings.TrimSpace(typ[7:])
		if structDef, ok := p.structMap[structName]; ok {
			return structDef.Size
		}
	}
	
	// Check typedef map
	if structDef, ok := p.typedefMap[typ]; ok {
		return structDef.Size
	}
	
	// Basic types
	switch typ {
	case "char", "signed char", "unsigned char":
		return 1
	case "short", "short int", "signed short", "unsigned short":
		return 2
	case "int", "signed int", "unsigned int":
		return 4
	case "long", "signed long", "unsigned long", "long long", "signed long long", "unsigned long long":
		return 8
	case "float":
		return 4
	case "double":
		return 8
	case "void":
		return 0
	default:
		return 8 // Default for unknown types
	}
}

// mapTypeString converts C type string to internal type
func (p *Preprocessor) mapTypeString(typeStr string) string {
	typeStr = strings.TrimSpace(typeStr)
	
	switch typeStr {
	case "int", "signed int":
		return "int"
	case "unsigned int", "unsigned":
		return "int" // Treat as int for now
	case "char", "signed char":
		return "char"
	case "unsigned char":
		return "char" // Treat as char for now
	case "float":
		return "float"
	case "double":
		return "double"
	case "void":
		return "void"
	default:
		// Could be a pointer or custom type
		if strings.HasSuffix(typeStr, "*") {
			return typeStr // Keep pointer notation
		}
		return typeStr // Return as-is for custom types
	}
}

// parseFunctionDeclaration parses a function declaration to extract return type
func (p *Preprocessor) parseFunctionDeclaration(line string) {
	// Clean up the line
	line = strings.TrimSpace(line)
	line = strings.ReplaceAll(line, "RLAPI ", "")
	
	// Skip if it doesn't look like a function declaration
	if !strings.Contains(line, "(") || !strings.Contains(line, ")") {
		return
	}
	
	// Find the function name and return type
	// Format: ReturnType FunctionName(params);
	parenIdx := strings.Index(line, "(")
	if parenIdx == -1 {
		return
	}
	
	beforeParen := strings.TrimSpace(line[:parenIdx])
	parts := strings.Fields(beforeParen)
	if len(parts) < 2 {
		return
	}
	
	// Last part is function name, everything before is return type
	funcName := parts[len(parts)-1]
	returnType := strings.Join(parts[:len(parts)-1], " ")
	
	// Extract parameter types
	paramStart := parenIdx + 1
	paramEnd := strings.Index(line[paramStart:], ")")
	if paramEnd == -1 {
		return
	}
	
	paramsStr := strings.TrimSpace(line[paramStart : paramStart+paramEnd])
	var paramTypes []string
	
	if paramsStr != "" && paramsStr != "void" {
		// Split by comma
		params := strings.Split(paramsStr, ",")
		for _, param := range params {
			param = strings.TrimSpace(param)
			// Extract just the type (remove parameter name)
			paramParts := strings.Fields(param)
			if len(paramParts) > 0 {
				// Simple heuristic: type is everything except last token (which is usually the name)
				// But if there's a *, it might be part of the type
				if len(paramParts) == 1 {
					paramTypes = append(paramTypes, paramParts[0])
				} else {
					// Take all but last unless last is a pointer indicator
					paramType := strings.Join(paramParts[:len(paramParts)-1], " ")
					paramTypes = append(paramTypes, paramType)
				}
			}
		}
	}
	
	// Store the function signature
	p.functionSigs[funcName] = &FunctionSignature{
		ReturnType: returnType,
		ParamTypes: paramTypes,
	}
}

// resolveStructSizes computes actual sizes of all structs after they're all parsed
func (p *Preprocessor) resolveStructSizes() {
// Keep resolving until no changes (handles nested structs)
maxIterations := 10
for iter := 0; iter < maxIterations; iter++ {
changed := false

for _, structDef := range p.structMap {
if structDef.Size > 0 {
continue // Already computed
}

// Compute size from members
totalSize := 0
allResolved := true

for i := range structDef.Members {
member := &structDef.Members[i]
if member.Size == 0 {
// Try to resolve the member size
memberSize := p.getTypeSizeHelper(member.Type, make(map[string]bool))
if memberSize > 0 {
member.Size = memberSize
member.Offset = totalSize
totalSize += memberSize
} else {
allResolved = false
}
} else {
totalSize = member.Offset + member.Size
}
}

if allResolved && totalSize > 0 {
structDef.Size = totalSize
changed = true
}
}

if !changed {
break
}
}
}
