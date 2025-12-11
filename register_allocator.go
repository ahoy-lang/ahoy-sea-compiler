package main

import (
	"fmt"
	"sort"
)

// Register allocator using graph coloring
type RegisterAllocator struct {
	instructions  []*IRInstruction
	liveRanges    map[string]*LiveRange
	interferenceGraph map[string]map[string]bool
	allocation    map[string]int
	spilledVars   map[string]int
	
	availableRegs []int
	usedRegs      map[int]bool
}

type LiveRange struct {
	VarName string
	Start   int
	End     int
	Uses    []int
}

func NewRegisterAllocator(instructions []*IRInstruction) *RegisterAllocator {
	// Available general-purpose registers (excluding RSP, RBP)
	availableRegs := []int{RAX, RBX, RCX, RDX, RSI, RDI, R8, R9, R10, R11, R12, R13, R14, R15}
	
	return &RegisterAllocator{
		instructions:      instructions,
		liveRanges:        make(map[string]*LiveRange),
		interferenceGraph: make(map[string]map[string]bool),
		allocation:        make(map[string]int),
		spilledVars:       make(map[string]int),
		availableRegs:     availableRegs,
		usedRegs:          make(map[int]bool),
	}
}

func (ra *RegisterAllocator) Allocate() error {
	// Step 1: Compute live ranges
	ra.computeLiveRanges()
	
	// Step 2: Build interference graph
	ra.buildInterferenceGraph()
	
	// Step 3: Allocate registers using graph coloring
	ra.colorGraph()
	
	// Step 4: Rewrite instructions with allocated registers
	ra.rewriteInstructions()
	
	return nil
}

func (ra *RegisterAllocator) computeLiveRanges() {
	for i, instr := range ra.instructions {
		// Record use/def for each operand
		operands := []*Operand{instr.Dst, instr.Src1, instr.Src2}
		
		for _, op := range operands {
			if op == nil || (op.Type != "temp" && op.Type != "var") {
				continue
			}
			
			varName := op.Value
			if op.Type == "var" {
				varName = "var_" + op.Value
			}
			
			if _, exists := ra.liveRanges[varName]; !exists {
				ra.liveRanges[varName] = &LiveRange{
					VarName: varName,
					Start:   i,
					End:     i,
					Uses:    []int{i},
				}
			} else {
				lr := ra.liveRanges[varName]
				lr.End = i
				lr.Uses = append(lr.Uses, i)
			}
		}
	}
}

func (ra *RegisterAllocator) buildInterferenceGraph() {
	// Two variables interfere if their live ranges overlap
	for name1, lr1 := range ra.liveRanges {
		if ra.interferenceGraph[name1] == nil {
			ra.interferenceGraph[name1] = make(map[string]bool)
		}
		
		for name2, lr2 := range ra.liveRanges {
			if name1 == name2 {
				continue
			}
			
			// Check if live ranges overlap
			if !(lr1.End < lr2.Start || lr2.End < lr1.Start) {
				ra.interferenceGraph[name1][name2] = true
			}
		}
	}
}

func (ra *RegisterAllocator) colorGraph() {
	// Sort variables by live range length (longer first)
	type varInfo struct {
		name   string
		length int
		degree int
	}
	
	vars := []varInfo{}
	for name, lr := range ra.liveRanges {
		degree := len(ra.interferenceGraph[name])
		vars = append(vars, varInfo{
			name:   name,
			length: lr.End - lr.Start,
			degree: degree,
		})
	}
	
	// Sort by degree (more neighbors first), then by length
	sort.Slice(vars, func(i, j int) bool {
		if vars[i].degree != vars[j].degree {
			return vars[i].degree > vars[j].degree
		}
		return vars[i].length > vars[j].length
	})
	
	// Greedy coloring
	for _, v := range vars {
		ra.allocateRegister(v.name)
	}
}

func (ra *RegisterAllocator) allocateRegister(varName string) {
	// Find available colors (registers)
	usedColors := make(map[int]bool)
	
	// Check what colors neighbors are using
	if neighbors, ok := ra.interferenceGraph[varName]; ok {
		for neighbor := range neighbors {
			if reg, allocated := ra.allocation[neighbor]; allocated {
				usedColors[reg] = true
			}
		}
	}
	
	// Find first available register
	for _, reg := range ra.availableRegs {
		if !usedColors[reg] {
			ra.allocation[varName] = reg
			ra.usedRegs[reg] = true
			return
		}
	}
	
	// No register available - spill to stack
	spillOffset := len(ra.spilledVars) * 8
	ra.spilledVars[varName] = spillOffset
}

func (ra *RegisterAllocator) rewriteInstructions() {
	for _, instr := range ra.instructions {
		ra.rewriteOperand(&instr.Dst)
		ra.rewriteOperand(&instr.Src1)
		ra.rewriteOperand(&instr.Src2)
	}
}

func (ra *RegisterAllocator) rewriteOperand(op **Operand) {
	if op == nil || *op == nil {
		return
	}
	
	operand := *op
	
	if operand.Type == "temp" {
		if reg, ok := ra.allocation[operand.Value]; ok {
			operand.Type = "reg"
			operand.Value = regNames[reg]
		} else if offset, ok := ra.spilledVars[operand.Value]; ok {
			// Spilled to stack
			operand.Type = "mem"
			operand.Offset = -offset
		}
	} else if operand.Type == "var" {
		varKey := "var_" + operand.Value
		if reg, ok := ra.allocation[varKey]; ok {
			operand.Type = "reg"
			operand.Value = regNames[reg]
		}
	}
}

func (ra *RegisterAllocator) GetUsedRegisters() []int {
	regs := []int{}
	for reg := range ra.usedRegs {
		regs = append(regs, reg)
	}
	sort.Ints(regs)
	return regs
}

func (ra *RegisterAllocator) GetSpilledVars() map[string]int {
	return ra.spilledVars
}

// Advanced: Linear scan register allocation (faster alternative)
type LinearScanAllocator struct {
	instructions []*IRInstruction
	intervals    []*Interval
	active       []*Interval
	allocation   map[string]int
	freeRegs     []int
	stackSlots   map[string]int
}

type Interval struct {
	VarName string
	Start   int
	End     int
	Reg     int
}

func NewLinearScanAllocator(instructions []*IRInstruction) *LinearScanAllocator {
	freeRegs := []int{RAX, RBX, RCX, RDX, RSI, RDI, R8, R9, R10, R11, R12, R13, R14, R15}
	
	return &LinearScanAllocator{
		instructions: instructions,
		intervals:    []*Interval{},
		active:       []*Interval{},
		allocation:   make(map[string]int),
		freeRegs:     freeRegs,
		stackSlots:   make(map[string]int),
	}
}

func (lsa *LinearScanAllocator) Allocate() error {
	// Compute intervals
	lsa.computeIntervals()
	
	// Sort intervals by start point
	sort.Slice(lsa.intervals, func(i, j int) bool {
		return lsa.intervals[i].Start < lsa.intervals[j].Start
	})
	
	// Linear scan
	for _, interval := range lsa.intervals {
		lsa.expireOldIntervals(interval)
		
		if len(lsa.freeRegs) == 0 {
			lsa.spillAtInterval(interval)
		} else {
			// Allocate register
			reg := lsa.freeRegs[0]
			lsa.freeRegs = lsa.freeRegs[1:]
			
			interval.Reg = reg
			lsa.allocation[interval.VarName] = reg
			lsa.active = append(lsa.active, interval)
			
			// Keep active sorted by end point
			sort.Slice(lsa.active, func(i, j int) bool {
				return lsa.active[i].End < lsa.active[j].End
			})
		}
	}
	
	// Rewrite instructions
	lsa.rewriteInstructions()
	
	return nil
}

func (lsa *LinearScanAllocator) computeIntervals() {
	varIntervals := make(map[string]*Interval)
	
	for i, instr := range lsa.instructions {
		operands := []*Operand{instr.Dst, instr.Src1, instr.Src2}
		
		for _, op := range operands {
			if op == nil || (op.Type != "temp" && op.Type != "var") {
				continue
			}
			
			varName := op.Value
			if op.Type == "var" {
				varName = "var_" + op.Value
			}
			
			if _, exists := varIntervals[varName]; !exists {
				varIntervals[varName] = &Interval{
					VarName: varName,
					Start:   i,
					End:     i,
					Reg:     -1,
				}
			} else {
				varIntervals[varName].End = i
			}
		}
	}
	
	for _, interval := range varIntervals {
		lsa.intervals = append(lsa.intervals, interval)
	}
}

func (lsa *LinearScanAllocator) expireOldIntervals(interval *Interval) {
	i := 0
	for i < len(lsa.active) {
		if lsa.active[i].End >= interval.Start {
			break
		}
		
		// Free the register
		lsa.freeRegs = append(lsa.freeRegs, lsa.active[i].Reg)
		lsa.active = append(lsa.active[:i], lsa.active[i+1:]...)
	}
}

func (lsa *LinearScanAllocator) spillAtInterval(interval *Interval) {
	// Find interval with furthest end point
	spill := lsa.active[len(lsa.active)-1]
	
	if spill.End > interval.End {
		// Spill the last active interval
		interval.Reg = spill.Reg
		lsa.allocation[interval.VarName] = spill.Reg
		
		offset := len(lsa.stackSlots) * 8
		lsa.stackSlots[spill.VarName] = offset
		delete(lsa.allocation, spill.VarName)
		
		lsa.active[len(lsa.active)-1] = interval
		sort.Slice(lsa.active, func(i, j int) bool {
			return lsa.active[i].End < lsa.active[j].End
		})
	} else {
		// Spill current interval
		offset := len(lsa.stackSlots) * 8
		lsa.stackSlots[interval.VarName] = offset
	}
}

func (lsa *LinearScanAllocator) rewriteInstructions() {
	for _, instr := range lsa.instructions {
		lsa.rewriteOperand(&instr.Dst)
		lsa.rewriteOperand(&instr.Src1)
		lsa.rewriteOperand(&instr.Src2)
	}
}

func (lsa *LinearScanAllocator) rewriteOperand(op **Operand) {
	if op == nil || *op == nil {
		return
	}
	
	operand := *op
	
	if operand.Type == "temp" || operand.Type == "var" {
		varName := operand.Value
		if operand.Type == "var" {
			varName = "var_" + operand.Value
		}
		
		if reg, ok := lsa.allocation[varName]; ok {
			operand.Type = "reg"
			operand.Value = regNames[reg]
		} else if offset, ok := lsa.stackSlots[varName]; ok {
			operand.Type = "mem"
			operand.Offset = -offset
		}
	}
}

// Register pressure analysis
func analyzeRegisterPressure(instructions []*IRInstruction) map[int]int {
	pressure := make(map[int]int)
	liveVars := make(map[string]bool)
	
	for i, instr := range instructions {
		// Kill defined variables
		if instr.Dst != nil && (instr.Dst.Type == "temp" || instr.Dst.Type == "var") {
			delete(liveVars, instr.Dst.Value)
		}
		
		// Add used variables
		if instr.Src1 != nil && (instr.Src1.Type == "temp" || instr.Src1.Type == "var") {
			liveVars[instr.Src1.Value] = true
		}
		if instr.Src2 != nil && (instr.Src2.Type == "temp" || instr.Src2.Type == "var") {
			liveVars[instr.Src2.Value] = true
		}
		
		pressure[i] = len(liveVars)
	}
	
	return pressure
}

// Simple register allocation for debugging
type SimpleAllocator struct {
	nextReg int
	regMap  map[string]int
}

func NewSimpleAllocator() *SimpleAllocator {
	return &SimpleAllocator{
		nextReg: RAX,
		regMap:  make(map[string]int),
	}
}

func (sa *SimpleAllocator) AllocateForVar(varName string) int {
	if reg, ok := sa.regMap[varName]; ok {
		return reg
	}
	
	// Skip RSP and RBP
	for sa.nextReg == RSP || sa.nextReg == RBP {
		sa.nextReg++
		if sa.nextReg > R15 {
			sa.nextReg = RAX
		}
	}
	
	reg := sa.nextReg
	sa.regMap[varName] = reg
	
	sa.nextReg++
	if sa.nextReg > R15 {
		sa.nextReg = RAX
	}
	
	return reg
}

func (sa *SimpleAllocator) GetRegister(varName string) (int, bool) {
	reg, ok := sa.regMap[varName]
	return reg, ok
}

func (sa *SimpleAllocator) Reset() {
	sa.nextReg = RAX
	sa.regMap = make(map[string]int)
}

func printLiveRanges(liveRanges map[string]*LiveRange) {
	fmt.Println("\n=== Live Ranges ===")
	for name, lr := range liveRanges {
		fmt.Printf("%s: [%d, %d] uses: %v\n", name, lr.Start, lr.End, lr.Uses)
	}
}

func printInterferenceGraph(graph map[string]map[string]bool) {
	fmt.Println("\n=== Interference Graph ===")
	for node, neighbors := range graph {
		fmt.Printf("%s interferes with: ", node)
		for neighbor := range neighbors {
			fmt.Printf("%s ", neighbor)
		}
		fmt.Println()
	}
}

func printAllocation(allocation map[string]int) {
	fmt.Println("\n=== Register Allocation ===")
	for varName, reg := range allocation {
		fmt.Printf("%s -> %s\n", varName, regNames[reg])
	}
}
