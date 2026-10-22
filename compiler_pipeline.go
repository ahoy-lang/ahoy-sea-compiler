package main

import (
	"runtime"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Cached debug-trace flags. os.Getenv takes a process-wide RWMutex on every
// call; the SEADBG/SEADEBUG checks sat in per-expression hot paths
// (memberArrayInfo runs per member access) and showed up in CPU profiles, so
// they are read once at startup. Debug tracing is process-scoped anyway.
var (
	envSeaDbg     = os.Getenv("SEADBG") != ""
	envSeaDebug   = os.Getenv("SEADEBUG") != ""
	envSeaParse   = os.Getenv("SEAPARSE") != ""
	envSeaPP      = os.Getenv("SEAPP") != ""
	envSeaPPTrace = os.Getenv("SEA_PPTRACE") != ""
)

type CompilerPipeline struct {
	source     string
	sourceFile string // main input path (for relative #include resolution)
	outputFile string // -o target (used by the single-pass path, which links inside Compile)
	ast        *ASTNode
	ir         []*IRInstruction
	assembly   string

	preprocessor *Preprocessor
	parser       *Parser
	selector     *InstructionSelector
	allocator    *RegisterAllocator
	usedRegs     []int
	emitter      *CodeEmitter

	options CompilerOptions
}

type CompilerOptions struct {
	OptimizationLevel int
	DebugInfo         bool
	Verbose           bool
	UseLinearScan     bool
	UseNativeBackend  bool
	NoPreprocess      bool     // Skip preprocessing
	LibraryFlags      []string // Additional library flags like -lc, -lraylib
	IncludePaths      []string // Additional #include search paths (-I)
	Defines           []string // -D defines (name or name=value), applied to the preprocessor
	// UseFastAsm selects the built-in in-process assembler (fastasm.go),
	// which writes an ELF .o directly instead of shelling out to GNU as.
	// Default on (tcc-style: no external assembler dependency); falls back
	// to GNU as automatically if in-process assembly fails. Disable with
	// -fno-fast-asm. Optimization passes, when they exist, are gated behind
	// OptimizationLevel > 0; level 0 is always the lean non-optimizing path.
	UseFastAsm bool
	// NoParallelSelect disables parallel instruction selection (one worker
	// per function) and uses the sequential fallback instead. Disable with
	// -fno-parallel-select. Default off (parallel selection is on).
	NoParallelSelect bool
	// NoParallelEmit disables parallel code emission (one worker per
	// function) and uses the sequential fallback instead. Disable with
	// -fno-parallel-emit. Default off (parallel emission is on).
	NoParallelEmit bool
	// SinglePass selects the experimental single-pass backend (spass.go):
	// AST -> machine code directly, no IR, no register-allocation pass,
	// no assembly text. TCC-style spill discipline at -O0; -O1 enables
	// short jumps. Default off; enable with -single-pass.
	SinglePass bool
}

func NewCompilerPipeline(source string, options CompilerOptions) *CompilerPipeline {
	return &CompilerPipeline{
		source:  source,
		options: options,
	}
}

// runPreprocessor runs phase 0 and returns the preprocessed source.
// Shared by the default pipeline and the single-pass backend.
func (cp *CompilerPipeline) runPreprocessor() (string, error) {
	preprocessedSource := cp.source

	if !cp.options.NoPreprocess {
		if cp.options.Verbose {
			fmt.Println("\n[0/5] Preprocessing...")
		}
		start := time.Now()

		// Use our simple preprocessor to handle #include and #define
		cp.preprocessor = NewPreprocessor()
		for _, inc := range cp.options.IncludePaths {
			cp.preprocessor.AddIncludePath(inc)
		}
		// Command-line -D defines (e.g. -D BACKEND_RAYLIB like ahoy-bin
		// passes through to the C compiler for the generated #ifdefs).
		for _, def := range cp.options.Defines {
			name, value := def, "1"
			if eq := strings.IndexByte(def, '='); eq >= 0 {
				name, value = def[:eq], def[eq+1:]
			}
			cp.preprocessor.Define(name, value)
		}
		if cp.sourceFile != "" {
			cp.preprocessor.AddIncludePath(filepath.Dir(cp.sourceFile))
		}
		var err error
		preprocessedSource, err = cp.preprocessor.Process(cp.source)
		if err != nil {
			return "", fmt.Errorf("preprocessing error: %w", err)
		}
		// The stdio stream globals live in the skipped <stdio.h>; declare
		// them so the linker resolves the real libc objects (bell prints
		// diagnostics via fprintf(stderr, ...)).
		preprocessedSource = "extern FILE* stderr;\nextern FILE* stdout;\nextern FILE* stdin;\n" + preprocessedSource
		// Save preprocessed output for debugging (verbose only: this is a
		// ~15MB disk write that cost real wall time on every compile).
		if cp.options.Verbose {
			os.WriteFile("/tmp/preprocessed.c", []byte(preprocessedSource), 0644)
		}
		if os.Getenv("SEA_PPONLY") != "" {
			// Debug fast path: stop after preprocessing.
			return "", nil
		}

		if cp.options.Verbose {
			fmt.Printf("  Completed in %v\n", time.Since(start))
		}
	}
	return preprocessedSource, nil
}

// runParser runs phase 1 (builds cp.ast from source text) and drops the
// token stream so parse-phase memory can be reclaimed. Shared by both
// backends.
func (cp *CompilerPipeline) runParser(preprocessedSource string) error {
	if cp.options.Verbose {
		fmt.Println("\n[1/5] Parsing...")
	}
	start := time.Now()

	// Parser will extract structs, typedefs, and functions from the preprocessed source
	cp.parser = NewParser(preprocessedSource)
	var err error
	cp.ast, err = cp.parser.Parse()
	if err != nil {
		return fmt.Errorf("parse error: %w", err)
	}
	// The token stream is dead once the AST exists; drop it (and the
	// source text) so the parse-phase memory can be reclaimed.
	cp.parser.tokens = nil
	cp.parser.compiler = nil
	cp.source = ""
	runtime.GC()
	if cp.options.Verbose {
		fmt.Printf("  Completed in %v\n", time.Since(start))
	}
	return nil
}

// symbolTables merges parser + preprocessor tables for codegen. Shared by
// the default pipeline (fills the selector) and the single-pass backend.
func (cp *CompilerPipeline) symbolTables() (map[string]*StructDef, map[string]string, map[string]int, map[string]*FunctionSignature) {
	structs := cp.parser.structs
	typedefs := cp.parser.typedefs
	enums := cp.parser.enums
	if structs == nil {
		structs = make(map[string]*StructDef)
	}

	// Also add structs from headers (preprocessor)
	if cp.preprocessor != nil {
		for structName, structDef := range cp.preprocessor.structMap {
			// Only add if not already defined in source
			if _, exists := structs[structName]; !exists {
				// Convert from preprocessor StructDef to parser StructDef
				members := []StructMember{}
				for _, m := range structDef.Members {
					members = append(members, StructMember{
						Name:   m.Name,
						Type:   m.Type,
						Offset: m.Offset,
						Size:   m.Size,
					})
				}
				structs[structName] = &StructDef{
					Name:    structDef.Name,
					Members: members,
					Size:    structDef.Size,
				}
			}
		}
	}

	// Extract function signatures from parsed AST
	funcs := make(map[string]*FunctionSignature)
	for _, child := range cp.ast.Children {
		if child.Type == NodeFunction {
			funcs[child.Name] = &FunctionSignature{
				ReturnType: child.ReturnType,
				ParamTypes: child.ParamTypes,
			}
		}
	}

	// Also add function signatures from headers (preprocessor)
	if cp.preprocessor != nil {
		for funcName, funcSig := range cp.preprocessor.functionSigs {
			// Only add if not already defined in source
			if _, exists := funcs[funcName]; !exists {
				funcs[funcName] = funcSig
			}
		}
	}
	return structs, typedefs, enums, funcs
}

// definedFuncSet returns the set of function names with bodies in the TU
// (single linear pass over top-level nodes).
func definedFuncSet(ast *ASTNode) map[string]bool {
	m := make(map[string]bool)
	for _, child := range ast.Children {
		if child.Type == NodeFunction && child.Name != "" && len(child.Children) > 0 {
			m[child.Name] = true
		}
	}
	return m
}

// compileSinglePass runs the experimental single-pass backend: preprocess
// -> parse -> spass direct codegen -> object -> link.
func (cp *CompilerPipeline) compileSinglePass() error {
	preprocessedSource, err := cp.runPreprocessor()
	if err != nil {
		return err
	}
	if os.Getenv("SEA_PPONLY") != "" {
		return nil
	}
	if err := cp.runParser(preprocessedSource); err != nil {
		return err
	}
	if cp.options.Verbose {
		fmt.Println("\n[2/3] Single-pass codegen...")
	}
	start := time.Now()
	structs, typedefs, enums, funcs := cp.symbolTables()
	sp := NewSPass(structs, typedefs, enums, funcs, nil, cp.options.OptimizationLevel)
	if err := sp.genTU(cp.ast); err != nil {
		return fmt.Errorf("single-pass error: %w", err)
	}
	obj := sp.finish()
	if cp.options.Verbose {
		fmt.Printf("  Generated %d text + %d rodata bytes\n", len(obj.Text), len(obj.Rodata))
		fmt.Printf("  Completed in %v\n", time.Since(start))
	}
	if cp.options.Verbose {
		fmt.Println("\n[3/3] Assembly and Linking...")
	}
	start = time.Now()
	f, err := os.CreateTemp("", "sea_spass_*.o")
	if err != nil {
		return err
	}
	objFile := f.Name()
	f.Close()
	if err := obj.WriteELF(objFile); err != nil {
		os.Remove(objFile)
		return err
	}
	out := cp.singlePassOutput()
	return cp.linkWithGCC([]string{"-no-pie", objFile, "-o", out}, out, start, objFile)
}

// singlePassOutput resolves the -o target (mirrors runCompiler default).
func (cp *CompilerPipeline) singlePassOutput() string {
	if cp.outputFile != "" {
		return cp.outputFile
	}
	if out := os.Getenv("SEA_OUTPUT"); out != "" {
		return out
	}
	return "a.out"
}

func (cp *CompilerPipeline) Compile() error {
	var err error

	if cp.options.Verbose {
		fmt.Println("=== Compilation Pipeline ===")
	}

	if cp.options.SinglePass {
		return cp.compileSinglePass()
	}

	// Phase 0: Preprocessing (if not disabled)
	preprocessedSource, err := cp.runPreprocessor()
	if err != nil {
		return err
	}
	if os.Getenv("SEA_PPONLY") != "" {
		// Debug fast path: stop after preprocessing.
		return nil
	}

	// Phase 1: Parsing
	if err := cp.runParser(preprocessedSource); err != nil {
		return err
	}

	// Phase 2: Instruction Selection
	if cp.options.Verbose {
		fmt.Println("\n[2/5] Instruction Selection...")
	}
	start := time.Now()

	cp.selector = NewInstructionSelector()
	structs, typedefs, enums, funcs := cp.symbolTables()
	cp.selector.structs = structs   // Pass struct definitions FROM PARSER
	cp.selector.typedefs = typedefs // Pass typedef aliases FROM PARSER
	cp.selector.enums = enums       // Pass enum constants FROM PARSER
	cp.selector.functions = funcs
	// funcDefined must be COMPLETE before any body is selected: calls
	// check it to decide extern-vs-sea ABI (isExtern/extFloatRet), so a
	// forward reference (call before the definition is visited) would
	// miscompile if the set only grew in visit order. The selector's own
	// recording covers the parallel pre-pass, but sequential selection
	// visits bodies in order — seed the full set here in one O(toplevel)
	// pass (the old O(#funcs x #toplevel) scan was correct but slow).
	cp.selector.funcDefined = definedFuncSet(cp.ast)

	if cp.options.NoParallelSelect {
		err = cp.selector.SelectInstructions(cp.ast)
	} else {
		err = cp.selector.SelectInstructionsParallel(cp.ast)
	}
	if err != nil {
		return fmt.Errorf("instruction selection error: %w", err)
	}
	cp.ir = cp.selector.instructions

	if cp.options.Verbose {
		fmt.Printf("  Generated %d IR instructions\n", len(cp.ir))
		fmt.Printf("  Completed in %v\n", time.Since(start))
	}

	// The AST is no longer needed: only the global initializer nodes
	// (referenced by sym.Init) survive, so dropping the tree lets the
	// bulk of the parse-phase memory be reclaimed before code emission.
	cp.ast = nil
	runtime.GC()
	// Phase 3: Register Allocation
	if cp.options.Verbose {
		fmt.Println("\n[3/5] Register Allocation...")
	}
	start = time.Now()

	// Linear scan register allocation (TCC-style: fast, simple, no
	// expensive graph coloring). Graph coloring was removed because
	// it caused O(V^2) hangs on large inputs and is unnecessary for
	// a non-optimizing compiler.
	lsAlloc := NewLinearScanAllocator(cp.ir)
	lsAlloc.PreSizeTemps(cp.selector.MaxTemp())
	err = lsAlloc.Allocate()
	if err != nil {
		return fmt.Errorf("register allocation error: %w", err)
	}
	cp.usedRegs = lsAlloc.GetUsedRegisters()
	if cp.options.Verbose {
		fmt.Printf("  Used %d registers\n", len(cp.usedRegs))
	}

	if cp.options.Verbose {
		fmt.Printf("  Completed in %v\n", time.Since(start))
	}

	// Phase 4: Code Emission
	if cp.options.Verbose {
		fmt.Println("\n[4/5] Code Emission...")
	}
	start = time.Now()

	cp.emitter = NewCodeEmitter(cp.ir, cp.selector.stringLits, cp.selector.globalVars, cp.selector.structs, cp.selector.funcIsLocal)
	cp.emitter.usedRegisters = cp.usedRegs
	if cp.options.NoParallelEmit {
		cp.assembly = cp.emitter.Emit()
	} else {
		cp.assembly = cp.emitter.EmitParallel()
	}

	if cp.options.Verbose {
		fmt.Printf("  Generated %d lines of assembly\n", countLines(cp.assembly))
		fmt.Printf("  Completed in %v\n", time.Since(start))
	}

	return nil
}

func (cp *CompilerPipeline) GetAssembly() string {
	return cp.assembly
}

// assembleFast assembles cp.assembly in-process (no GNU as subprocess) and
// writes an ELF relocatable object, returning its path. Any error triggers
// the caller's fallback to the GNU as path. Uses a unique temp file so
// concurrent compiler invocations never collide.
func (cp *CompilerPipeline) assembleFast() (string, error) {
	obj, err := AssembleObject(cp.assembly)
	if err != nil {
		return "", err
	}
	// The assembly text (tens of MB) is dead after this: release it before
	// the fork/exec of the linker to keep peak RSS down.
	cp.assembly = ""
	f, err := os.CreateTemp("", "sea_output_*.o")
	if err != nil {
		return "", err
	}
	objFile := f.Name()
	f.Close()
	if err := obj.WriteELF(objFile); err != nil {
		os.Remove(objFile)
		return "", err
	}
	return objFile, nil
}

func (cp *CompilerPipeline) WriteAssembly(filename string) error {
	return os.WriteFile(filename, []byte(cp.assembly), 0644)
}

func (cp *CompilerPipeline) AssembleAndLink(outputBinary string) error {
	if cp.options.Verbose {
		fmt.Println("\n[5/5] Assembly and Linking...")
	}
	start := time.Now()

	// Assemble and link with GCC
	// Fast path (default): assemble the generated text in-process and hand
	// gcc an object file, skipping the 49MB assembly round-trip through GNU
	// as. Falls back to the .s + GNU as path on any error.
	inputFile := ""
	fastObj := "" // non-empty when inputFile is our temp .o (remove after link)
	if cp.options.UseFastAsm {
		fastStart := time.Now()
		if objFile, err := cp.assembleFast(); err == nil {
			inputFile, fastObj = objFile, objFile
			if cp.options.Verbose {
				fmt.Printf("  In-process assemble: %v (%s)\n", time.Since(fastStart), objFile)
			}
		} else if cp.options.Verbose {
			fmt.Printf("  fast assembler failed (%v), falling back to GNU as\n", err)
		}
	}
	if inputFile == "" {
		// Write assembly to temp file
		asmFile := "/tmp/compiler_output.s"
		err := cp.WriteAssembly(asmFile)
		if err != nil {
			return fmt.Errorf("failed to write assembly: %w", err)
		}
		inputFile = asmFile
	}
	gccArgs := []string{"-no-pie", inputFile, "-o", outputBinary}
	return cp.linkWithGCC(gccArgs, outputBinary, start, fastObj)
}

// linkWithGCC runs the gcc link step shared by AssembleAndLink and the
// single-pass backend. fastObj names a temp .o to remove on success (or ""
// to keep everything).
func (cp *CompilerPipeline) linkWithGCC(gccArgs []string, outputBinary string, start time.Time, fastObj string) error {
	// tcc philosophy: undefined symbols are a link-time non-event (they
	// only matter if actually called at runtime; tcc binaries resolve
	// lazily too). Bell's box2d/box3d archives reference assert hooks
	// the app never defines, and tcc links them fine.
	gccArgs = append(gccArgs, "-Wl,--unresolved-symbols=ignore-all")

	// Add Raylib library flags. The default clibs/raylib is only used
	// when the caller did not supply their own raylib archive (e.g. bell
	// passes ../ahoy/repos/raylib/src/libraylib.a, whose rmodels.o differs
	// from the stale clibs one and avoids its strict face-count assert).
	hasRaylibLib := false
	for _, f := range cp.options.LibraryFlags {
		if strings.Contains(f, "raylib") {
			hasRaylibLib = true
			break
		}
	}
	if !hasRaylibLib {
		gccArgs = append(gccArgs,
			"-L/home/lee/Documents/clibs/raylib/src",
			"-lraylib",
		)
	}
	gccArgs = append(gccArgs, "-lm", "-lpthread", "-ldl", "-lrt")

	// Add any additional library flags from options
	if len(cp.options.LibraryFlags) > 0 {
		gccArgs = append(gccArgs, cp.options.LibraryFlags...)
	}

	cmd := exec.Command("gcc", gccArgs...)
	output, err := cmd.CombinedOutput()
	// The temp .o did its job: drop it on success (keep it next to the
	// failed-output .s dump when linking fails, for debugging).
	if fastObj != "" && err == nil {
		os.Remove(fastObj)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "GCC output: %s\n", output)
		return fmt.Errorf("assembly/linking failed: %w", err)
	}

	if cp.options.Verbose {
		fmt.Printf("  Output: %s\n", outputBinary)
		fmt.Printf("  Completed in %v\n", time.Since(start))
	}

	return nil
}

func (cp *CompilerPipeline) AssembleAndLinkNative(outputBinary string) error {
	if cp.options.Verbose {
		fmt.Println("\n[5/5] Native Assembly and Linking...")
	}
	start := time.Now()

	// Use already-generated assembly text
	asmText := cp.assembly

	if cp.options.Verbose {
		fmt.Printf("  Assembling %d bytes of code\n", len(asmText))
	}

	// Write assembly to temp file
	asmFile := "/tmp/native_output.s"
	err := os.WriteFile(asmFile, []byte(asmText), 0644)
	if err != nil {
		// Fall back to old method if file write fails
		return fmt.Errorf("failed to write assembly: %w", err)
	}

	// Use GCC to assemble and link with Raylib
	gccArgs := []string{
		"-no-pie",
		asmFile,
		"-o", outputBinary,
		"-L/home/lee/Documents/clibs/raylib/src",
		"-lraylib",
		"-lm",
		"-lpthread",
		"-ldl",
		"-lrt",
	}

	cmd := exec.Command("gcc", gccArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "GCC output: %s\n", output)
		// Save assembly for debugging
		os.WriteFile("/tmp/failed_native.s", []byte(asmText), 0644)
		fmt.Fprintf(os.Stderr, "Assembly saved to: /tmp/failed_native.s\n")
		return fmt.Errorf("native assembly/linking failed: %w", err)
	}

	if cp.options.Verbose {
		fmt.Printf("  Output: %s\n", outputBinary)
		fi, _ := os.Stat(outputBinary)
		if fi != nil {
			fmt.Printf("  Size: %d bytes\n", fi.Size())
		}
		fmt.Printf("  Completed in %v\n", time.Since(start))
	}

	return nil
}

func countLines(s string) int {
	count := 0
	for _, c := range s {
		if c == '\n' {
			count++
		}
	}
	return count
}

// CLI entry point
func runCompiler() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: ccompiler <source.c> [options]")
		fmt.Println("\nOptions:")
		fmt.Println("  -run          Compile and run immediately")
		fmt.Println("  -v            Verbose output")
		fmt.Println("  -O<level>     Optimization level (0-3, default 0 = no optimizing passes)")
		fmt.Println("  -fno-fast-asm Disable the built-in assembler (fall back to GNU as)")
		fmt.Println("  -fno-parallel-select Disable parallel instruction selection (sequential fallback)")
		fmt.Println("  -fno-parallel-emit Disable parallel code emission (sequential fallback)")
		fmt.Println("  -single-pass      Experimental single-pass backend (AST straight to machine code)")
		fmt.Println("  -o <file>     Output file (default: a.out)")
		fmt.Println("  -S            Output assembly only")
		fmt.Println("  -l<lib>       Link with library (e.g., -lc, -lraylib)")
		fmt.Println("  -linear-scan  Use linear scan register allocation")
		fmt.Println("  -native       Use built-in assembler/linker (faster!)")
		os.Exit(1)
	}

	sourceFile := os.Args[1]

	// Parse options
	options := CompilerOptions{
		OptimizationLevel: 0,
		DebugInfo:         false,
		Verbose:           false,
		UseLinearScan:     false,
		UseNativeBackend:  false,
		LibraryFlags:      []string{},
		UseFastAsm:        true,
	}

	runMode := false
	asmOnly := false
	outputFile := "a.out"

	for i := 2; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch {
		case arg == "-run":
			runMode = true
		case arg == "-v":
			options.Verbose = true
		case arg == "-S":
			asmOnly = true
		case arg == "-linear-scan":
			options.UseLinearScan = true
		case arg == "-native":
			options.UseNativeBackend = true
		case arg == "-fno-fast-asm":
			// Opt out of the in-process assembler; use GNU as.
			options.UseFastAsm = false
		case arg == "-fno-parallel-select":
			// Opt out of parallel instruction selection; use the
			// sequential fallback.
			options.NoParallelSelect = true
		case arg == "-fno-parallel-emit":
			// Opt out of parallel code emission; use the sequential
			// fallback.
			options.NoParallelEmit = true
		case arg == "-single-pass":
			// Experimental TCC-style backend: AST straight to machine
			// code, no IR / regalloc / assembly text. -O0 spills at
			// every statement; -O1 enables short jumps.
			options.SinglePass = true
		case arg == "-o":
			if i+1 < len(os.Args) {
				outputFile = os.Args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-l"):
			// Library flag: -lc, -lraylib, etc.
			options.LibraryFlags = append(options.LibraryFlags, arg)
		case strings.HasPrefix(arg, "-L"):
			// Library search path: forward to the link.
			options.LibraryFlags = append(options.LibraryFlags, arg)
		case strings.HasSuffix(arg, ".a") || strings.HasSuffix(arg, ".o") || strings.HasSuffix(arg, ".so"):
			// Bare library/object paths: forward to the link (link
			// order puts them after our object, like gcc).
			options.LibraryFlags = append(options.LibraryFlags, arg)
		case strings.HasPrefix(arg, "-Wl,"):
			// Linker passthrough: forward verbatim (e.g.
			// -Wl,--unresolved-symbols=ignore-all).
			options.LibraryFlags = append(options.LibraryFlags, arg)
		case arg == "-I":
			if i+1 < len(os.Args) {
				options.IncludePaths = append(options.IncludePaths, os.Args[i+1])
				i++
			}
		case strings.HasPrefix(arg, "-I"):
			options.IncludePaths = append(options.IncludePaths, arg[2:])
		case arg == "-D":
			if i+1 < len(os.Args) {
				options.Defines = append(options.Defines, os.Args[i+1])
				i++
			}
		case strings.HasPrefix(arg, "-D"):
			// -DNAME or -DNAME=value (preprocessor define).
			options.Defines = append(options.Defines, arg[2:])
		case arg == "-O0":
			options.OptimizationLevel = 0
		case arg == "-O1":
			options.OptimizationLevel = 1
		case arg == "-O2":
			options.OptimizationLevel = 2
		case arg == "-O3":
			options.OptimizationLevel = 3
		}
	}

	startTime := time.Now()

	// Read source file
	source, err := os.ReadFile(sourceFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading file: %v\n", err)
		os.Exit(1)
	}

	// Create compiler
	compiler := NewCompilerPipeline(string(source), options)
	compiler.sourceFile = sourceFile
	compiler.outputFile = outputFile

	// Compile
	err = compiler.Compile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Compilation error: %v\n", err)
		os.Exit(1)
	}

	compileTime := time.Since(startTime)

	if asmOnly {
		// Output assembly only
		asmFile := outputFile
		if asmFile == "a.out" {
			asmFile = "output.s"
		}

		err = compiler.WriteAssembly(asmFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error writing assembly: %v\n", err)
			os.Exit(1)
		}

		if !runMode {
			fmt.Printf("✓ Assembly generated: %s\n", asmFile)
			fmt.Printf("  Time: %v\n", compileTime)
		}
		return
	}

	// Assemble and link (the single-pass backend already linked inside
	// Compile; -S dumps nothing for it yet — spass has no text form).
	if !options.SinglePass {
		if options.UseNativeBackend {
			err = compiler.AssembleAndLinkNative(outputFile)
		} else {
			err = compiler.AssembleAndLink(outputFile)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)

		// Save assembly for debugging
		asmFile := "/tmp/failed_output.s"
		compiler.WriteAssembly(asmFile)
		fmt.Fprintf(os.Stderr, "Assembly saved to: %s\n", asmFile)
		os.Exit(1)
	}

	totalTime := time.Since(startTime)

	if !runMode && !options.Verbose {
		fmt.Printf("\n✓ Compilation successful!\n")
		fmt.Printf("  Time: %v\n", totalTime)
		fmt.Printf("  Output: %s\n", outputFile)
	}

	// Run if requested
	if runMode {
		if options.Verbose {
			fmt.Println("\n=== Running Program ===")
		}

		cmd := exec.Command("./" + outputFile)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin

		err := cmd.Run()

		if err != nil {
			if options.Verbose {
				fmt.Printf("\n=== Program Crashed ===\n")
			}
			if exitErr, ok := err.(*exec.ExitError); ok {
				if options.Verbose {
					fmt.Fprintf(os.Stderr, "Exit code: %d\n", exitErr.ExitCode())
				}
				os.Exit(exitErr.ExitCode())
			}
			fmt.Fprintf(os.Stderr, "\nProgram error: %v\n", err)
			os.Exit(1)
		}

		if options.Verbose {
			fmt.Printf("\n=== Program Finished Successfully ===\n")
		}

		if options.Verbose {
			fmt.Printf("Total time: %v\n", totalTime)
		} else {
			fmt.Printf("\n[Compiled and ran in %v]\n", totalTime)
		}
	}
}
