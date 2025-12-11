package main

import (
	"fmt"
	"strings"
)

// x86-64 registers
const (
	RAX = iota
	RBX
	RCX
	RDX
	RSI
	RDI
	RBP
	RSP
	R8
	R9
	R10
	R11
	R12
	R13
	R14
	R15
)

var regNames = []string{
	"rax", "rbx", "rcx", "rdx", "rsi", "rdi", "rbp", "rsp",
	"r8", "r9", "r10", "r11", "r12", "r13", "r14", "r15",
}

var reg32Names = []string{
	"eax", "ebx", "ecx", "edx", "esi", "edi", "ebp", "esp",
	"r8d", "r9d", "r10d", "r11d", "r12d", "r13d", "r14d", "r15d",
}

var reg8Names = []string{
	"al", "bl", "cl", "dl", "sil", "dil", "bpl", "spl",
	"r8b", "r9b", "r10b", "r11b", "r12b", "r13b", "r14b", "r15b",
}

// Calling convention argument registers
var argRegs = []int{RDI, RSI, RDX, RCX, R8, R9}

type CodeGenerator struct {
	output       strings.Builder
	dataSection  strings.Builder
	bssSection   strings.Builder
	
	labelCounter int
	strCounter   int
	
	// Symbol tables
	globalVars   map[string]*Symbol
	localVars    map[string]*Symbol
	functions    map[string]*Function
	strings      map[string]string
	
	// Current function context
	currentFunc  *Function
	stackOffset  int
	
	// Register allocation
	regUsed      [16]bool
	regStack     []int
}

type Symbol struct {
	Name       string
	Offset     int  // Stack offset for locals
	IsGlobal   bool
	Size       int
	Type       string
	ArraySize  int  // For arrays, 0 if not an array
}

type Function struct {
	Name       string
	Params     []string
	LocalSize  int
	ReturnType string
}

func NewCodeGenerator() *CodeGenerator {
	return &CodeGenerator{
		globalVars: make(map[string]*Symbol),
		localVars:  make(map[string]*Symbol),
		functions:  make(map[string]*Function),
		strings:    make(map[string]string),
	}
}

func (g *CodeGenerator) newLabel() string {
	g.labelCounter++
	return fmt.Sprintf(".L%d", g.labelCounter)
}

func (g *CodeGenerator) newStringLabel() string {
	g.strCounter++
	return fmt.Sprintf(".str%d", g.strCounter)
}

func (g *CodeGenerator) emit(format string, args ...interface{}) {
	g.output.WriteString(fmt.Sprintf(format, args...))
	g.output.WriteString("\n")
}

func (g *CodeGenerator) emitData(format string, args ...interface{}) {
	g.dataSection.WriteString(fmt.Sprintf(format, args...))
	g.dataSection.WriteString("\n")
}

func (g *CodeGenerator) emitBss(format string, args ...interface{}) {
	g.bssSection.WriteString(fmt.Sprintf(format, args...))
	g.bssSection.WriteString("\n")
}

// Allocate a register
func (g *CodeGenerator) allocReg() int {
	// Simple allocation - just find first free register
	for i := RAX; i <= R15; i++ {
		if i == RSP || i == RBP {
			continue // Reserved
		}
		if !g.regUsed[i] {
			g.regUsed[i] = true
			return i
		}
	}
	// If all busy, spill to stack (simplified)
	return RAX
}

func (g *CodeGenerator) freeReg(reg int) {
	g.regUsed[reg] = false
}

// Generate function prologue
func (g *CodeGenerator) genFunctionPrologue(name string, paramCount int) {
	g.emit("    .globl %s", name)
	g.emit("    .type %s, @function", name)
	g.emit("%s:", name)
	g.emit("    pushq %%rbp")
	g.emit("    movq %%rsp, %%rbp")
	
	g.stackOffset = 0
	g.localVars = make(map[string]*Symbol)
	
	// Save parameter registers to stack
	for i := 0; i < paramCount && i < len(argRegs); i++ {
		g.stackOffset -= 8
		g.emit("    movq %%%s, %d(%%rbp)", regNames[argRegs[i]], g.stackOffset)
	}
}

func (g *CodeGenerator) genFunctionEpilogue() {
	g.emit("    movq %%rbp, %%rsp")
	g.emit("    popq %%rbp")
	g.emit("    ret")
}

// Generate return statement
func (g *CodeGenerator) genReturn(hasValue bool) {
	// Value should already be in RAX
	if !hasValue {
		g.emit("    xorq %%rax, %%rax")
	}
	g.genFunctionEpilogue()
}

// Generate function call
func (g *CodeGenerator) genCall(name string, argCount int) {
	// Arguments are already in registers/stack
	// Align stack to 16 bytes
	if argCount > 6 {
		g.emit("    subq $%d, %%rsp", ((argCount-6)*8+15) & ^15)
	}
	
	g.emit("    call %s", name)
	
	if argCount > 6 {
		g.emit("    addq $%d, %%rsp", ((argCount-6)*8+15) & ^15)
	}
	// Result is in RAX
}

// Load constant to register
func (g *CodeGenerator) genLoadImm(reg int, value int64) {
	g.emit("    movq $%d, %%%s", value, regNames[reg])
}

// Binary operations
func (g *CodeGenerator) genAdd(dst, src int) {
	g.emit("    addq %%%s, %%%s", regNames[src], regNames[dst])
}

func (g *CodeGenerator) genSub(dst, src int) {
	g.emit("    subq %%%s, %%%s", regNames[src], regNames[dst])
}

func (g *CodeGenerator) genMul(dst, src int) {
	g.emit("    imulq %%%s, %%%s", regNames[src], regNames[dst])
}

func (g *CodeGenerator) genDiv(dividend, divisor int) {
	// Move dividend to RAX, divisor to another reg
	if dividend != RAX {
		g.emit("    movq %%%s, %%rax", regNames[dividend])
	}
	g.emit("    cqto")  // Sign extend RAX to RDX:RAX
	g.emit("    idivq %%%s", regNames[divisor])
	// Quotient in RAX, remainder in RDX
}

// Comparisons
func (g *CodeGenerator) genCmp(left, right int) {
	g.emit("    cmpq %%%s, %%%s", regNames[right], regNames[left])
}

func (g *CodeGenerator) genSetCC(condition string, dst int) {
	g.emit("    set%s %%%s", condition, reg8Names[dst])
	g.emit("    movzbq %%%s, %%%s", reg8Names[dst], regNames[dst])
}

// Conditional jump
func (g *CodeGenerator) genJump(label string) {
	g.emit("    jmp %s", label)
}

func (g *CodeGenerator) genCondJump(condition string, label string) {
	g.emit("    j%s %s", condition, label)
}

// Load/Store
func (g *CodeGenerator) genLoad(dst int, src string, offset int) {
	if offset == 0 {
		g.emit("    movq (%s), %%%s", src, regNames[dst])
	} else {
		g.emit("    movq %d(%%%s), %%%s", offset, src, regNames[dst])
	}
}

func (g *CodeGenerator) genStore(src int, dst string, offset int) {
	if offset == 0 {
		g.emit("    movq %%%s, (%s)", regNames[src], dst)
	} else {
		g.emit("    movq %%%s, %d(%%%s)", regNames[src], offset, dst)
	}
}

// Variable access
func (g *CodeGenerator) genLoadVar(dst int, varName string) {
	if sym, ok := g.localVars[varName]; ok {
		g.emit("    movq %d(%%rbp), %%%s", sym.Offset, regNames[dst])
	} else if sym, ok := g.globalVars[varName]; ok {
		g.emit("    movq %s(%%rip), %%%s", sym.Name, regNames[dst])
	}
}

func (g *CodeGenerator) genStoreVar(src int, varName string) {
	if sym, ok := g.localVars[varName]; ok {
		g.emit("    movq %%%s, %d(%%rbp)", regNames[src], sym.Offset)
	} else if sym, ok := g.globalVars[varName]; ok {
		g.emit("    movq %%%s, %s(%%rip)", regNames[src], sym.Name)
	}
}

// String literal
func (g *CodeGenerator) genStringLiteral(str string) string {
	label := g.newStringLabel()
	g.strings[label] = str
	g.emitData("%s:", label)
	g.emitData("    .string \"%s\"", str)
	return label
}

// Push/Pop for expression evaluation
func (g *CodeGenerator) genPush(reg int) {
	g.emit("    pushq %%%s", regNames[reg])
}

func (g *CodeGenerator) genPop(reg int) {
	g.emit("    popq %%%s", regNames[reg])
}

// Generate final assembly output
func (g *CodeGenerator) Generate() string {
	var result strings.Builder
	
	// Data section
	result.WriteString("    .data\n")
	result.WriteString(g.dataSection.String())
	result.WriteString("\n")
	
	// BSS section
	if g.bssSection.Len() > 0 {
		result.WriteString("    .bss\n")
		result.WriteString(g.bssSection.String())
		result.WriteString("\n")
	}
	
	// Text section
	result.WriteString("    .text\n")
	result.WriteString(g.output.String())
	
	return result.String()
}

// Allocate local variable
func (g *CodeGenerator) allocLocal(name string, size int) {
	g.stackOffset -= size
	g.localVars[name] = &Symbol{
		Name:   name,
		Offset: g.stackOffset,
		Size:   size,
	}
}

// Declare global variable
func (g *CodeGenerator) declareGlobal(name string, size int) {
	g.globalVars[name] = &Symbol{
		Name:     name,
		IsGlobal: true,
		Size:     size,
	}
	g.emitBss("    .comm %s,%d,%d", name, size, size)
}
