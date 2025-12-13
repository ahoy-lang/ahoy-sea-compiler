package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Preprocessor handles C preprocessor directives
type Preprocessor struct {
	defines       map[string]string
	funcMacros    map[string]*FunctionMacro // Function-like macros
	includePaths  []string
	processed     map[string]bool  // Track processed files to avoid cycles
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
		includePaths: []string{"/usr/include", "/usr/local/include", ".", "/home/lee/Documents/clibs/raylib/src"},
		processed:    make(map[string]bool),
		typedefMap:   make(map[string]*StructDef),
		structMap:    make(map[string]*StructDef),
		functionSigs: make(map[string]*FunctionSignature),
	}
	
	// Add standard built-in macros
	p.defines["NULL"] = "0"
	p.defines["true"] = "1"
	p.defines["false"] = "0"
	
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

func (p *Preprocessor) Process(source string) (string, error) {
	lines := strings.Split(source, "\n")
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
			directive := strings.Fields(trimmed)
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
				
				filename := strings.Join(directive[1:], " ")
				originalFilename := filename
				filename = strings.Trim(filename, `"<>`)
				
				// Skip system headers (those in angle brackets)
				if strings.HasPrefix(originalFilename, "<") {
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
					// Find the opening paren
					parenIdx := strings.Index(restOfLine, "(")
					name := restOfLine[:parenIdx]
					
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
							
							// Extract body (everything after closing paren)
							body := strings.TrimSpace(restOfLine[closeParenIdx+1:])
							
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
				
				// Simple object-like macro
				name := directive[1]
				value := ""
				if len(directive) > 2 {
					value = strings.Join(directive[2:], " ")
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
	
	if !found {
		return "", fmt.Errorf("include file not found: %s", filename)
	}
	
	// Read file
	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", err
	}
	
	// Mark as processed
	p.processed[fullPath] = true
	
	// Extract types and function signatures from this header
	// (Do this BEFORE processing to catch declarations before they're preprocessed away)
	p.ExtractTypesFromHeader(fullPath)
	
	// Process all files the same way
	return p.Process(string(content))
}

func (p *Preprocessor) processHeaderSimple(source string) (string, error) {
	lines := strings.Split(source, "\n")
	var result strings.Builder
	
	type condState struct {
		active bool
		taken  bool
	}
	condStack := []condState{{active: true, taken: false}}
	
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		
		if strings.HasPrefix(trimmed, "#") {
			directive := strings.Fields(trimmed)
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
				
				filename := strings.Join(directive[1:], " ")
				filename = strings.Trim(filename, `"<>`)
				
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
					value = strings.Join(directive[2:], " ")
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

func (p *Preprocessor) expandMacros(line string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	
	result := line
	
	// First expand function-like macros
	for name, macro := range p.funcMacros {
		result = p.expandFunctionMacro(result, name, macro)
	}
	
	// Then expand object-like macros
	for name, value := range p.defines {
		// Use word boundary matching
		result = replaceIdentifier(result, name, value)
	}
	
	return result
}

func (p *Preprocessor) expandFunctionMacro(text string, name string, macro *FunctionMacro) string {
	var result strings.Builder
	i := 0
	
	for i < len(text) {
		// Check if we found the macro name
		if strings.HasPrefix(text[i:], name) {
			// Check if it's a complete identifier followed by (
			if (i == 0 || !isIdentifierChar(text[i-1])) {
				// Check for opening paren
				j := i + len(name)
				// Skip whitespace
				for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
					j++
				}
				
				if j < len(text) && text[j] == '(' {
					// This is a function macro invocation
					// Extract arguments
					j++ // skip (
					depth := 1
					var args []string
					currentArg := ""
					
					for j < len(text) && depth > 0 {
						if text[j] == '(' {
							depth++
							currentArg += string(text[j])
						} else if text[j] == ')' {
							depth--
							if depth == 0 {
								// End of macro invocation
								if currentArg != "" || len(args) > 0 {
									args = append(args, strings.TrimSpace(currentArg))
								}
								break
							}
							currentArg += string(text[j])
						} else if text[j] == ',' && depth == 1 {
							// Argument separator
							args = append(args, strings.TrimSpace(currentArg))
							currentArg = ""
						} else {
							currentArg += string(text[j])
						}
						j++
					}
					
					// Substitute parameters in body
					expansion := macro.Body
					for idx, param := range macro.Params {
						if idx < len(args) {
							expansion = replaceIdentifier(expansion, param, args[idx])
						}
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

func replaceIdentifier(text, identifier, replacement string) string {
	var result strings.Builder
	i := 0
	
	for i < len(text) {
		// Check if we found the identifier
		if strings.HasPrefix(text[i:], identifier) {
			// Check if it's a complete identifier (not part of another word)
			if (i == 0 || !isIdentifierChar(text[i-1])) &&
			   (i+len(identifier) >= len(text) || !isIdentifierChar(text[i+len(identifier)])) {
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
