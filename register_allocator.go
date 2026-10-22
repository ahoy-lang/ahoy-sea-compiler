package main

import (
	"fmt"
	"sort"
)

// Scratch registers reserved for the emitter (never allocated to temps):
// RAX (return value, universal scratch), RDX (array bases, div/mod high
// half), R11 (index/pointer scratch). Letting temps live in these meant
// every emitter scratch sequence could silently kill them (the a[0]-b[0]
// bug: two live temps collapsed into one register). 10 GPRs remain, plus
// (now working) spills.
var scratchRegs = map[int]bool{RAX: true, RDX: true, R11: true}

// frameExtent scans instructions for the deepest frame offset in use so
// spilled temps can live below it (never overlapping locals). Without
// this, spill slot 0 landed on the saved-rbp slot and corrupted frames.
func frameExtent(instructions []*IRInstruction) int {
	maxOff := 0
	for _, instr := range instructions {
		// Unrolled over the three operands; IndexTemp chains are
		// shallow (one level in practice), so walk them iteratively
		// instead of via a per-instruction closure.
		for k := 0; k < 3; k++ {
			var op *Operand
			switch k {
			case 0:
				op = instr.Dst
			case 1:
				op = instr.Src1
			default:
				op = instr.Src2
			}
			for op != nil {
				if (op.Type == "var" || op.Type == "mem" || op.Type == "array") && op.Offset < 0 && !op.IsGlobal {
					if -op.Offset > maxOff {
						maxOff = -op.Offset
					}
				}
				op = op.IndexTemp
			}
		}
	}
	return maxOff
}

// Register allocator using graph coloring
type RegisterAllocator struct {
	instructions  []*IRInstruction
	liveRanges    map[string]*LiveRange
	interferenceGraph map[string]map[string]bool
	allocation    map[string]int
	spilledVars   map[string]int
	frameBase     int
	
	availableRegs []int
	usedRegs      map[int]bool
}

type LiveRange struct {
	VarName string
	Start   int
	End     int
	Uses    []int
}

// instrTemps flattens an instruction's operands including nested IndexTemp
// pointers (e.g. ptr->member address temps, array indices). A temp used only
// inside a ptr/array operand is still live at this instruction — skipping it
// let two live temps share one register (segfaults). No opts, just correct.
func instrTemps(instr *IRInstruction) []*Operand {
	return instrTempsInto(instr, nil)
}

// instrTempsInto is the allocation-free variant: operands are appended to
// buf (caller passes a reused scratch, e.g. tbuf[:0]). computeIntervals
// calls this 1.6M times; a fresh slice per call was ~half its allocs.
func instrTempsInto(instr *IRInstruction, buf []*Operand) []*Operand {
	walkTemps(instr.Dst, &buf)
	walkTemps(instr.Src1, &buf)
	walkTemps(instr.Src2, &buf)
	return buf
}

func walkTemps(op *Operand, buf *[]*Operand) {
	if op == nil {
		return
	}
	if op.Type == "temp" {
		*buf = append(*buf, op)
	}
	if op.IndexTemp != nil {
		walkTemps(op.IndexTemp, buf)
	}
}

func NewRegisterAllocator(instructions []*IRInstruction) *RegisterAllocator {
	// Available general-purpose registers (excluding RSP, RBP)
	availableRegs := []int{}
	for _, r := range []int{RAX, RBX, RCX, RDX, RSI, RDI, R8, R9, R10, R11, R12, R13, R14, R15} {
		if !scratchRegs[r] {
			availableRegs = append(availableRegs, r)
		}
	}

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
	// Spill space lives below the frame; measure the frame first.
	ra.frameBase = frameExtent(ra.instructions)
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
		// Record use/def for each operand (including nested pointer/index temps)
		operands := instrTemps(instr)

		for _, op := range operands {
			// Only compute live ranges for temporaries, not variables
			// Variables stay on the stack and don't need register allocation
		if op == nil || op.Type != "temp" {
			continue
		}
			
			varName := op.Value
			
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
	// Initialize graph
	for name := range ra.liveRanges {
		ra.interferenceGraph[name] = make(map[string]bool)
	}

	// Sweep-line construction: O(n * k) where k is average live-set
	// size, instead of O(V^2). Two temps interfere iff their live
	// ranges (inclusive [Start,End]) overlap.
	type entry struct {
		name string
		lr   *LiveRange
	}
	sorted := make([]entry, 0, len(ra.liveRanges))
	for name, lr := range ra.liveRanges {
		sorted = append(sorted, entry{name, lr})
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].lr.Start != sorted[j].lr.Start {
			return sorted[i].lr.Start < sorted[j].lr.Start
		}
		return sorted[i].lr.End < sorted[j].lr.End
	})

	active := make([]entry, 0, 64)
	i := 0
	n := len(sorted)
	for i < n {
		curStart := sorted[i].lr.Start
		// Drop temps whose range ended before curStart.
		kept := active[:0]
		for _, a := range active {
			if a.lr.End >= curStart {
				kept = append(kept, a)
			}
		}
		active = kept

		// Collect all temps starting at curStart.
		batch := []entry{}
		for i < n && sorted[i].lr.Start == curStart {
			batch = append(batch, sorted[i])
			i++
		}
		// Each new temp interferes with every currently-live temp.
		for _, b := range batch {
			for _, a := range active {
				if a.name != b.name {
					ra.interferenceGraph[b.name][a.name] = true
					ra.interferenceGraph[a.name][b.name] = true
				}
			}
		}
		// Temps starting together interfere with each other.
		for x := 0; x < len(batch); x++ {
			for y := x + 1; y < len(batch); y++ {
				ra.interferenceGraph[batch[x].name][batch[y].name] = true
				ra.interferenceGraph[batch[y].name][batch[x].name] = true
			}
		}
		active = append(active, batch...)
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
	
	// No register available - spill below the frame (never overlapping).
	spillOffset := ra.frameBase + (len(ra.spilledVars)+1)*8
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
		// Variables are always on the stack - don't allocate to registers
		// Keep as "var" type for code emitter to handle
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

// argRegBusyRange records that a SysV call-argument register (rdi/rsi/...)
// holds a live argument value from instruction Start through the closing
// call at End. Temps must never be allocated to that register while their
// live range overlaps the window: the float-arg staging temps used to land
// in %rdi/%rsi and clobber earlier integer arguments (stbtt_Rasterize got
// a 1.0f bit pattern as its vertices pointer). OpSetArg writes these
// registers directly and bypasses the allocator, so the allocator has to
// model them or it hands them out twice.
type argRegBusyRange struct {
	reg   int
	start int
	end   int
}

// Advanced: Linear scan register allocation (faster alternative)
type LinearScanAllocator struct {
	instructions []*IRInstruction
	intervals    []*Interval
	active       []*Interval
	slots        []tempSlot // indexed by temp id (tN -> N); replaces
	nSpills      int        // distinct spilled temps (for slot offsets)
	freeRegs     []int
	frameBase    int
	argBusy      []argRegBusyRange // pending call-argument register windows
}

// tempSlot is one temp's allocator state. Temps are always named tN with
// dense N (single atomic counter), so id-indexed slots replace the three
// name-keyed maps (varIntervals/allocation/stackSlots) from the hot path:
// a manual integer parse + slice index is far cheaper than a string hash.
type tempSlot struct {
	iv    *Interval // interval under construction (computeIntervals)
	reg   int32     // allocated register, -1 = none
	spill int32     // spill offset, 0 = none
}

// tempID parses a "tN" temp name to its numeric id.
func tempID(name string) (int, bool) {
	if len(name) < 2 || name[0] != 't' {
		return 0, false
	}
	id := 0
	for i := 1; i < len(name); i++ {
		c := name[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		id = id*10 + int(c-'0')
	}
	return id, true
}

// PreSizeTemps sizes slots (and intervals capacity) for temp ids up to
// maxID in one allocation, so the computeIntervals/Allocate walks never
// regrow: growTemp's doubling make+copy+init cost ~34MB transient on the
// editor TU. Temp ids come from one dense atomic counter (see newTemp),
// so the selector's MaxTemp bounds every id. Callers that skip this still
// work via growTemp.
func (lsa *LinearScanAllocator) PreSizeTemps(maxID int) {
	need := maxID + 64
	if need <= len(lsa.slots) {
		return
	}
	ns := make([]tempSlot, need)
	copy(ns, lsa.slots)
	for i := len(lsa.slots); i < need; i++ {
		ns[i].reg = -1
	}
	lsa.slots = ns
	if cap(lsa.intervals) < need {
		ni := make([]*Interval, len(lsa.intervals), need)
		copy(ni, lsa.intervals)
		lsa.intervals = ni
	}
}

// growTemp ensures slots covers id, preserving existing entries.
func (lsa *LinearScanAllocator) growTemp(id int) {
	if id < len(lsa.slots) {
		return
	}
	n := len(lsa.slots)*2 + 64
	if n <= id {
		n = id + 64
	}
	ns := make([]tempSlot, n)
	copy(ns, lsa.slots)
	for i := len(lsa.slots); i < n; i++ {
		ns[i].reg = -1
	}
	lsa.slots = ns
}

type Interval struct {
	VarName   string
	Start     int
	End       int
	Reg       int
	Forbidden uint16 // bit i set = register i is a pending call arg here
}

func NewLinearScanAllocator(instructions []*IRInstruction) *LinearScanAllocator {
	freeRegs := []int{}
	for _, r := range []int{RAX, RBX, RCX, RDX, RSI, RDI, R8, R9, R10, R11, R12, R13, R14, R15} {
		if !scratchRegs[r] {
			freeRegs = append(freeRegs, r)
		}
	}

	n := len(instructions)
	// slots starts empty; growTemp initializes reg=-1 on expansion.
	return &LinearScanAllocator{
		instructions: instructions,
		intervals:    make([]*Interval, 0, n/4),
		active:       []*Interval{},
		slots:        make([]tempSlot, 0, n/4),
		freeRegs:     freeRegs,
	}
}

// intervalByStart sorts intervals by Start without sort.Slice's per-call
// reflection overhead (~500K intervals on the editor TU).
type intervalByStart []*Interval

func (s intervalByStart) Len() int           { return len(s) }
func (s intervalByStart) Less(i, j int) bool { return s[i].Start < s[j].Start }
func (s intervalByStart) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

func (lsa *LinearScanAllocator) Allocate() error {
	// Compute intervals (frame extent is measured in the same walk).
	lsa.computeIntervals()
	
	// Sort intervals by start point
	sort.Sort(intervalByStart(lsa.intervals))

	// Fixed argument registers live from their OpSetArg until the call;
	// temps overlapping a window must not be colored into that register.
	lsa.collectArgRegBusy()
	lsa.computeForbiddenMasks()
	// Emitter sequences (calls, shift count staging, imm materialization)
	// write allocatable registers outside the allocator's model.
	lsa.computeEmitterClobberMasks()
	
	// Linear scan
	for _, interval := range lsa.intervals {
		lsa.expireOldIntervals(interval)
		
		// Pick the first free register that is not a pending call
		// argument register for this interval.
		pick := -1
		for fi, r := range lsa.freeRegs {
			if interval.Forbidden&(1<<uint(r)) == 0 {
				pick = fi
				break
			}
		}
		if pick < 0 {
			lsa.spillAtInterval(interval)
		} else {
			// Allocate register
			reg := lsa.freeRegs[pick]
			lsa.freeRegs = append(lsa.freeRegs[:pick], lsa.freeRegs[pick+1:]...)

			interval.Reg = reg
			if id, ok := tempID(interval.VarName); ok {
				lsa.growTemp(id)
				lsa.slots[id].reg = int32(reg)
			}
			lsa.insertActive(interval)
		}
	}
	
	// Rewrite instructions
	lsa.rewriteInstructions()

	return nil
}

// collectArgRegBusy scans for call-setup windows: every write into a SysV
// integer argument register (OpSetArg / OpMov / OpLoadAddr with a reg dst)
// opens a window that closes at the next OpCall. The written registers are
// live (pending argument values) throughout the window.
func (lsa *LinearScanAllocator) collectArgRegBusy() {
	firstAt := make(map[int]int, 6)
	// Roughly one busy range per call-argument register per call.
	if cap(lsa.argBusy) == 0 {
		lsa.argBusy = make([]argRegBusyRange, 0, len(lsa.instructions)/64)
	}
	flush := func(end int) {
		for reg, start := range firstAt {
			lsa.argBusy = append(lsa.argBusy, argRegBusyRange{reg: reg, start: start, end: end})
		}
		// Reuse the map across calls (was: fresh map per OpCall, ~10K
		// allocs on the editor TU).
		clear(firstAt)
	}

	for i, instr := range lsa.instructions {
		if instr.Dst != nil && instr.Dst.Type == "reg" {
			if r, ok := argRegNameToReg(instr.Dst.Value); ok {
				if _, seen := firstAt[r]; !seen {
					firstAt[r] = i
				}
			}
		}
		if instr.Op == OpCall {
			flush(i)
		}
	}
	flush(len(lsa.instructions) - 1)
}

// argRegNameToReg maps a SysV integer argument register name to its number
// without a per-call map lookup (collectArgRegBusy runs per instruction).
func argRegNameToReg(name string) (int, bool) {
	switch name {
	case "rdi":
		return RDI, true
	case "rsi":
		return RSI, true
	case "rdx":
		return RDX, true
	case "rcx":
		return RCX, true
	case "r8":
		return R8, true
	case "r9":
		return R9, true
	}
	return 0, false
}

// computeForbiddenMasks marks each interval with the argument registers
// whose busy windows overlap it. Ranges are merged per register first, then
// found by a linear two-pointer sweep (intervals are sorted by Start).
func (lsa *LinearScanAllocator) computeForbiddenMasks() {
	byReg := map[int][]argRegBusyRange{}
	for _, b := range lsa.argBusy {
		byReg[b.reg] = append(byReg[b.reg], b)
	}
	for reg, list := range byReg {
		sort.Slice(list, func(i, j int) bool { return list[i].start < list[j].start })
		merged := list[:1]
		for _, b := range list[1:] {
			last := &merged[len(merged)-1]
			if b.start <= last.end+1 {
				if b.end > last.end {
					last.end = b.end
				}
			} else {
				merged = append(merged, b)
			}
		}
		byReg[reg] = merged
	}

	for reg, list := range byReg {
		ri := 0
		for _, interval := range lsa.intervals {
			for ri < len(list) && list[ri].end < interval.Start {
				ri++
			}
			if ri < len(list) && list[ri].start <= interval.End {
				interval.Forbidden |= 1 << uint(reg)
			}
		}
	}
}

// immMaterializedIntoReg reports whether the emitter would stage this
// operand in a scratch GPR (loadFloatIfNeeded / loadIntImmIfNeeded):
// float-typed immediates always materialize; integer immediates do when
// they don't fit an imm32 field.
func immMaterializedIntoReg(op *Operand) bool {
	if op == nil || op.Type != "imm" {
		return false
	}
	if op.DataType == "float" || op.DataType == "double" {
		return true
	}
	return !immFits32(op.Value)
}

// computeEmitterClobberMasks marks intervals whose register would be
// destroyed by emitter sequences that write allocatable registers outside
// the allocator's model:
//
//   - A call clobbers the caller-saved set (RCX, RSI, RDI, R8, R9, R10):
//     the emitter has no save/restore around calls, so an interval live
//     strictly across one (Start < call < End) must sit in a callee-saved
//     register or on the stack. (stbtt rasterizer bug: `s += f(...)` read
//     the old value of s out of %edi after the call clobbered it. RAX/RDX/
//     R11 are already reserved as emitter scratch.)
//   - OpShl/OpShr with a register (or float) shift count stages the count
//     via `movq src, %rcx`; nothing may live in RCX across that
//     instruction (the dst would be overwritten before the shift).
//   - add/sub/and/or/xor/mul/compare with an immediate that needs
//     materializing (float, or !immFits32) loads it into %r10; nothing may
//     live in R10 across that instruction.
func (lsa *LinearScanAllocator) computeEmitterClobberMasks() {
	// Imm32-fit results are cached per unique immediate spelling: the
	// editor TU has ~340K ALU-with-imm instructions but only ~1.4K distinct
	// spellings, and immFits32 parses each from scratch.
	fitsCache := make(map[string]bool, 2048)
	immNeedsR10 := func(op *Operand) bool {
		if op == nil || op.Type != "imm" {
			return false
		}
		if op.DataType == "float" || op.DataType == "double" {
			return true
		}
		if fits, ok := fitsCache[op.Value]; ok {
			return !fits
		}
		fits := immFits32(op.Value)
		fitsCache[op.Value] = fits
		return !fits
	}

	var callIdxs, rcxIdxs, r10Idxs []int
	for i, instr := range lsa.instructions {
		switch instr.Op {
		case OpCall:
			callIdxs = append(callIdxs, i)
		case OpShl, OpShr:
			s2 := instr.Src2
			if s2 == nil || s2.Type != "imm" ||
				s2.DataType == "float" || s2.DataType == "double" {
				rcxIdxs = append(rcxIdxs, i)
			}
		case OpAdd, OpSub, OpAnd, OpOr, OpXor, OpMul,
			OpEq, OpNe, OpLt, OpLe, OpGt, OpGe:
			if immNeedsR10(instr.Src2) {
				r10Idxs = append(r10Idxs, i)
			}
		}
	}

	callMask := uint16(0)
	for _, r := range []int{RCX, RSI, RDI, R8, R9, R10} {
		callMask |= 1 << uint(r)
	}

	// All three index lists are ascending (built in instruction order)
	// and intervals arrive sorted by Start (Allocate sorts before this
	// pass), so each list is swept once with a monotone cursor: total
	// O(intervals + events) instead of 3 binary searches per interval.
	// Predicates match the old searches exactly: a call strictly inside
	// (Start, End); rcx/r10 touching [Start, End].
	ci, ri, ti := 0, 0, 0
	for _, iv := range lsa.intervals {
		for ci < len(callIdxs) && callIdxs[ci] <= iv.Start {
			ci++
		}
		if ci < len(callIdxs) && callIdxs[ci] < iv.End {
			iv.Forbidden |= callMask
		}
		for ri < len(rcxIdxs) && rcxIdxs[ri] < iv.Start {
			ri++
		}
		if ri < len(rcxIdxs) && rcxIdxs[ri] <= iv.End {
			iv.Forbidden |= 1 << uint(RCX)
		}
		for ti < len(r10Idxs) && r10Idxs[ti] < iv.Start {
			ti++
		}
		if ti < len(r10Idxs) && r10Idxs[ti] <= iv.End {
			iv.Forbidden |= 1 << uint(R10)
		}
	}
}

// GetUsedRegisters returns the distinct registers holding temps (for
// callee-saved push/pop in the emitter).
func (lsa *LinearScanAllocator) GetUsedRegisters() []int {
	var mask uint32
	for i := range lsa.slots {
		if r := lsa.slots[i].reg; r >= 0 {
			mask |= 1 << uint(r)
		}
	}
	var regs []int
	for r := 0; mask != 0; r++ {
		if mask&1 != 0 {
			regs = append(regs, r)
		}
		mask >>= 1
	}
	return regs
}

func (lsa *LinearScanAllocator) computeIntervals() {
	maxOff := 0
	for i, instr := range lsa.instructions {
		// Single walk over the three operand chains per instruction:
		// temps extend intervals, var/mem/array deepen the frame extent
		// for spill slots (used to be two separate full walks plus an
		// instrTempsInto scratch pass).
		for k := 0; k < 3; k++ {
			var op *Operand
			switch k {
			case 0:
				op = instr.Dst
			case 1:
				op = instr.Src1
			default:
				op = instr.Src2
			}
			for op != nil {
				if op.Type == "temp" {
					if id, ok := tempID(op.Value); ok {
						lsa.growTemp(id)
						if lsa.slots[id].iv == nil {
							iv := &Interval{
								VarName: op.Value,
								Start:   i,
								End:     i,
								Reg:     -1,
							}
							lsa.slots[id].iv = iv
							lsa.intervals = append(lsa.intervals, iv)
						} else {
							lsa.slots[id].iv.End = i
						}
					}
				} else if (op.Type == "var" || op.Type == "mem" || op.Type == "array") && op.Offset < 0 && !op.IsGlobal {
					if -op.Offset > maxOff {
						maxOff = -op.Offset
					}
				}
				op = op.IndexTemp
			}
		}
	}
	lsa.frameBase = maxOff
}

func (lsa *LinearScanAllocator) expireOldIntervals(interval *Interval) {
	// active is sorted by End, so expired intervals form a prefix: free
	// them and drop the prefix with one memmove instead of one per expiry.
	k := 0
	for k < len(lsa.active) && lsa.active[k].End < interval.Start {
		lsa.freeRegs = append(lsa.freeRegs, lsa.active[k].Reg)
		lsa.active[k] = nil
		k++
	}
	if k == 0 {
		return
	}
	copy(lsa.active, lsa.active[k:])
	for i := len(lsa.active) - k; i < len(lsa.active); i++ {
		lsa.active[i] = nil
	}
	lsa.active = lsa.active[:len(lsa.active)-k]
}

// insertActive adds an interval to active, which is kept sorted by End.
// Active holds at most a handful of intervals (bounded by free registers),
// so insertion sort beats sort.Slice's reflection overhead per interval.
func (lsa *LinearScanAllocator) insertActive(interval *Interval) {
	pos := len(lsa.active)
	for pos > 0 && lsa.active[pos-1].End > interval.End {
		pos--
	}
	lsa.active = append(lsa.active, nil)
	copy(lsa.active[pos+1:], lsa.active[pos:])
	lsa.active[pos] = interval
}

func (lsa *LinearScanAllocator) spillAtInterval(interval *Interval) {
	// Pick the active interval with the furthest end whose register this
	// interval may legally take (never a pending call-arg register).
	// active is sorted by End ascending.
	pick := -1
	for i := len(lsa.active) - 1; i >= 0; i-- {
		if interval.Forbidden&(1<<uint(lsa.active[i].Reg)) == 0 {
			pick = i
			break
		}
	}
	
	if pick >= 0 && lsa.active[pick].End > interval.End {
		// Spill the chosen active interval
		spill := lsa.active[pick]
		interval.Reg = spill.Reg
		if id, ok := tempID(interval.VarName); ok {
			lsa.growTemp(id)
			lsa.slots[id].reg = int32(spill.Reg)
		}

		lsa.spillTemp(spill.VarName)
		lsa.unallocTemp(spill.VarName)

		lsa.active[pick] = interval
		// Active was sorted by End before the swap; only the element at
		// pick can be out of place, so bubble it to its slot instead of
		// re-sorting (sort.Slice per spill showed up in profiles).
		for pick > 0 && lsa.active[pick-1].End > lsa.active[pick].End {
			lsa.active[pick-1], lsa.active[pick] = lsa.active[pick], lsa.active[pick-1]
			pick--
		}
		for pick+1 < len(lsa.active) && lsa.active[pick].End > lsa.active[pick+1].End {
			lsa.active[pick], lsa.active[pick+1] = lsa.active[pick+1], lsa.active[pick]
			pick++
		}
	} else {
		// Spill current interval
		lsa.spillTemp(interval.VarName)
	}
}

// spillTemp assigns a stack slot to a temp (idempotent: each temp is
// spilled at most once, mirroring the old map-len-based offsets exactly).
func (lsa *LinearScanAllocator) spillTemp(varName string) {
	id, ok := tempID(varName)
	if !ok {
		return
	}
	lsa.growTemp(id)
	if lsa.slots[id].spill == 0 {
		lsa.nSpills++
		lsa.slots[id].spill = int32(lsa.frameBase + lsa.nSpills*8)
	}
}

// unallocTemp drops a temp's register assignment (spill of an active).
func (lsa *LinearScanAllocator) unallocTemp(varName string) {
	if id, ok := tempID(varName); ok {
		lsa.growTemp(id)
		lsa.slots[id].reg = -1
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
	
	// Only allocate registers to temporaries, not to variables
	// Variables should stay on the stack
	if operand.Type == "temp" {
		// NOTE: DataType needs no save/restore: assigning Type/Value
		// leaves it untouched (the old dance was a no-op).
		if id, ok := tempID(operand.Value); ok && id < len(lsa.slots) {
			if reg := lsa.slots[id].reg; reg >= 0 {
				operand.Type = "reg"
				operand.Value = regNames[reg]
			} else if off := lsa.slots[id].spill; off != 0 {
				operand.Type = "mem"
				operand.Offset = -int(off)
			}
		}
	} else if operand.Type == "var" {
		// Variables are always on the stack - keep as "var" type
		// The code emitter will handle loading/storing from stack
		// Don't change to register
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
