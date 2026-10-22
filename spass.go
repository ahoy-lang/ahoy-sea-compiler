// spass: single-pass C code generator (TCC-style).
//
// Experimental backend behind `-single-pass` (CompilerOptions.SinglePass).
// The default multi-pass pipeline (AST -> IR -> regalloc -> text -> object)
// is untouched. spass walks the parser AST function-by-function and emits
// machine code directly into section buffers using a value stack:
//
//   - No IR, no register allocation pass, no assembly text.
//   - O0 discipline (TCC §2): every statement boundary spills all live
//     values to their frame homes. Within an expression, a small fixed set
//     of scratch registers is used with stack discipline. This deletes the
//     entire live-range / graph / linear-scan machinery by construction:
//     nothing is ever live across a statement, so nothing can be clobbered
//     across one (the stbtt `s += f(...)` class of bug is impossible).
//   - Jumps are emitted NEAR at O0 (no relaxation loop); -O1 enables
//     short-form selection in the fixup pass (a pure size win, gated).
//   - Forward references (local jumps, RIP-relative data) use per-function
//     fixup records patched when the function ends. Only genuinely
//     external symbols become ELF relocations.
//
// File layout:
//
//	spass.go       this file: types, state, scopes, frame, sections,
//	               symbols, fixups, spill helpers, SysV call sequence.
//	spass_encode.go  structured x86-64 byte encoder (no text parsing).
//	spass_stmt.go    statement codegen (Agent: STMT).
//	spass_expr.go    rvalue expression codegen + calls (Agent: EXPR).
//	spass_mem.go     lvalues, aggregates, floats, conversions (Agent: MEM).
//
// Cross-file API (exact names; do not rename):
//
//	func (s *SPass) genStmt(n *ASTNode) error            // STMT
//	func (s *SPass) genRVal(n *ASTNode) SVal             // EXPR
//	func (s *SPass) addrOf(n *ASTNode) Addr               // MEM (lvalue address)
//	func (s *SPass) convert(v SVal, to CType) SVal        // MEM
//	func (s *SPass) genDataInit(sym string, n *ASTNode)   // MEM (global inits)
//	SVal helpers here: s.imm, s.spillAll, s.store, s.load,
//	  s.materialize, s.emitCall, s.strLit, s.floatConst.
package main

import (
	"fmt"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// C types (width/sign/float classification for codegen)
// ---------------------------------------------------------------------------

// CType is the codegen view of a C type: size in bytes plus the semantic
// bits that select instruction forms (sign extension, float vs int).
type CType struct {
	Name    string // canonical spelling, e.g. "int", "unsigned long", "double", "struct Point", "char*"
	Size    int    // 1, 2, 4, 8 (16-byte structs handled via Size for copies)
	Signed  bool
	IsFloat bool // float or double (no long double in our TU)
	IsPtr   bool
	IsVoid  bool
}

// ctypeOf resolves a type spelling through typedefs to a CType. Pointers
// (any trailing '*', including function pointers) are 8-byte IsPtr.
func ctypeOf(spelling string, p *SPass) (CType, bool) {
	t := stripStorageClass(spelling)
	// Resolve typedef chains (parser.typedefs: alias -> spelling).
	for i := 0; i < 32; i++ {
		if aliased, ok := p.typedefs[t]; ok {
			t = stripStorageClass(aliased)
			continue
		}
		break
	}
	t = strings.TrimSpace(t)
	if strings.HasSuffix(t, "*") {
		return CType{Name: t, Size: 8, IsPtr: true}, true
	}
	return ctypeResolve(t, p)
}

// ctypeResolve handles the non-typedef, non-pointer core.
func ctypeResolve(t string, p *SPass) (CType, bool) {
	base := stripStorageClass(t)
	switch base {
	case "void":
		return CType{Name: "void", Size: 1, IsVoid: true}, true
	case "_Bool", "bool":
		return CType{Name: base, Size: 1}, true
	case "char", "signed char":
		return CType{Name: base, Size: 1, Signed: true}, true
	case "unsigned char":
		return CType{Name: base, Size: 1}, true
	case "short", "short int", "signed short", "signed short int":
		return CType{Name: base, Size: 2, Signed: true}, true
	case "unsigned short", "unsigned short int":
		return CType{Name: base, Size: 2}, true
	case "int", "signed", "signed int", "enum":
		return CType{Name: "int", Size: 4, Signed: true}, true
	case "unsigned", "unsigned int":
		return CType{Name: base, Size: 4}, true
	case "long", "long int", "signed long", "signed long int":
		return CType{Name: base, Size: 8, Signed: true}, true
	case "unsigned long", "unsigned long int":
		return CType{Name: base, Size: 8}, true
	case "long long", "signed long long", "long long int":
		return CType{Name: base, Size: 8, Signed: true}, true
	case "unsigned long long", "unsigned long long int":
		return CType{Name: base, Size: 8}, true
	case "float":
		return CType{Name: base, Size: 4, IsFloat: true}, true
	case "double":
		return CType{Name: base, Size: 8, IsFloat: true}, true
	}
	if strings.HasPrefix(base, "struct ") || strings.HasPrefix(base, "union ") {
		name := strings.TrimSpace(base[len("struct "):])
		if strings.HasPrefix(base, "union ") {
			name = strings.TrimSpace(base[len("union "):])
		}
		// Opaque libc sync types (mirrors the default backend's cSizeOf
		// stubs; headers are skipped so no layout is available).
		switch name {
		case "__pthread_mutex":
			return CType{Name: base, Size: 40}, true // glibc pthread_mutex_t
		case "__pthread_cond":
			return CType{Name: base, Size: 48}, true // glibc pthread_cond_t
		case "__pthread_attr":
			return CType{Name: base, Size: 56}, true // glibc pthread_attr_t
		}
		if def, ok := p.structs[name]; ok {
			return CType{Name: base, Size: def.Size}, true
		}
		// Opaque struct used by value: invalid C; fail loudly.
		if !strings.Contains(t, "*") {
			return CType{}, false
		}
		return CType{Name: t, Size: 8, IsPtr: true}, true
	}
	if strings.HasPrefix(base, "enum ") {
		return CType{Name: "int", Size: 4, Signed: true}, true
	}
	if expr, ok := p.enums[base]; ok {
		_ = expr
		return CType{Name: "int", Size: 4, Signed: true}, true
	}
	// Function designators decay to pointers (handled by callers via IsFunc).
	return CType{}, false
}

// spChunkClasses classifies each 8-byte chunk of a struct for SysV
// passing: "int" (contains int/pointer bytes) or "sse" (all float/double).
// Identical rule to the default backend's structChunkClasses (per-byte
// voting; padding-only chunks default to int), so spass agrees with both
// TU-internal callers and external SysV code to the same degree.
func spChunkClasses(sp *SPass, structName string) []string {
	t := strings.TrimSpace(structName)
	for _, q := range []string{"const ", "volatile ", "static ", "extern ", "struct ", "union "} {
		t = strings.TrimSpace(strings.TrimPrefix(t, q))
	}
	// Resolve typedefs to the underlying struct spelling.
	for i := 0; i < 32; i++ {
		if aliased, ok := sp.typedefs[t]; ok {
			t = strings.TrimSpace(aliased)
			for _, q := range []string{"const ", "volatile ", "static ", "extern ", "struct ", "union "} {
				t = strings.TrimSpace(strings.TrimPrefix(t, q))
			}
			continue
		}
		break
	}
	def, ok := sp.structs[t]
	if !ok {
		return nil
	}
	n := (def.Size + 7) / 8
	if n < 1 {
		n = 1
	}
	classes := make([]string, n)
	for i := range classes {
		classes[i] = "int"
	}
	votes := make([]int, n)
	for _, m := range def.Members {
		isFloat := strings.Contains(m.Type, "float") || strings.Contains(m.Type, "double")
		sz := m.Size
		if sz <= 0 {
			sz = 8
		}
		for b := m.Offset; b < m.Offset+sz; b++ {
			c := b / 8
			if c < 0 || c >= n {
				continue
			}
			if isFloat {
				if votes[c] == 0 {
					votes[c] = 2
				}
			} else {
				votes[c] = 1
			}
		}
	}
	for i := range classes {
		if votes[i] == 2 {
			classes[i] = "sse"
		}
	}
	return classes
}

// ---------------------------------------------------------------------------
// Values and addresses (the value stack)
// ---------------------------------------------------------------------------

// SKind classifies where a computed value lives.
type SKind int

const (
	// KConst: compile-time constant. Imm holds the C literal spelling
	// (parsed on materialize; floats go through floatConst).
	KConst SKind = iota
	// KReg: integer value in physical register Reg.
	KReg
	// KXMM: float/double value in physical xmm register Reg.
	KXMM
	// KMem: value in memory described by Addr (frame slot, global, RIP
	// data, or indirect through a register).
	KMem
	// KVoid: no value (statements, void calls).
	KVoid
)

// SVal is one value-stack entry: a computed rvalue.
type SVal struct {
	Kind SKind
	Type CType
	// KConst: literal spelling exactly as parsed ("12", "0x8000", "1e-5",
	// "'a'", "\"str\"" never reaches here — strings are KMem via strLit).
	Imm string
	// KReg/KXMM: physical register number (REG_RAX.. or 0..15 for xmm).
	Reg int
	// KMem: location.
	Mem Addr
}

// Addr describes a memory location for loads/stores/lea.
type Addr struct {
	// Exactly one of Sym / RegBase is the addressing mode:
	Sym     string // global/section label (RIP-relative); "" if reg-based
	RegBase int    // physical base register (REG_RBP etc.); -1 if Sym
	Disp    int    // displacement
	Index   int    // index register, -1 if none (scaled by Scale)
	Scale   int    // 1, 2, 4, 8
	Size    int    // access width 1, 2, 4, 8
}

// ---------------------------------------------------------------------------
// Single-pass state
// ---------------------------------------------------------------------------

// LocalVar is one frame slot (or global) visible to the current function.
type LocalVar struct {
	Name   string
	Type   CType
	Offset int // negative rbp offset for frame homes
	Global bool
	Sym    string // global symbol name when Global
	// homedByAddr is set when a large-struct param was received by
	// pointer and copied to its home in the prologue's alloc pass; the
	// home pass must skip re-homing it (but still advance its register
	// counter to stay in sync with the caller).
	homedByAddr bool
	// homeStruct is the full-struct home for by-pointer params (p.Offset
	// holds the pointer home).
	homeStruct int
}

// SPass is the whole single-pass backend state for one translation unit.
// Functions are compiled by a worker pool (see CompileSinglePass); each
// worker owns a funcState, while sections/symbols merge in order.
type SPass struct {
	structs  map[string]*StructDef
	typedefs map[string]string
	enums    map[string]int
	funcSigs map[string]*FunctionSignature
	globals  map[string]*Symbol // already-collected globals (from parser)

	oLevel int // -O level: 0 = pure spill discipline; 1 = short jumps

	// TU-wide output (merged in function order by the driver).
	text   []byte
	rodata []byte
	data   []byte
	bss    uint64
	syms   []ObjSymbol
	relocs []ObjReloc

	// Interned constants (dedup across the TU).
	strTab   map[string]string // C unescaped bytes -> .SPS label
	floatTab map[string]string // "D:<bits>" / "F:<bits>" -> .SPC label

	// TU-defined functions (collected in genTU pass 1; calls to these
	// resolve locally at merge instead of via PLT).
	definedFuncs map[string]bool

	// Internal counters.
	strCounter   int
	floatCounter int
	labelCounter int

	mu sync.Mutex // guards TU-wide tables during parallel codegen
}

// funcState is the per-function worker state.
type funcState struct {
	sp     *SPass
	name   string
	retTy  CType
	sret   bool // large-struct return: hidden pointer home at fixed slot
	params []LocalVar

	locals map[string]*LocalVar
	frame  int // next free negative rbp offset (grows down, magnitudes)

	text   []byte            // this function's .text bytes
	rodata []byte            // function-local rodata (switch tables etc.; usually empty)
	labels map[string]uint64 // local label -> text offset

	// Fixups resolved at function end (forward local jumps) or TU merge
	// (calls to TU functions, RIP refs to merged data).
	fixups     []spFixup
	roFixups   []spRoFix
	callFixups []spCallFix
	relocs     []ObjReloc // externals (function-relative offsets; shifted at merge)
	syms       []ObjSymbol

	// Scratch dispensers (round-robin; sound at O0, see scratchReg).
	// rsvScratch reserves loop-held registers from the dispenser.
	scratchIdx int
	xmmIdx     int
	rsvScratch uint64

	// Prologue/epilogue bookkeeping.
	sretHome      int    // frame slot holding the hidden sret pointer
	retLabel      string // all `return`s jump here
	framePatchPos uint64 // text offset of the second sub-rsp imm32 to patch
	paramReserve  int    // bytes reserved by the first sub (param area)
	funcErr       error  // recovered spassError from genStmt/genRVal panics

	// Loop/switch targets for break/continue (innermost last).
	loopStack []loopCtx
}

// loopCtx holds the break/continue targets of one loop or switch.
type loopCtx struct {
	breakLabel    string
	continueLabel string // "" for switch (continue = outer loop's)
	isSwitch      bool
}

// spRoFix is a RIP-relative reference to merged data (strings, float
// constants, globals, rodata chunks), patched at TU merge time.
type spRoFix struct {
	pos uint64 // offset in the function's text of the disp32 field
	key string // SPC/SPS label, global symbol, or rodata chunk label
	add int64  // extra addend (e.g. struct field offsets on globals)
}

// spCallFix is a direct call to a TU-local function, patched at merge.
type spCallFix struct {
	pos    uint64 // offset of the disp32 field (after E8)
	target string // callee function name
}

// Fixup kinds for spFixup.kind.
const (
	fixJump      = 0 // local jump/call-rel32 to local label (func-end patch)
	fixCallLocal = 1 // (reserved: calls use spCallFix instead)
)

// spFixup records one forward reference patched at function end.
type spFixup struct {
	pos    uint64 // offset in funcState.text of the 32-bit field
	label  string // local label ("" if external symbol)
	sym    string // external symbol ("" if local)
	addend int64
	kind   int // 0 = rel32 (jmp/jcc/call/lea/mov-rip), 1 = abs64 (.quad)
	// O1 short-jump support: for kind 0 jumps, the opcode position and
	// long/short forms so the fixup pass can shrink when it fits.
	opPos uint64
	isJcc bool
	cond  byte // jcc condition code (for short form 0x7x)
}

// NewSPass creates the backend. oLevel comes from -O (0 default).
func NewSPass(structs map[string]*StructDef, typedefs map[string]string,
	enums map[string]int, funcSigs map[string]*FunctionSignature,
	globals map[string]*Symbol, oLevel int) *SPass {
	return &SPass{
		structs:      structs,
		typedefs:     typedefs,
		enums:        enums,
		funcSigs:     funcSigs,
		globals:      globals,
		oLevel:       oLevel,
		strTab:       make(map[string]string),
		floatTab:     make(map[string]string),
		definedFuncs: make(map[string]bool),
	}
}

// ---------------------------------------------------------------------------
// Scopes, frame, locals
// ---------------------------------------------------------------------------

// allocSlot reserves Size bytes in the frame (16-aligned at function end)
// and returns the negative rbp offset of the slot base.
func (f *funcState) allocSlot(size int) int {
	if size < 1 {
		size = 1
	}
	// Natural alignment up to 16 (matches existing backend behavior for
	// doubles/structs closely enough for SysV correctness).
	align := size
	if align > 16 {
		align = 16
	}
	if align < 1 {
		align = 1
	}
	f.frame = (f.frame + align - 1) &^ (align - 1)
	f.frame += size
	return -f.frame
}

// declare adds a local; callers provide the already-resolved CType.
func (f *funcState) declare(name string, ty CType) *LocalVar {
	lv := &LocalVar{Name: name, Type: ty, Offset: f.allocSlot(ty.Size)}
	f.locals[name] = lv
	return lv
}

// lookup finds a local or global by name.
func (f *funcState) lookup(name string) (*LocalVar, bool) {
	if lv, ok := f.locals[name]; ok {
		return lv, true
	}
	f.sp.mu.Lock()
	g, ok := f.sp.globals[name]
	f.sp.mu.Unlock()
	if !ok {
		return nil, false
	}
	ty, valid := ctypeOf(g.Type, f.sp)
	if !valid {
		ty = CType{Name: g.Type, Size: g.Size}
		if ty.Size == 0 {
			ty.Size = 8
		}
	}
	return &LocalVar{Name: name, Type: ty, Global: true, Sym: name}, true
}

// ---------------------------------------------------------------------------
// Sections, symbols, relocations (TU-wide, mutex-guarded)
// ---------------------------------------------------------------------------

func (s *SPass) alignRodata(n int) {
	for uint64(len(s.rodata))%uint64(n) != 0 {
		s.rodata = append(s.rodata, 0)
	}
}

// strLit interns a C string literal's *unescaped* bytes, emitting .string
// data, and returns its label.
func (s *SPass) strLit(cooked string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lab, ok := s.strTab[cooked]; ok {
		return lab
	}
	s.strCounter++
	lab := fmt.Sprintf(".SPS%d", s.strCounter)
	s.strTab[cooked] = lab
	// Layout mirrors GAS .string (bytes + NUL); label offsets are fixed
	// at merge time by the driver's rodata assembly.
	syms := s.syms
	_ = syms
	return lab
}

// floatConst interns an IEEE-754 bit pattern (64-bit for double, low 32 for
// float) and returns its rodata label.
func (s *SPass) floatConst(bits uint64, isFloat bool) string {
	key := "D:"
	if isFloat {
		key = "F:"
	}
	key += fmt.Sprintf("%x", bits)
	s.mu.Lock()
	defer s.mu.Unlock()
	if lab, ok := s.floatTab[key]; ok {
		return lab
	}
	s.floatCounter++
	lab := fmt.Sprintf(".SPC%d", s.floatCounter)
	s.floatTab[key] = lab
	return lab
}

// localLabel returns a function-scoped label name.
func (f *funcState) localLabel(prefix string) string {
	f.sp.mu.Lock()
	f.sp.labelCounter++
	n := f.sp.labelCounter
	f.sp.mu.Unlock()
	return fmt.Sprintf(".SPL_%s_%d", prefix, n)
}

// ---------------------------------------------------------------------------
// Spill discipline (O0: TCC-style — nothing live across a statement)
// ---------------------------------------------------------------------------

// Scratch registers for within-expression evaluation. Caller-saved and
// never used for homes, so clobbering across calls is fine: at O0 every
// value that must survive is in memory before any call is emitted.
var spScratchRegs = []int{REG_RAX, REG_RCX, REG_RDX, REG_RSI, REG_RDI, REG_R8, REG_R9, REG_R10, REG_R11}

// scratchNext hands out scratch registers round-robin, skipping reserved
// ones (see reserveScratch). At O0, straight-line code holds at most one
// live scratch across a scratchReg() call, so round-robin is sound there;
// loops that keep a base register live across iterations must reserve it.
func (f *funcState) scratchNext() int {
	for i := 0; i < len(spScratchRegs); i++ {
		r := spScratchRegs[f.scratchIdx%len(spScratchRegs)]
		f.scratchIdx++
		if !f.scratchReserved(r) {
			return r
		}
	}
	panic("spass: scratch pool exhausted (all regs reserved)")
}

// scratchReserved reports whether a register is reserved.
func (f *funcState) scratchReserved(reg int) bool {
	for i, r := range spScratchRegs {
		if r == reg {
			return f.rsvScratch&(1<<uint(i)) != 0
		}
	}
	return false
}

// reserveScratch marks registers as unavailable to the dispenser until
// released. Copy loops call this on their base/index registers so the
// per-chunk scratch picks can never clobber a live base (the bell
// assets_load_model crash: round-robin re-issued a live base after ~9
// chunks). Returns a release function for defer.
func (f *funcState) reserveScratch(regs ...int) func() {
	var bits uint64
	for _, reg := range regs {
		for i, r := range spScratchRegs {
			if r == reg {
				bits |= 1 << uint(i)
			}
		}
	}
	// RBP/RSP are never in the pool; ignore silently.
	f.rsvScratch |= bits
	return func() { f.rsvScratch &^= bits }
}

// addrRegs lists the live register components of an address (base/index)
// for scratch reservation. RBP/RSP homes need none; Sym bases are
// materialized per chunk (consecutive with the value reg, hence safe).
// A zero RegBase alongside Sym is just an unset field, never RAX.
func addrRegs(a Addr) []int {
	if a.Sym != "" {
		return nil
	}
	var out []int
	if a.RegBase >= 0 && a.RegBase != REG_RBP && a.RegBase != REG_RSP {
		out = append(out, a.RegBase)
	}
	if a.Index >= 0 {
		out = append(out, a.Index)
	}
	return out
}

// reserveRegs marks address registers as unavailable to the scratch
// dispenser until the returned release function runs (defer it).
func (f *funcState) reserveRegs(regs ...int) func() {
	return f.reserveScratch(regs...)
}

// spillAll stores every stack-held value to its home. In this design every
// named local/global already HAS a memory home and expression temps use
// dedicated temp slots, so spilling = nothing to do for homes; this hook
// exists for the -O1 future (register-resident values) and as the single
// choke point statement codegen calls.
func (f *funcState) spillAll() {
	// O0: all values already in memory. No-op by construction.
}

// tempSlot returns a frame slot for an expression temporary of Size bytes.
// Slots are per-statement-epoch: the frame only grows, so this is a bump
// allocator (bounded by the largest statement in the function — same as
// TCC's statement-lifetime temporaries).
func (f *funcState) tempSlot(size int) Addr {
	off := f.allocSlot(size)
	return Addr{RegBase: REG_RBP, Disp: off, Index: -1, Scale: 1, Size: size}
}

// ---------------------------------------------------------------------------
// Loads, stores, materialization (pure emission on analyzed values)
// ---------------------------------------------------------------------------

// load emits value-from-address into a scratch reg/xmm and returns it.
func (f *funcState) load(a Addr, ty CType) SVal {
	if ty.IsFloat {
		x := f.scratchXMM()
		f.encMovXMMMem(x, a, true)
		return SVal{Kind: KXMM, Type: ty, Reg: x}
	}
	r := f.scratchReg()
	f.encMovRegMem(r, a, ty)
	return SVal{Kind: KReg, Type: ty, Reg: r}
}

// store emits value-into-address.
func (f *funcState) store(a Addr, v SVal) {
	// KMem sources must be loaded first (no case below matches them);
	// float constants can't move imm->mem either (same as materialize).
	// (EXPR-agent skeleton fix: KMem stores were silently dropped, so
	// e.g. `p = &x` to a global vanished. KConst-int still takes the
	// direct C7 path below.)
	if v.Kind == KMem || (v.Kind == KConst && v.Type.IsFloat) {
		v = f.materialize(v)
	}
	switch v.Kind {
	case KReg:
		f.encMovMemReg(a, v.Reg, v.Type)
	case KXMM:
		f.encMovXMMMem(v.Reg, a, false)
	case KConst:
		f.encMovMemImm(a, v.Imm, v.Type)
	}
}

// materialize forces a value into a register (KReg) or xmm (KXMM).
func (f *funcState) materialize(v SVal) SVal {
	switch v.Kind {
	case KReg, KXMM:
		return v
	case KMem:
		return f.load(v.Mem, v.Type)
	case KConst:
		if v.Type.IsFloat {
			bits, isF := floatBitsOf(v.Imm, v.Type)
			lab := f.sp.floatConst(bits, isF)
			x := f.scratchXMM()
			f.encMovXMMRip(x, lab, v.Type.Size)
			return SVal{Kind: KXMM, Type: v.Type, Reg: x}
		}
		r := f.scratchReg()
		f.encMovRegImm(r, v.Imm, v.Type)
		return SVal{Kind: KReg, Type: v.Type, Reg: r}
	}
	return SVal{Kind: KVoid}
}

// ---------------------------------------------------------------------------
// Scratch management (within-expression stack discipline)
// ---------------------------------------------------------------------------

func (f *funcState) scratchReg() int {
	// O0: expression depth is small; the evaluation order in spass_expr
	// uses at most 2 live scratches at once (left is spilled to a temp
	// slot before evaluating right, TCC-style), so a rotating handout
	// from the caller-saved set is sound.
	return f.scratchNext()
}

// scratchXMM hands out xmm0/xmm1 alternating (same discipline as above).
func (f *funcState) scratchXMM() int {
	x := f.xmmIdx % 2
	f.xmmIdx++
	return x
}

// ---------------------------------------------------------------------------
// SysV call sequence (pure emission; arg evaluation lives in spass_expr)
// ---------------------------------------------------------------------------

var spIntArgRegs = []int{REG_RDI, REG_RSI, REG_RDX, REG_RCX, REG_R8, REG_R9}

// emitCall emits the call itself: args must already be evaluated into vals
// (in source order). Handles int/xmm regs, stack spillover, %al for
// varargs, hidden sret pointer (RDI), and return collection
// (rax/xmm0/sret buffer). Returns the return-value SVal.
//
// Staging discipline (sound at O0): every KReg/KXMM arg is spilled to a
// temp slot FIRST, so no placement step can clobber an unevaluated arg.
// Placement then uses only RAX/R10/R11 as staging (never arg regs) plus
// direct mem/reg/imm forms that need no scratch at all.
func (f *funcState) emitCall(target SVal, isVarArg bool, nFloatArgs int, args []SVal, retTy CType, sretBuf *Addr) SVal {
	// Phase 0: hidden sret pointer occupies RDI when returning large.
	intStart := 0
	if sretBuf != nil {
		f.encLeaRegMem(REG_RDI, *sretBuf)
		intStart = 1
	}
	// Spill the indirect-call target first: arg spilling below uses
	// stores (via scratch regs) that could otherwise clobber it.
	tgt := target
	if tgt.Kind == KReg || tgt.Kind == KXMM {
		slot := f.tempSlot(8)
		f.store(slot, tgt)
		tgt = SVal{Kind: KMem, Type: target.Type, Mem: slot}
	}
	// Phase 1: spill register-resident args to temp slots.
	for i := range args {
		a := args[i]
		if a.Kind == KReg || a.Kind == KXMM {
			slot := f.tempSlot(8)
			f.store(slot, a)
			args[i] = SVal{Kind: KMem, Type: a.Type, Mem: slot}
		}
	}
	// Classify + assign: first 6 int-ish to RDI..R9, first 8 floats to
	// xmm0..7, remainder on the stack (right-to-left push order = forward
	// emission after alignment).
	type slot struct {
		v     SVal
		reg   int // -1 => stack
		isXMM bool
	}
	var slots []slot
	intRegs, xmmRegs := intStart, 0
	for _, a := range args {
		if a.Type.IsFloat {
			if xmmRegs < 8 {
				slots = append(slots, slot{a, xmmRegs, true})
				xmmRegs++
			} else {
				slots = append(slots, slot{a, -1, true})
			}
		} else {
			if intRegs < 6 {
				slots = append(slots, slot{a, spIntArgRegs[intRegs], false})
				intRegs++
			} else {
				slots = append(slots, slot{a, -1, false})
			}
		}
	}
	// Stack args: count first for alignment (total %rsp movement must keep
	// 16-alignment at the call: our rsp is 8-mod-16 after the call insn
	// pushes its return address... standard: before call, rsp%16==8, so
	// stack-arg bytes must be a multiple of 16... we sub to align).
	nStack := 0
	for _, sl := range slots {
		if sl.reg < 0 {
			nStack += 8
		}
	}
	nStackAligned := (nStack + 15) &^ 15
	if nStackAligned > 0 {
		f.encSubRsp(nStackAligned)
	}
	// Staging uses RAX/R10 only (never arg regs, never XMM).
	off := 0
	for _, sl := range slots {
		if sl.reg >= 0 {
			continue
		}
		v := sl.v
		if v.Kind == KXMM {
			f.encMovXMMMem(v.Reg, Addr{RegBase: REG_RSP, Disp: off, Index: -1, Scale: 1, Size: 8}, false)
		} else if v.Kind == KReg {
			f.encMovMemReg(Addr{RegBase: REG_RSP, Disp: off, Index: -1, Scale: 1, Size: 8}, v.Reg, v.Type)
		} else if v.Kind == KMem {
			f.encMovRegMem(REG_RAX, v.Mem, v.Type)
			f.encMovMemReg(Addr{RegBase: REG_RSP, Disp: off, Index: -1, Scale: 1, Size: 8}, REG_RAX, v.Type)
		} else {
			// KConst: int (possibly wide) or float.
			if v.Type.IsFloat {
				bits, isF := floatBitsOf(v.Imm, v.Type)
				lab := f.sp.floatConst(bits, isF)
				sz := 8
				if isF {
					sz = 4
				}
				f.encMovXMMRip(REG_RAX, lab, sz)
				f.encMovXMMMem(REG_RAX, Addr{RegBase: REG_RSP, Disp: off, Index: -1, Scale: 1, Size: 8}, false)
			} else {
				f.encMovRegImm(REG_RAX, v.Imm, v.Type)
				f.encMovMemReg(Addr{RegBase: REG_RSP, Disp: off, Index: -1, Scale: 1, Size: 8}, REG_RAX, v.Type)
			}
		}
		off += 8
	}
	// Register args. All values are KMem/KConst here (phase-1 spill), so
	// placement needs no scratch except via RAX for int->xmm consts.
	for _, sl := range slots {
		if sl.reg < 0 {
			continue
		}
		v := sl.v
		if sl.isXMM {
			switch v.Kind {
			case KXMM:
				if v.Reg != sl.reg {
					f.encMovXMMReg(sl.reg, v.Reg)
				}
			case KMem:
				f.encMovXMMMem(sl.reg, v.Mem, true)
			default:
				// KConst: float via rodata, int via cvt.
				if v.Type.IsFloat {
					bits, isF := floatBitsOf(v.Imm, v.Type)
					lab := f.sp.floatConst(bits, isF)
					sz := 8
					if isF {
						sz = 4
					}
					f.encMovXMMRip(sl.reg, lab, sz)
				} else {
					f.encMovRegImm(REG_RAX, v.Imm, v.Type)
					f.encIntToXMM(sl.reg, SVal{Kind: KReg, Type: v.Type, Reg: REG_RAX})
				}
			}
		} else {
			switch v.Kind {
			case KReg:
				if v.Reg != sl.reg {
					f.encMovRegReg(sl.reg, v.Reg, 8)
				}
			case KXMM:
				f.encXMMToInt(sl.reg, v)
			case KMem:
				// Load with the VALUE's own width (width-correct mov:
				// movslq/movl/movzx as appropriate). A hardcoded 64-bit
				// load here picks up garbage upper bytes when the slot
				// holds a narrower spill (the min8 malloc-size bug).
				f.encMovRegMem(sl.reg, v.Mem, v.Type)
			default:
				// KConst int: extend per signedness (signed: C7 sign
				// extends; unsigned: movl zero-extends). Blindly
				// widening unsigned breaks values > INT64... > INT_MAX.
				nt := v.Type
				if nt.Size < 8 && nt.Signed {
					nt.Size = 8
				}
				f.encMovRegImm(sl.reg, v.Imm, nt)
			}
		}
	}
	if isVarArg {
		f.encMovRegImm(REG_RAX, fmt.Sprintf("%d", nFloatArgs), CType{Name: "int", Size: 4, Signed: true})
	}
	// The call. Indirect targets load through fixed R13: the pool
	// dispenser could hand out an already-assigned argument register
	// (e.g. RDI), clobbering the arg. R13 is never an arg home, never
	// handed out, and survives the call (callee-saved; no agent holds
	// values there across a call). (EXPR-agent skeleton fix.)
	switch tgt.Kind {
	case KConst:
		// Direct call by symbol name (in Imm).
		f.encCallSym(tgt.Imm)
	default:
		tm := tgt
		if tm.Kind == KMem {
			f.encMovRegMem(REG_R13, tm.Mem, CType{Name: "ptr", Size: 8, IsPtr: true})
		} else if tm.Kind == KReg {
			if tm.Reg != REG_R13 {
				f.encMovRegReg(REG_R13, tm.Reg, 8)
			}
		} else {
			t := f.materialize(tm)
			if t.Reg != REG_R13 {
				f.encMovRegReg(REG_R13, t.Reg, 8)
			}
		}
		// Function pointer in a register.
		f.encCallReg(REG_R13)
	}
	if nStackAligned > 0 {
		f.encAddRsp(nStackAligned)
	}
	// Collect return.
	if retTy.IsVoid {
		return SVal{Kind: KVoid}
	}
	if retTy.Size > 16 {
		// Large struct: caller passed hidden pointer (sretBuf set by the
		// call-site builder); value lives there.
		return SVal{Kind: KMem, Type: retTy, Mem: *sretBuf}
	}
	if retTy.IsFloat {
		return SVal{Kind: KXMM, Type: retTy, Reg: 0}
	}
	return SVal{Kind: KReg, Type: retTy, Reg: REG_RAX}
}

// ---------------------------------------------------------------------------
// Prologue / epilogue (mirrors the default backend: homed params,
// callee-saved RBX/R12-R15, 16-aligned frames)
// ---------------------------------------------------------------------------

// homeStructParam homes one struct parameter chunk-by-chunk, mirroring
// emitCall's register/stack assignment (int regs and XMM regs counted
// independently, stack slots in encounter order). Chunk sizes follow
// exStructChunks exactly (8-byte pieces, shrinking 4/2/1 tail).
func (f *funcState) homeStructParam(p *LocalVar, slot int, classes []string, argIdx, xmmIdx, stackOff *int) {
	off := 0
	for off < p.Type.Size {
		chunk := 8
		for chunk > 1 && p.Type.Size-off < chunk {
			chunk /= 2
		}
		// Class of the 8-byte region this chunk starts in.
		cl := "int"
		if ri := off / 8; ri < len(classes) {
			cl = classes[ri]
		}
		cAddr := Addr{RegBase: REG_RBP, Disp: slot + off, Index: -1, Scale: 1, Size: chunk}
		if cl == "sse" && chunk >= 4 {
			if *xmmIdx < 8 {
				f.encMovXMMMem(*xmmIdx, cAddr, false)
				*xmmIdx++
			} else {
				f.encMovMemStackParam(cAddr.Disp, *stackOff, chunk)
				*stackOff += 8
			}
		} else {
			if *argIdx < 6 {
				var ct CType
				switch chunk {
				case 8:
					ct = CType{Name: "long", Size: 8, Signed: true}
				case 4:
					ct = CType{Name: "int", Size: 4, Signed: true}
				case 2:
					ct = CType{Name: "short", Size: 2, Signed: true}
				default:
					ct = CType{Name: "char", Size: 1, Signed: true}
				}
				f.encMovMemReg(cAddr, spIntArgRegs[*argIdx], ct)
				*argIdx++
			} else {
				f.encMovMemStackParam(cAddr.Disp, *stackOff, chunk)
				*stackOff += 8
			}
		}
		off += chunk
	}
}

// emitPrologue writes the function header. Params are homed to frame slots
// (ints from arg regs, floats from xmm regs, stack params from positive rbp
// offsets, hidden sret pointer from RDI when largeRet). Returns nothing;
// param homes are in f.params.
// spSavedArea is the area pushed by emitPrologue after the frame pointer
// is established: RBX/R12-R15 (5 regs = 40 bytes; the RBP push predates
// `mov %rsp,%rbp` so saved RBP sits AT rbp, not below it). Frame homes
// allocated by declare()/allocSlot() start below it; the epilogue rewinds
// rsp to rbp-spSavedArea before popping.
const spSavedArea = 5 * 8

func (f *funcState) emitPrologue() {
	f.encLabel(f.name)
	f.encPush(REG_RBP)
	f.encMovRegReg(REG_RBP, REG_RSP, 8)
	// Callee-saved.
	for _, r := range []int{REG_RBX, REG_R12, REG_R13, REG_R14, REG_R15} {
		f.encPush(r)
	}
	// Incoming argument registers stay live until their homes are
	// written below; reserve them so struct-copy scratch picks (and any
	// other prologue scratch use) can never clobber a not-yet-homed
	// argument (engine_open lost `title` this way).
	releaseArgs := f.reserveScratch(REG_RDI, REG_RSI, REG_RDX, REG_RCX, REG_R8, REG_R9)
	defer releaseArgs()
	// Reserve the pushed area so frame homes never overlap it.
	f.frame += spSavedArea
	argIdx, xmmIdx, stackOff := 0, 0, 16
	if f.sret {
		// Hidden return-buffer pointer arrives in RDI.
		slot := f.allocSlot(8)
		f.sretHome = slot
		argIdx = 1
	}
	for i := range f.params {
		p := &f.params[i]
		// Large structs (>16B) arrive by pointer (sea internal ABI).
		// Allocate the pointer slot and the struct home here; homing
		// and the copy happen in the home pass below (after the first
		// reservation), so nothing executes below a reserved RSP.
		if p.Type.Size > 16 && !p.Type.IsPtr && !p.Type.IsFloat && !p.Type.IsVoid {
			p.Offset = f.allocSlot(8) // pointer home (filled below)
			p.homedByAddr = true
			// Full-struct home for the copy target.
			p.homeStruct = f.allocSlot(p.Type.Size)
			continue
		}
		slot := f.allocSlot(max(p.Type.Size, 1))
		p.Offset = slot
	}
	// Split reservation: the param area is known now, the rest only after
	// body codegen. Homes execute before the second sub, so the param area
	// must already be reserved — otherwise homing writes below RSP
	// (red-zone violation for big structs; engine_open crashed here).
	// Both subs keep 16-alignment: pushes (48) ≡ 0, so total ≡ 8 mod 16.
	paramBytes := f.frame - spSavedArea
	paramReserve := (paramBytes + 15) &^ 15
	if paramReserve > 0 {
		f.encSubRsp(paramReserve)
	}
	f.paramReserve = paramReserve
	if f.sret {
		f.encMovMemReg(Addr{RegBase: REG_RBP, Disp: f.sretHome, Index: -1, Scale: 1, Size: 8}, REG_RDI, CType{Name: "ptr", Size: 8})
	}
	for i := range f.params {
		p := &f.params[i]
		slot := p.Offset
		if p.homedByAddr {
			// By-pointer large struct: home the incoming pointer, then
			// copy the full struct so member paths work unchanged.
			// (Single consumption point for argIdx/stackOff.)
			if argIdx < 6 {
				f.encMovMemReg(Addr{RegBase: REG_RBP, Disp: slot, Index: -1, Scale: 1, Size: 8}, spIntArgRegs[argIdx], CType{Name: "void*", Size: 8, IsPtr: true})
				argIdx++
			} else {
				f.encMovMemStackParam(slot, stackOff, 8)
				stackOff += 8
			}
			pr := f.scratchReg()
			f.encMovRegMem(pr, Addr{RegBase: REG_RBP, Disp: slot, Index: -1, Scale: 1, Size: 8}, CType{Name: "void*", Size: 8, IsPtr: true})
			// Protect the pointer across copyStruct's internal scratch
			// use (see reserveScratch).
			release := f.reserveScratch(pr)
			f.copyStruct(
				Addr{RegBase: REG_RBP, Disp: p.homeStruct, Index: -1, Scale: 1, Size: p.Type.Size},
				Addr{RegBase: pr, Disp: 0, Index: -1, Scale: 1, Size: p.Type.Size},
				p.Type.Size)
			release()
			p.Offset = p.homeStruct
			continue
		}
		// Struct params arrive chunked (SysV eightbyte classification):
		// home each chunk from its int/XMM register or stack slot, in
		// order — mirroring emitCall's assignment exactly.
		if classes := spChunkClasses(f.sp, p.Type.Name); classes != nil {
			f.homeStructParam(p, slot, classes, &argIdx, &xmmIdx, &stackOff)
			continue
		}
		if p.Type.IsFloat {
			if xmmIdx < 8 {
				f.encMovXMMMem(xmmIdx, Addr{RegBase: REG_RBP, Disp: slot, Index: -1, Scale: 1, Size: p.Type.Size}, false)
				xmmIdx++
			} else {
				f.encMovMemStackParam(slot, stackOff, p.Type.Size)
				stackOff += 8
			}
			continue
		}
		if argIdx < 6 {
			f.encMovMemReg(Addr{RegBase: REG_RBP, Disp: slot, Index: -1, Scale: 1, Size: p.Type.Size}, spIntArgRegs[argIdx], p.Type)
			argIdx++
		} else {
			f.encMovMemStackParam(slot, stackOff, p.Type.Size)
			stackOff += 8
		}
	}
	// Frame size patch: reserve the REMAINDER (aligned) now that body
	// codegen (below) has grown the frame. Record position, patch at
	// function end when f.frame is final.
	f.framePatchPos = uint64(len(f.text))
	f.encSubRsp(0) // patched later
}

// emitEpilogue patches the frame reservation, restores callee-saveds, and
// emits `ret`. All `return` statements jump to f.retLabel.
func (f *funcState) emitEpilogue() {
	// Second sub covers the remainder (total minus the already-reserved
	// param area). Pushes (48) are ≡ 0 mod 16 and entry rsp is 0 mod 16,
	// so total ≡ 8 keeps every call site at rsp ≡ 8 (misalignment
	// faults in movaps for printf %f and friends).
	total := ((f.frame + 15) &^ 15) + 8 - f.paramReserve
	if total < 0 {
		total = 0
	}
	f.patchU32(f.framePatchPos+3, uint32(total))
	f.defineLabel(f.retLabel)
	// Struct return: value already in sret buffer (stores wrote there via
	// the hidden pointer); rax gets the buffer address (SysV).
	if f.sret {
		f.encLeaRegMem(REG_RAX, Addr{RegBase: REG_RBP, Disp: f.sretHome, Index: -1, Scale: 1, Size: 8})
		f.encMovRegMem(REG_RAX, Addr{RegBase: REG_RAX, Disp: 0, Index: -1, Scale: 1, Size: 8}, CType{Name: "ptr", Size: 8})
	}
	f.encLeaRegMem(REG_RSP, Addr{RegBase: REG_RBP, Disp: -spSavedArea, Index: -1, Scale: 1, Size: 8})
	for i := len([]int{REG_RBX, REG_R12, REG_R13, REG_R14, REG_R15}) - 1; i >= 0; i-- {
		f.encPop([]int{REG_RBX, REG_R12, REG_R13, REG_R14, REG_R15}[i])
	}
	f.encPop(REG_RBP)
	f.encRet()
}

// ---------------------------------------------------------------------------
// Driver: whole-TU single-pass compile
// ---------------------------------------------------------------------------

// CompileSinglePass runs PP (existing) + parse (existing) + spass codegen +
// native object write + system link. Used when -single-pass is passed.
func CompileSinglePass(cp *CompilerPipeline) error {
	sp := NewSPass(cp.selector.structs, cp.selector.typedefs, cp.selector.enums,
		cp.selector.functions, cp.selector.globalVars, cp.options.OptimizationLevel)
	if err := sp.genTU(cp.ast); err != nil {
		return err
	}
	obj := sp.finish()
	// TODO: write object + link (wired in pipeline step below).
	_ = obj
	return nil
}

// genTU walks top-level declarations: globals first (data/bss/comm/rodata),
// function names collected for local-call resolution, then functions
// (parallel codegen, ordered merge).
func (s *SPass) genTU(ast *ASTNode) error {
	// Pass 1 (sequential): globals + collect function nodes/names.
	// Multi-declarator globals (`static float a, b;`) parse as a top-level
	// NodeBlock of NodeVarDecls; unwrap so each lands in genGlobal.
	var funcs []*ASTNode
	var visit func(n *ASTNode) error
	visit = func(n *ASTNode) error {
		if n.Type == NodeBlock {
			for _, k := range n.Children {
				if err := visit(k); err != nil {
					return err
				}
			}
			return nil
		}
		switch n.Type {
		case NodeFunction:
			if len(n.Children) > 0 {
				funcs = append(funcs, n)
				s.definedFuncs[n.Name] = true
			}
		default:
			if err := s.genGlobal(n); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range ast.Children {
		if err := visit(child); err != nil {
			return err
		}
	}
	// Pass 2 (parallel): one funcState per function.
	type fres struct {
		idx        int
		name       string
		text       []byte
		rodata     []byte
		relocs     []ObjReloc
		syms       []ObjSymbol
		roFixups   []spRoFix
		callFixups []spCallFix
		global     bool // .globl (non-static) vs local
		size       int  // for .size
		err        error
	}
	results := make([]fres, len(funcs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64) // cap workers (functions are many)
	for i, fn := range funcs {
		wg.Add(1)
		go func(idx int, fn *ASTNode) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fs := &funcState{sp: s, locals: make(map[string]*LocalVar), labels: make(map[string]uint64)}
			err := fs.genFunction(fn)
			r := fres{idx: idx, name: fn.Name}
			if err == nil {
				r.text, r.rodata = fs.text, fs.rodata
				r.relocs, r.syms = fs.relocs, fs.syms
				r.roFixups, r.callFixups = fs.roFixups, fs.callFixups
				r.global = !funcIsLocalSp(fn)
				r.size = len(fs.text)
			} else {
				r.err = fmt.Errorf("%s: %w", fn.Name, err)
			}
			results[idx] = r
		}(i, fn)
	}
	wg.Wait()
	var errs []string
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, r.err.Error())
		}
	}
	if len(errs) > 0 {
		// Report every failing function (bring-up audit), not just the
		// first: one bell run then lists the whole coverage gap.
		n := len(errs)
		if n > 25 {
			errs = append(errs[:25], fmt.Sprintf("... and %d more", n-25))
		}
		return fmt.Errorf("single-pass errors (%d functions):\n  %s", n, strings.Join(errs, "\n  "))
	}
	// Pass 3 (ordered merge): concatenate text, shift relocs, record
	// function offsets for call/data fixups.
	funcOff := make(map[string]uint64, len(results))
	var off uint64
	for _, r := range results {
		funcOff[r.name] = off
		off += uint64(len(r.text))
	}
	s.text = make([]byte, 0, off)
	syms := []ObjSymbol{}
	for _, r := range results {
		base := uint64(len(s.text))
		s.text = append(s.text, r.text...)
		syms = append(syms, ObjSymbol{
			Name: r.name, Section: ".text", Offset: base,
			Global: r.global, Size: uint64(r.size),
		})
		for _, rl := range r.relocs {
			rl.Offset += base
			s.relocs = append(s.relocs, rl)
		}
		_ = base
	}
	// Resolve TU-local calls (now that every function offset is known).
	for i, r := range results {
		base := funcOff[r.name]
		for _, cf := range r.callFixups {
			tgt, ok := funcOff[cf.target]
			if !ok {
				return fmt.Errorf("call to unknown function %s", cf.target)
			}
			disp := int64(tgt) - int64(base+cf.pos+4)
			off := base + cf.pos
			s.text[off] = byte(disp)
			s.text[off+1] = byte(disp >> 8)
			s.text[off+2] = byte(disp >> 16)
			s.text[off+3] = byte(disp >> 24)
		}
		_ = i
	}
	s.syms = append(s.syms, syms...)
	// Data references (RIP-relative) become real relocations: cross-section
	// distances are the linker's job, not the merge's. The linker resolves
	// PC32 against the SPC/SPS/global symbols finish() defines.
	for _, r := range results {
		base := funcOff[r.name]
		for _, rf := range r.roFixups {
			s.relocs = append(s.relocs, ObjReloc{
				Section: ".text", Offset: base + rf.pos,
				Symbol: rf.key, Type: R_X86_64_PC32, Addend: rf.add - 4,
			})
		}
	}
	// Data sections (.data/.bss/.comm recorded by genGlobal in pass 1;
	// string/float tables materialized here) and their symbols.
	s.layoutRodata()
	return nil
}

// funcIsLocalSp reports internal linkage (static / plain inline).
func funcIsLocalSp(fn *ASTNode) bool {
	rt := fn.ReturnType
	return funcHasWord(rt, "static") ||
		(funcHasWord(rt, "inline") && !funcHasWord(rt, "extern"))
}

// genGlobal handles file-scope declarations (data/bss/comm/rodata).
func (s *SPass) genGlobal(n *ASTNode) error {
	// Implemented in spass_mem.go (needs const-init evaluation).
	return s.genDataInit("", n)
}

// layoutRodata materializes the interned string/float tables into s.rodata
// (SPS labels in numeric order, then SPC labels) and registers their
// symbols. Called once at merge; workers only intern (never emit bytes).
func (s *SPass) layoutRodata() {
	// Strings: .SPS1..N in order.
	for i := 1; i <= s.strCounter; i++ {
		lab := fmt.Sprintf(".SPS%d", i)
		var cooked string
		for k, v := range s.strTab {
			if v == lab {
				cooked = k
				break
			}
		}
		s.alignRodata(1)
		off := uint64(len(s.rodata))
		s.rodata = append(s.rodata, []byte(cooked)...)
		s.rodata = append(s.rodata, 0)
		s.syms = append(s.syms, ObjSymbol{Name: lab, Section: ".rodata", Offset: off})
	}
	// Floats: .SPC1..N in order.
	for i := 1; i <= s.floatCounter; i++ {
		lab := fmt.Sprintf(".SPC%d", i)
		var key string
		for k, v := range s.floatTab {
			if v == lab {
				key = k
				break
			}
		}
		isF := strings.HasPrefix(key, "F:")
		var bits uint64
		fmt.Sscanf(key[2:], "%x", &bits)
		s.alignRodata(4)
		off := uint64(len(s.rodata))
		if isF {
			for j := 0; j < 4; j++ {
				s.rodata = append(s.rodata, byte(bits>>(8*j)))
			}
		} else {
			for j := 0; j < 8; j++ {
				s.rodata = append(s.rodata, byte(bits>>(8*j)))
			}
		}
		s.syms = append(s.syms, ObjSymbol{Name: lab, Section: ".rodata", Offset: off})
	}
}

// finish assembles the TU-wide object: sections already merged by genTU
// (text + relocs + function symbols; data/bss/comm by genGlobal; rodata by
// layoutRodata). Adds section symbols and returns the ObjectFile.
func (s *SPass) finish() *ObjectFile {
	// Register every relocation target as a symbol so WriteELF can resolve
	// it: TU-local functions/data are already present; anything else
	// becomes an UNDEF entry (external libc calls like printf/malloc).
	have := make(map[string]bool, len(s.syms))
	for _, sy := range s.syms {
		have[sy.Name] = true
	}
	for _, r := range s.relocs {
		if !have[r.Symbol] {
			have[r.Symbol] = true
			s.syms = append(s.syms, ObjSymbol{Name: r.Symbol, Global: true})
		}
	}
	obj := &ObjectFile{
		Text:    s.text,
		Rodata:  s.rodata,
		Data:    s.data,
		BSS:     s.bss,
		Symbols: s.syms,
		Relocs:  s.relocs,
	}
	return obj
}

// ---------------------------------------------------------------------------
// Agent stubs (STMT/EXPR/MEM replace these bodies; signatures are frozen)
// ---------------------------------------------------------------------------

// spassError is raised for unsupported constructs; recovered in genFunction.
type spassError struct {
	what string
	node *ASTNode
}

func (e *spassError) Error() string {
	if e.node != nil {
		return fmt.Sprintf("spass: unsupported %s (node %v)", e.what, e.node.Type)
	}
	return "spass: unsupported " + e.what
}

func unsupported(what string, n *ASTNode) *spassError {
	return &spassError{what: what, node: n}
}

// genStmt compiles one statement (spass_stmt.go; STMT agent owns the body).
// genRVal compiles an rvalue expression (spass_expr.go; EXPR agent owns the
// body). addrOf/convert/genDataInit below are MEM-owned (stubs until MEM
// lands).

// addrOf/convert/genDataInit are MEM-owned (spass_mem.go).

// ---------------------------------------------------------------------------
// Misc helpers shared by the agents
// ---------------------------------------------------------------------------

// isLargeStruct reports SysV "memory class" returns (>16 bytes).
func isLargeStructSp(ty CType) bool {
	return !ty.IsFloat && !ty.IsPtr && !ty.IsVoid && ty.Size > 16
}

// typeOfNode resolves an expression node's DataType spelling.
func (s *SPass) typeOfNode(n *ASTNode) (CType, bool) {
	if n == nil {
		return CType{}, false
	}
	if n.DataType != "" {
		if ty, ok := ctypeOf(n.DataType, s); ok {
			return ty, true
		}
	}
	// Fallbacks by node kind.
	switch n.Type {
	case NodeNumber:
		if isFloatSpelling(n.Value) {
			return CType{Name: "double", Size: 8, IsFloat: true}, true
		}
		return CType{Name: "int", Size: 4, Signed: true}, true
	case NodeString:
		return CType{Name: "char*", Size: 8, IsPtr: true}, true
	}
	return CType{}, false
}

// intConst reports whether Imm is an integer literal (not float/char).
func intConst(imm string) bool {
	if imm == "" || isFloatSpelling(imm) {
		return false
	}
	if len(imm) >= 2 && imm[0] == '\'' {
		return true // char literal folds at materialize
	}
	return true
}

// ---------------------------------------------------------------------------
// Function driver (core infra; statement bodies via genStmt in spass_stmt.go)
// ---------------------------------------------------------------------------

// genFunction compiles one function: prologue, body, epilogue, fixups.
func (f *funcState) genFunction(fn *ASTNode) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if se, ok := r.(*spassError); ok {
				f.funcErr = se
				// Named return: without this the recovered error is
				// discarded and the caller sees success with a
				// truncated function (EXPR-agent skeleton fix).
				err = se
			} else {
				panic(r)
			}
		}
	}()
	f.name = fn.Name
	retTy, ok := ctypeOf(fn.ReturnType, f.sp)
	if !ok {
		// Unknown return spelling: treat bare/missing as int (matches the
		// default backend's leniency for implicit-int relics).
		retTy = CType{Name: "int", Size: 4, Signed: true}
	}
	f.retTy = retTy
	f.sret = isLargeStructSp(retTy)
	// Parameters: fn.Params names + fn.ParamTypes spellings.
	if len(fn.Params) != len(fn.ParamTypes) {
		return fmt.Errorf("param/paramtype count mismatch")
	}
	f.params = make([]LocalVar, len(fn.Params))
	for i, pname := range fn.Params {
		pty, ok := ctypeOf(fn.ParamTypes[i], f.sp)
		if !ok {
			return fmt.Errorf("unknown param type %q", fn.ParamTypes[i])
		}
		if pty.IsVoid {
			continue
		}
		f.params[i] = LocalVar{Name: pname, Type: pty}
	}
	f.retLabel = f.localLabel("ret")
	f.emitPrologue()
	// Body: all children (NodeBlock or bare statement list).
	for _, st := range fn.Children {
		if err := f.genStmt(st); err != nil {
			return err
		}
	}
	f.emitEpilogue()
	f.patchFunction()
	// Function symbol (offset fixed at merge; size = text length).
	f.syms = append(f.syms, ObjSymbol{Name: f.name, Section: ".text", Offset: 0, Size: uint64(len(f.text))})
	return f.funcErr
}

// patchFunction resolves local jump fixups. O0: single pass (all near).
// O1: iterate shrinking near jumps to short form when they fit.
func (f *funcState) patchFunction() {
	if f.sp.oLevel >= 1 {
		f.patchFunctionO1()
		return
	}
	for _, fx := range f.fixups {
		if fx.kind != fixJump {
			continue
		}
		tgt, ok := f.labels[fx.label]
		if !ok {
			panic(fmt.Sprintf("spass: undefined local label %s", fx.label))
		}
		disp := int64(tgt) - int64(fx.pos+4)
		f.patchU32(fx.pos, uint32(int32(disp)))
	}
}

// patchFunctionO1 shrinks near jumps to short form when the displacement
// fits int8. Jump sizes only shrink, so label distances monotonically
// decrease and a few rounds converge.
func (f *funcState) patchFunctionO1() {
	// short[i] tracks whether fixup i is currently short (starts as
	// emitted: O1 emits short placeholders).
	short := make([]bool, len(f.fixups))
	for i := range f.fixups {
		short[i] = f.fixups[i].isJcc || true // emitted short at O1
		_ = short
		break
	}
	_ = short
	// Simpler correct approach: recompute from the emitted (short) forms.
	// Since O1 emits short placeholders, expansion is handled inline here
	// by rewriting to near when out of range (positions shift; fixups after
	// the expansion point are adjusted).
	type item struct {
		idx int
		pos uint64
	}
	// Work in emission order (fixups appended in order = ascending pos).
	var shift int64
	for i := range f.fixups {
		fx := &f.fixups[i]
		if fx.kind != fixJump {
			continue
		}
		tgt := int64(f.labels[fx.label])
		// Label positions shift only by expansions before them; track by
		// adjusting recorded label offsets lazily is complex, so recompute:
		// maintain a shift table is overkill — instead note expansions only
		// move bytes AFTER opPos; compute adjusted target by replaying.
		_ = tgt
		_ = shift
	}
	// Correctness-first O1: fall back to near-form patching (identical to
	// O0) when any short jump would overflow. Implemented by checking fit
	// against final positions: first patch all as near-equivalent... but
	// bytes are already short. Expansion path below.
	f.expandShortJumps()
}

// expandShortJumps converts overflowing short jumps to near form, shifting
// subsequent bytes and later fixup positions/labels.
func (f *funcState) expandShortJumps() {
	// Map label -> current offset (mutated by expansions).
	labels := make(map[string]uint64, len(f.labels))
	for k, v := range f.labels {
		labels[k] = v
	}
	var shift int64
	for i := range f.fixups {
		fx := &f.fixups[i]
		if fx.kind != fixJump {
			continue
		}
		pos := int64(fx.pos) + shift
		opPos := int64(fx.opPos) + shift
		tgt := int64(labels[fx.label])
		disp := tgt - (pos + 4) // disp field is 1 byte in short form; +4 = end of NEAR form... careful
		_ = opPos
		// Short form: opcode at opPos (1 byte: EB or 7x), disp8 at opPos+1.
		// disp8 = tgt - (opPos+2).
		disp8 := tgt - (opPos + 2)
		if disp8 >= -128 && disp8 <= 127 {
			f.text[opPos+1] = byte(int8(disp8))
			_ = disp
			_ = pos
			continue
		}
		// Expand to near: rewrite [op][disp8] as [nearOp][disp32].
		var nearOp []byte
		if fx.isJcc {
			nearOp = []byte{0x0F, fx.cond}
		} else {
			nearOp = []byte{0xE9}
		}
		delta := len(nearOp) + 4 - 2 // extra bytes inserted
		// Insert delta zero bytes after the disp8, then overwrite.
		tail := append([]byte(nil), f.text[opPos+2:]...)
		f.text = append(f.text[:opPos+2], make([]byte, delta)...)
		copy(f.text[opPos+2+int64(delta):], tail)
		copy(f.text[opPos:], nearOp)
		newDisp := tgt + shiftTail(labels, fx.label, opPos, int64(delta), shift) - (opPos + int64(len(nearOp)) + 4)
		// Patch disp32 right after nearOp.
		dp := opPos + int64(len(nearOp))
		f.text[dp] = byte(newDisp)
		f.text[dp+1] = byte(newDisp >> 8)
		f.text[dp+2] = byte(newDisp >> 16)
		f.text[dp+3] = byte(newDisp >> 24)
		// Adjust later fixups and labels.
		shift += int64(delta)
		for j := range f.fixups {
			if int64(f.fixups[j].pos) > int64(fx.pos) {
				// positions recorded pre-shift; handled via shift below.
				_ = j
			}
		}
		// Shift labels after opPos.
		for k, v := range labels {
			if int64(v) > opPos {
				labels[k] = v + uint64(delta)
			}
		}
		for j := i + 1; j < len(f.fixups); j++ {
			f.fixups[j].pos += uint64(delta)
			f.fixups[j].opPos += uint64(delta)
		}
	}
	// Commit adjusted label offsets.
	for k, v := range labels {
		f.labels[k] = v
	}
}

// shiftTail is a no-op helper kept for readability of the expansion math.
func shiftTail(labels map[string]uint64, _ string, _ int64, _ int64, _ int64) int64 {
	_ = labels
	return 0
}

// floatBitsOf parses a C floating literal to IEEE-754 bits + float-ness.
func floatBitsOf(imm string, ty CType) (uint64, bool) {
	isF := ty.Size == 4
	if isFloatSpelling(imm) && (strings.HasSuffix(imm, "f") || strings.HasSuffix(imm, "F")) {
		isF = true
	}
	if isF {
		return uint64(floatBits32(imm)), true
	}
	return doubleBits64(imm), false
}

// spassUnescape decodes C string-literal escapes to raw bytes (for .SPS
// interning). Handles the escapes the ahoy front-end emits.
func spassUnescape(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 1)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		case '\\':
			b.WriteByte('\\')
		case '\'':
			b.WriteByte('\'')
		case '"':
			b.WriteByte('"')
		case 'x':
			// \xHH hex escape.
			v := 0
			n := 0
			for j := i + 1; j < len(s) && n < 2; j++ {
				h := s[j]
				var d int
				switch {
				case h >= '0' && h <= '9':
					d = int(h - '0')
				case h >= 'a' && h <= 'f':
					d = int(h-'a') + 10
				case h >= 'A' && h <= 'F':
					d = int(h-'A') + 10
				default:
					d = -1
				}
				if d < 0 {
					break
				}
				v = v*16 + d
				n++
				i = j
			}
			b.WriteByte(byte(v))
		default:
			// Octal \ooo (up to 3 digits).
			if s[i] >= '0' && s[i] <= '7' {
				v := int(s[i] - '0')
				n := 1
				for j := i + 1; j < len(s) && n < 3; j++ {
					if s[j] < '0' || s[j] > '7' {
						break
					}
					v = v*8 + int(s[j]-'0')
					n++
					i = j
				}
				b.WriteByte(byte(v))
			} else {
				b.WriteByte(s[i])
			}
		}
	}
	return b.String()
}

// genDataInit is implemented in spass_mem.go (MEM agent).

// ---------------------------------------------------------------------------
// Truthiness + conditional branches (shared by STMT/EXPR)
// ---------------------------------------------------------------------------

// jcc condition codes (second opcode byte after 0F).
const (
	ccE  = 0x84
	ccNE = 0x85
	ccB  = 0x82
	ccBE = 0x86
	ccAE = 0x83
	ccA  = 0x87
	ccL  = 0x8C
	ccLE = 0x8E
	ccG  = 0x8F
	ccGE = 0x8D
)

// cmpCond maps a C comparison operator to (jcc code, unsigned).
func cmpCond(op string, unsigned bool) byte {
	switch op {
	case "==":
		return ccE
	case "!=":
		return ccNE
	case "<":
		if unsigned {
			return ccB
		}
		return ccL
	case "<=":
		if unsigned {
			return ccBE
		}
		return ccLE
	case ">":
		if unsigned {
			return ccA
		}
		return ccG
	case ">=":
		if unsigned {
			return ccAE
		}
		return ccGE
	}
	panic("spass: bad comparison " + op)
}

// truthy reduces any scalar SVal to 0/1 in an integer register.
func (f *funcState) truthy(v SVal) SVal {
	v = f.materialize(v)
	r := f.scratchReg()
	one := CType{Name: "int", Size: 4, Signed: true}
	switch v.Kind {
	case KReg:
		f.encTestRegReg(v.Reg, v.Reg)
		f.encSetcc(0x95, r)
		f.encMovExtend(r, r, 1, 4, false)
		return SVal{Kind: KReg, Type: one, Reg: r}
	case KXMM:
		// Unordered (NaN) is false: jp skips the set, leaving 0.
		// Compare against +0.0 (self-compare would always report equal).
		f.encMovRegImm(r, "0", one)
		z := f.scratchXMM()
		f.encSSE(0x66, 0x57, z, z)
		f.encUcomisd(v.Reg, z)
		done := f.localLabel("nan")
		f.encJcc(0x8A, done) // jp (0F 8A): NaN -> stays 0
		f.encSetcc(0x95, r)
		f.defineLabel(done)
		f.encMovExtend(r, r, 1, 4, false)
		return SVal{Kind: KReg, Type: one, Reg: r}
	}
	panic("spass: truthy of " + v.Type.Name)
}

// branchOn emits a conditional jump to label: taken iff (v != 0) ==
// wantTrue. Integers/pointers use test+jcc; floats use ucomisd with
// explicit NaN handling (unordered counts as false).
func (f *funcState) branchOn(v SVal, wantTrue bool, label string) {
	v = f.materialize(v)
	switch v.Kind {
	case KReg:
		f.encTestRegReg(v.Reg, v.Reg)
		if wantTrue {
			f.encJcc(ccNE, label)
		} else {
			f.encJcc(ccE, label)
		}
	case KXMM:
		// Float truthiness compares against +0.0 (a self-compare would
		// always report equal). NaN counts as false (unordered -> jump).
		// (EXPR-agent skeleton fix: the old self-compare made every
		// ordered float test false.)
		z := f.scratchXMM()
		f.encSSE(0x66, 0x57, z, z)
		f.encUcomisd(v.Reg, z)
		if wantTrue {
			// NaN -> fall through (false); zero -> fall through.
			next := f.localLabel("ord")
			f.encJcc(0x8A, next) // jp: unordered -> not-true
			f.encJcc(ccNE, label)
			f.defineLabel(next)
		} else {
			// NaN or zero -> jump (false).
			f.encJcc(0x8A, label) // jp
			f.encJcc(ccE, label)
		}
	default:
		panic("spass: branch on " + v.Type.Name)
	}
}
