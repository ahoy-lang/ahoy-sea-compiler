package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type CompilerPipeline struct {
	source   string
	ast      *ASTNode
	ir       []*IRInstruction
	assembly string
	
	parser     *Parser
	selector   *InstructionSelector
	allocator  *RegisterAllocator
	emitter    *CodeEmitter
	
	options CompilerOptions
}

type CompilerOptions struct {
	OptimizationLevel int
	DebugInfo         bool
	Verbose           bool
	UseLinearScan     bool
	UseNativeBackend  bool
	NoPreprocess      bool // Skip preprocessing
	LibraryFlags      []string // Additional library flags like -lc, -lraylib
}

func NewCompilerPipeline(source string, options CompilerOptions) *CompilerPipeline {
	return &CompilerPipeline{
		source:  source,
		options: options,
	}
}

func (cp *CompilerPipeline) Compile() error {
	var err error
	
	if cp.options.Verbose {
		fmt.Println("=== Compilation Pipeline ===")
	}
	
	// Phase 0: Preprocessing (if not disabled)
	preprocessedSource := cp.source
	preprocessor := NewPreprocessor()
	
	if !cp.options.NoPreprocess {
		if cp.options.Verbose {
			fmt.Println("\n[0/5] Preprocessing...")
		}
		start := time.Now()
		
		// Extract types from common raylib headers if they exist
		raylibHeaders := []string{
			"/home/lee/Documents/clibs/raylib/src/raylib.h",
			"/home/lee/Documents/clibs/raylib/src/raymath.h",
		}
		
		for _, header := range raylibHeaders {
			if _, err := os.Stat(header); err == nil {
				preprocessor.ExtractTypesFromHeader(header)
			}
		}
		
		preprocessedSource, err = preprocessor.Process(cp.source)
		if err != nil {
			return fmt.Errorf("preprocessing error: %w", err)
		}
		
		if cp.options.Verbose {
			fmt.Printf("  Completed in %v\n", time.Since(start))
		}
	}
	
	// Phase 1: Parsing
	if cp.options.Verbose {
		fmt.Println("\n[1/5] Parsing...")
	}
	start := time.Now()
	
	cp.parser = NewParser(preprocessedSource)
	// Pass extracted struct types to parser
	for name, structDef := range preprocessor.structMap {
		cp.parser.structs[name] = structDef
		// Also add typedef alias
		cp.parser.typedefs[name] = name
	}
	cp.ast, err = cp.parser.Parse()
	if err != nil {
		return fmt.Errorf("parse error: %w", err)
	}
	
	if cp.options.Verbose {
		fmt.Printf("  Completed in %v\n", time.Since(start))
	}
	
	// Phase 2: Instruction Selection
	if cp.options.Verbose {
		fmt.Println("\n[2/5] Instruction Selection...")
	}
	start = time.Now()
	
	cp.selector = NewInstructionSelector()
	cp.selector.structs = cp.parser.structs  // Pass struct definitions
	err = cp.selector.SelectInstructions(cp.ast)
	if err != nil {
		return fmt.Errorf("instruction selection error: %w", err)
	}
	cp.ir = cp.selector.instructions
	
	if cp.options.Verbose {
		fmt.Printf("  Generated %d IR instructions\n", len(cp.ir))
		fmt.Printf("  Completed in %v\n", time.Since(start))
	}
	
	// Phase 3: Register Allocation
	if cp.options.Verbose {
		fmt.Println("\n[3/5] Register Allocation...")
	}
	start = time.Now()
	
	if cp.options.UseLinearScan {
		lsAlloc := NewLinearScanAllocator(cp.ir)
		err = lsAlloc.Allocate()
		if err != nil {
			return fmt.Errorf("register allocation error: %w", err)
		}
	} else {
		cp.allocator = NewRegisterAllocator(cp.ir)
		err = cp.allocator.Allocate()
		if err != nil {
			return fmt.Errorf("register allocation error: %w", err)
		}
		
		if cp.options.Verbose {
			usedRegs := cp.allocator.GetUsedRegisters()
			spilledVars := cp.allocator.GetSpilledVars()
			fmt.Printf("  Used %d registers\n", len(usedRegs))
			fmt.Printf("  Spilled %d variables\n", len(spilledVars))
		}
	}
	
	if cp.options.Verbose {
		fmt.Printf("  Completed in %v\n", time.Since(start))
	}
	
	// Phase 4: Code Emission
	if cp.options.Verbose {
		fmt.Println("\n[4/5] Code Emission...")
	}
	start = time.Now()
	
	cp.emitter = NewCodeEmitter(cp.ir, cp.selector.stringLits, cp.selector.globalVars)
	cp.assembly = cp.emitter.Emit()
	
	if cp.options.Verbose {
		fmt.Printf("  Generated %d lines of assembly\n", countLines(cp.assembly))
		fmt.Printf("  Completed in %v\n", time.Since(start))
	}
	
	return nil
}

func (cp *CompilerPipeline) GetAssembly() string {
	return cp.assembly
}

func (cp *CompilerPipeline) WriteAssembly(filename string) error {
	return os.WriteFile(filename, []byte(cp.assembly), 0644)
}

func (cp *CompilerPipeline) AssembleAndLink(outputBinary string) error {
	if cp.options.Verbose {
		fmt.Println("\n[5/5] Assembly and Linking...")
	}
	start := time.Now()
	
	// Write assembly to temp file
	asmFile := "/tmp/compiler_output.s"
	err := cp.WriteAssembly(asmFile)
	if err != nil {
		return fmt.Errorf("failed to write assembly: %w", err)
	}
	
	// Assemble and link with GCC
	gccArgs := []string{"-no-pie", asmFile, "-o", outputBinary}
	
	// Add library flags
	if len(cp.options.LibraryFlags) > 0 {
		gccArgs = append(gccArgs, cp.options.LibraryFlags...)
	}
	
	cmd := exec.Command("gcc", gccArgs...)
	output, err := cmd.CombinedOutput()
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
	
	// Add _start stub that calls main and exits
	startStub := `    .globl _start
_start:
    xorq %rbp, %rbp
    call main
    movq %rax, %rdi
    movq $60, %rax
    syscall

`
	
	// Insert _start after first .text directive
	fullAsm := asmText
	if strings.Contains(fullAsm, "    .text\n") {
		fullAsm = strings.Replace(fullAsm, "    .text\n", "    .text\n\n"+startStub, 1)
	} else {
		fullAsm = "    .text\n" + startStub + asmText
	}
	
	if cp.options.Verbose {
		fmt.Printf("  Assembling %d bytes of code\n", len(fullAsm))
	}
	
	// Create assembler and generate machine code
	assembler := NewAssembler()
	machineCode, err := assembler.AssembleText(fullAsm)
	if err != nil {
		return fmt.Errorf("machine code generation failed: %w", err)
	}
	
	symbols := assembler.GetSymbols()
	
	// Get data sections
	rodata, data, bssSize := cp.emitter.GetSections()
	
	// Create linker
	linker := NewLinker()
	linker.SetSections(machineCode, rodata, data, bssSize)
	
	// Add all symbols from assembler
	for name, offset := range symbols {
		linker.AddSymbol(name, offset, "text")
	}
	
	// Set entry point to _start
	linker.SetEntryPoint("_start")
	
	// Link to create executable
	executable, err := linker.Link()
	if err != nil {
		return fmt.Errorf("linking failed: %w", err)
	}
	
	// Write executable to file
	err = os.WriteFile(outputBinary, executable, 0755)
	if err != nil {
		return fmt.Errorf("failed to write executable: %w", err)
	}
	
	if cp.options.Verbose {
		fmt.Printf("  Output: %s\n", outputBinary)
		fmt.Printf("  Size: %d bytes\n", len(executable))
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
		fmt.Println("  -O<level>     Optimization level (0-3)")
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
		case arg == "-o":
			if i+1 < len(os.Args) {
				outputFile = os.Args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-l"):
			// Library flag: -lc, -lraylib, etc.
			options.LibraryFlags = append(options.LibraryFlags, arg)
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
	
	// Assemble and link
	if options.UseNativeBackend {
		err = compiler.AssembleAndLinkNative(outputFile)
	} else {
		err = compiler.AssembleAndLink(outputFile)
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
			fmt.Println("\n=== Running Program ===\n")
		}
		
		cmd := exec.Command("./" + outputFile)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		
		err := cmd.Run()
		
		if options.Verbose {
			fmt.Printf("\n=== Program Finished ===\n")
		}
		
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				os.Exit(exitErr.ExitCode())
			}
			fmt.Fprintf(os.Stderr, "\nProgram error: %v\n", err)
			os.Exit(1)
		}
		
		if options.Verbose {
			fmt.Printf("Total time: %v\n", totalTime)
		} else {
			fmt.Printf("\n[Compiled and ran in %v]\n", totalTime)
		}
	}
}
