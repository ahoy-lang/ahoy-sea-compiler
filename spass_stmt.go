// spass_stmt.go: statement codegen for the single-pass backend (STMT agent).
//
// Walks parser AST statement nodes and emits machine code directly via the
// structured encoder (spass_encode.go). Expression evaluation is delegated
// to EXPR (genRVal), conversions to MEM (convert); this file owns control
// flow, declarations, returns, and switch dispatch.
//
// O0 spill discipline: every statement boundary calls spillAll() (currently
// a no-op hook; the calls mark the choke points for a future -O1).
package main

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Entry point (replaces the stub in spass.go; signature is frozen)
// ---------------------------------------------------------------------------

// genStmt compiles one statement.
func (f *funcState) genStmt(n *ASTNode) error {
	if n == nil {
		return nil
	}
	switch n.Type {
	case NodeBlock:
		for _, st := range n.Children {
			if err := f.genStmt(st); err != nil {
				return err
			}
		}
		return nil
	case NodeVarDecl:
		return f.spStmtVarDecl(n)
	case NodeExprStmt:
		if len(n.Children) > 0 {
			f.genRVal(n.Children[0])
		}
		f.spillAll()
		return nil
	case NodeCompoundLiteral:
		// Expression statement consisting of a bare compound literal,
		// e.g. `(struct P){1, 2};` — evaluate and discard.
		f.genRVal(n)
		f.spillAll()
		return nil
	case NodeIf:
		return f.spStmtIf(n)
	case NodeWhile:
		return f.spStmtWhile(n)
	case NodeDoWhile:
		return f.spStmtDoWhile(n)
	case NodeFor:
		return f.spStmtFor(n)
	case NodeReturn:
		return f.spStmtReturn(n)
	case NodeBreak:
		if len(f.loopStack) == 0 {
			return unsupported("break outside loop or switch", n)
		}
		f.encJmp(f.loopStack[len(f.loopStack)-1].breakLabel)
		return nil
	case NodeContinue:
		for i := len(f.loopStack) - 1; i >= 0; i-- {
			if !f.loopStack[i].isSwitch {
				f.encJmp(f.loopStack[i].continueLabel)
				return nil
			}
		}
		return unsupported("continue outside loop", n)
	case NodeSwitch:
		return f.spStmtSwitch(n)
	case NodeCase:
		// Case nodes are consumed by the switch driver; reaching one
		// here means it is misplaced. (The parser represents `default:`
		// as a NodeCase with Value "default", so there is no NodeDefault.)
		return unsupported("case outside switch", n)
	case NodeGoto:
		f.encJmp(n.Value)
		return nil
	case NodeLabel:
		f.defineLabel(n.Value)
		return nil
	default:
		return unsupported(fmt.Sprintf("statement type %d", n.Type), n)
	}
}

// ---------------------------------------------------------------------------
// Declarations
// ---------------------------------------------------------------------------

// spStmtVarDecl handles a function-scope variable declaration. Frame slots
// are unique per declare() call (no slot reuse), so shadowing simply
// rebinds the name in f.locals — the outer binding is lost after the inner
// scope ends. This matches the default backend (localVars map, no
// save/restore on block exit); nested shadowing is a known limitation in
// both backends.
func (f *funcState) spStmtVarDecl(n *ASTNode) error {
	if n.IsGlobal {
		return unsupported("global declaration inside function", n)
	}
	ty, ok := ctypeOf(n.DataType, f.sp)
	if !ok {
		return unsupported("unknown type "+n.DataType, n)
	}
	// Array declarator: element type in DataType, total element count in
	// ArraySize. Slot = count * element size; the LocalVar keeps the
	// element type (matches the default backend's Symbol.Type).
	total := 0
	if n.ArraySize > 0 {
		total = n.ArraySize * ty.Size
	} else if len(n.Dims) == 0 && len(n.Children) > 0 &&
		n.Children[0].Type == NodeString && ty.Size == 1 {
		// `char s[] = "..."`: the parser only infers array sizes for
		// brace initializers, so infer from the string (bytes + NUL).
		// (The default backend treats this as a scalar and miscompiles
		// it; doing the C-correct thing here.)
		total = len(spassUnescape(spStmtStripQuotes(n.Children[0].Value))) + 1
	}
	if total > 0 {
		off := f.allocSlot(total)
		lv := &LocalVar{Name: n.VarName, Type: ty, Offset: off}
		f.locals[n.VarName] = lv
		// MEM's dims side table (multi-dim index lowering): record the
		// declared dimensions right after allocating the slot.
		dims := n.Dims
		if len(dims) == 0 {
			dims = []int{total / ty.Size}
		}
		MemNoteArray(f, n.VarName, ty, dims)
		if len(n.Children) > 0 {
			if err := f.spStmtInitArray(lv, n.Children[0], ty, total); err != nil {
				return err
			}
		}
		f.spillAll()
		return nil
	}
	lv := f.declare(n.VarName, ty)
	if len(n.Children) > 0 {
		init := n.Children[0]
		v := f.genRVal(init)
		v = f.spStmtMaybeConvert(v, ty)
		dst := Addr{RegBase: REG_RBP, Disp: lv.Offset, Index: -1, Scale: 1, Size: ty.Size}
		f.spStmtStore(dst, v, ty)
	}
	f.spillAll()
	return nil
}

// spStmtInitArray initializes a local array slot. C semantics: missing
// elements are zero (hence the whole slot is zeroed first, then the given
// elements are stored). String literals initialize char arrays (with NUL,
// truncated/padded to the slot size).
func (f *funcState) spStmtInitArray(lv *LocalVar, init *ASTNode, elemTy CType, total int) error {
	dst := Addr{RegBase: REG_RBP, Disp: lv.Offset, Index: -1, Scale: 1, Size: 8}
	// String literal into a byte-element array: copy bytes + NUL.
	if init.Type == NodeString && elemTy.Size == 1 {
		cooked := spassUnescape(spStmtStripQuotes(init.Value))
		lab := f.sp.strLit(cooked)
		n := len(cooked) + 1 // include NUL
		if n > total {
			n = total
		}
		f.spStmtCopy(dst, Addr{Sym: lab, RegBase: -1, Disp: 0, Index: -1, Scale: 1, Size: 8}, n)
		if n < total {
			f.spStmtZero(Addr{RegBase: REG_RBP, Disp: lv.Offset + n, Index: -1, Scale: 1, Size: 8}, total-n)
		}
		return nil
	}
	if init.Type != NodeCompoundLiteral {
		// Not valid C (e.g. `int a[3] = 5;`); evaluate for side effects.
		f.genRVal(init)
		return nil
	}
	for _, fld := range init.InitFields {
		if fld != "" {
			return unsupported("designated array initializer", init)
		}
	}
	f.spStmtZero(dst, total)
	elemSize := elemTy.Size
	for i, e := range init.Children {
		if i*elemSize >= total {
			break
		}
		v := f.genRVal(e)
		v = f.spStmtMaybeConvert(v, elemTy)
		f.spStmtStore(Addr{RegBase: REG_RBP, Disp: lv.Offset + i*elemSize, Index: -1, Scale: 1, Size: elemTy.Size}, v, elemTy)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Selection and iteration
// ---------------------------------------------------------------------------

func (f *funcState) spStmtIf(n *ASTNode) error {
	if len(n.Children) < 2 {
		return unsupported("malformed if", n)
	}
	cond := f.genRVal(n.Children[0])
	if len(n.Children) > 2 {
		elseLabel := f.localLabel("if_else")
		endLabel := f.localLabel("if_end")
		f.branchOn(cond, false, elseLabel)
		if err := f.genStmt(n.Children[1]); err != nil {
			return err
		}
		f.encJmp(endLabel)
		f.defineLabel(elseLabel)
		if err := f.genStmt(n.Children[2]); err != nil {
			return err
		}
		f.defineLabel(endLabel)
	} else {
		endLabel := f.localLabel("if_end")
		f.branchOn(cond, false, endLabel)
		if err := f.genStmt(n.Children[1]); err != nil {
			return err
		}
		f.defineLabel(endLabel)
	}
	f.spillAll()
	return nil
}

func (f *funcState) spStmtWhile(n *ASTNode) error {
	if len(n.Children) < 2 {
		return unsupported("malformed while", n)
	}
	topLabel := f.localLabel("while_top")
	endLabel := f.localLabel("while_end")
	f.defineLabel(topLabel)
	cond := f.genRVal(n.Children[0])
	f.branchOn(cond, false, endLabel)
	f.loopStack = append(f.loopStack, loopCtx{breakLabel: endLabel, continueLabel: topLabel})
	berr := f.genStmt(n.Children[1])
	f.loopStack = f.loopStack[:len(f.loopStack)-1]
	if berr != nil {
		return berr
	}
	f.encJmp(topLabel)
	f.defineLabel(endLabel)
	f.spillAll()
	return nil
}

func (f *funcState) spStmtDoWhile(n *ASTNode) error {
	if len(n.Children) < 2 {
		return unsupported("malformed do-while", n)
	}
	bodyLabel := f.localLabel("do_body")
	condLabel := f.localLabel("do_cond")
	endLabel := f.localLabel("do_end")
	f.defineLabel(bodyLabel)
	f.loopStack = append(f.loopStack, loopCtx{breakLabel: endLabel, continueLabel: condLabel})
	berr := f.genStmt(n.Children[0])
	f.loopStack = f.loopStack[:len(f.loopStack)-1]
	if berr != nil {
		return berr
	}
	f.defineLabel(condLabel)
	cond := f.genRVal(n.Children[1])
	f.branchOn(cond, true, bodyLabel)
	f.defineLabel(endLabel)
	f.spillAll()
	return nil
}

func (f *funcState) spStmtFor(n *ASTNode) error {
	init, cond, incr, body := n.ForInit, n.ForCond, n.ForIncr, n.ForBody
	if body == nil && len(n.Children) > 0 {
		body = n.Children[len(n.Children)-1]
	}
	// Initializer: declaration(s) via genStmt, bare expression via
	// genRVal + discard.
	if init != nil {
		switch init.Type {
		case NodeVarDecl, NodeBlock, NodeExprStmt:
			if err := f.genStmt(init); err != nil {
				return err
			}
		default:
			f.genRVal(init)
			f.spillAll()
		}
	}
	startLabel := f.localLabel("for_start")
	contLabel := f.localLabel("for_cont")
	endLabel := f.localLabel("for_end")
	f.defineLabel(startLabel)
	if cond != nil {
		cv := f.genRVal(cond)
		f.branchOn(cv, false, endLabel)
	}
	f.loopStack = append(f.loopStack, loopCtx{breakLabel: endLabel, continueLabel: contLabel})
	var berr error
	if body != nil {
		berr = f.genStmt(body)
	}
	f.loopStack = f.loopStack[:len(f.loopStack)-1]
	if berr != nil {
		return berr
	}
	f.defineLabel(contLabel)
	if incr != nil {
		f.genRVal(incr)
		f.spillAll()
	}
	f.encJmp(startLabel)
	f.defineLabel(endLabel)
	f.spillAll()
	return nil
}

// ---------------------------------------------------------------------------
// Switch (compare-chain dispatch, natural fallthrough)
// ---------------------------------------------------------------------------

// spStmtSwitch evaluates the condition once into a frame slot, emits a
// compare-and-jump-to-case-label chain (plus a jump to default/end), then
// emits the case bodies in order so control falls through naturally.
// `break` exits via the pushed loopCtx (isSwitch, so an enclosing loop's
// `continue` still finds its own target).
func (f *funcState) spStmtSwitch(n *ASTNode) error {
	if len(n.Children) < 1 {
		return unsupported("switch without expression", n)
	}
	cv := f.genRVal(n.Children[0])
	m := f.materialize(cv)
	if m.Kind != KReg && m.Kind != KXMM {
		return unsupported("switch on non-scalar", n)
	}
	condTy := m.Type
	condSlot := f.tempSlot(condTy.Size)
	condSlot.Size = condTy.Size
	f.spStmtStore(condSlot, m, condTy)

	type caseInfo struct {
		label string
		value *ASTNode // nil for default
		body  []*ASTNode
	}
	var cases []caseInfo
	var defaultBody []*ASTNode
	var defaultLabel string
	hasDefault := false
	for _, c := range n.Children[1:] {
		if c == nil || c.Type != NodeCase {
			continue
		}
		if c.Value == "default" {
			hasDefault = true
			defaultLabel = f.localLabel("switch_default")
			defaultBody = c.Children
			continue
		}
		if len(c.Children) < 1 {
			continue
		}
		cases = append(cases, caseInfo{
			label: f.localLabel("switch_case"),
			value: c.Children[0],
			body:  c.Children[1:],
		})
	}
	endLabel := f.localLabel("switch_end")

	// Dispatch chain.
	for _, c := range cases {
		kv := f.genRVal(c.value)
		km := f.materialize(kv)
		f.spStmtCmpGoto(condSlot, condTy, km, c.label)
	}
	if hasDefault {
		f.encJmp(defaultLabel)
	} else {
		f.encJmp(endLabel)
	}

	f.loopStack = append(f.loopStack, loopCtx{breakLabel: endLabel, isSwitch: true})
	var serr error
	for _, c := range cases {
		f.defineLabel(c.label)
		for _, st := range c.body {
			if st == nil {
				continue
			}
			if err := f.genStmt(st); err != nil {
				serr = err
				break
			}
		}
		if serr != nil {
			break
		}
	}
	if serr == nil && hasDefault {
		f.defineLabel(defaultLabel)
		for _, st := range defaultBody {
			if st == nil {
				continue
			}
			if err := f.genStmt(st); err != nil {
				serr = err
				break
			}
		}
	}
	f.loopStack = f.loopStack[:len(f.loopStack)-1]
	if serr != nil {
		return serr
	}
	f.defineLabel(endLabel)
	f.spillAll()
	return nil
}

// spStmtCmpGoto emits "if (condSlot == kv) goto label" for an integer or
// float switch condition. Integer comparison is full-width 64-bit: both
// sides are width-correct (loads sign/zero-extend, immediates arrive via
// movl/movabs), matching C promotion of the compared values.
func (f *funcState) spStmtCmpGoto(condSlot Addr, condTy CType, kv SVal, label string) {
	if condTy.IsFloat {
		xc := f.scratchXMM()
		ld := condSlot
		ld.Size = condTy.Size
		f.encMovXMMMem(xc, ld, true)
		var xk int
		if kv.Kind == KXMM {
			xk = kv.Reg
		} else {
			xk = f.scratchXMM()
			f.encIntToXMM(xk, kv)
		}
		f.encUcomisd(xc, xk)
		skip := f.localLabel("sw_nan")
		f.encJcc(0x8A, skip) // jp (0F 8A): unordered (NaN) never equals
		f.encJcc(ccE, label)
		f.defineLabel(skip)
		return
	}
	k := kv
	if k.Kind != KReg {
		k = f.materialize(kv)
	}
	if k.Kind != KReg {
		panic(unsupported("switch case value", nil))
	}
	// Cond load: byte loads must avoid regs 4-7 (see spStmtByteReg);
	// keep clear of a live case-value register.
	r := f.scratchReg()
	ld := condSlot
	ld.Size = condTy.Size
	if condTy.Size == 1 {
		r = spStmtByteReg
		if k.Reg == spStmtByteReg {
			r = spStmtByteReg2
		}
	}
	f.encMovRegMem(r, ld, condTy)
	f.encALURegReg("cmp", r, k.Reg)
	f.encJcc(ccE, label)
}

// ---------------------------------------------------------------------------
// Return
// ---------------------------------------------------------------------------

func (f *funcState) spStmtReturn(n *ASTNode) error {
	if len(n.Children) == 0 {
		f.encJmp(f.retLabel)
		f.spillAll()
		return nil
	}
	v := f.genRVal(n.Children[0])
	v = f.spStmtMaybeConvert(v, f.retTy)
	switch {
	case f.sret:
		// Large struct: copy the value to the hidden return buffer.
		// The epilogue loads rax from the buffer; nothing more needed.
		if v.Kind != KMem {
			return unsupported("large struct return value", n)
		}
		f.spStmtCopySret(v.Mem, f.retTy.Size)
	case f.retTy.IsVoid:
		// `return <void-expr>;`: already evaluated, nothing to move.
	case f.retTy.IsFloat:
		m := f.materialize(v)
		if m.Kind != KXMM {
			return unsupported("float return value", n)
		}
		if m.Reg != 0 {
			f.encMovXMMReg(0, m.Reg)
		}
	default:
		m := f.materialize(v)
		if m.Kind != KReg {
			return unsupported("integer return value", n)
		}
		if m.Reg != REG_RAX {
			f.encMovRegReg(REG_RAX, m.Reg, 8)
		}
	}
	f.encJmp(f.retLabel)
	f.spillAll()
	return nil
}

// ---------------------------------------------------------------------------
// Small helpers (spStmt* prefix; STMT-agent-owned)
// ---------------------------------------------------------------------------

// Fixed registers for byte-width traffic. The frozen encoder omits the REX
// prefix for registers 4-7, which selects AH..DH instead of SPL..DIL on
// 8-bit moves; R10/R11 (like all regs >= 8) always encode correctly. They
// are used only where no value spans a genRVal call (copy/zero/compare and
// single stores), so bypassing the round-robin scratch dispenser is sound.
const (
	spStmtByteReg  = REG_R10
	spStmtByteReg2 = REG_R11
)

// spStmtMaybeConvert converts v to ty, skipping the call when the value
// already has exactly that type (avoids depending on MEM's convert for the
// common identity case).
func (f *funcState) spStmtMaybeConvert(v SVal, to CType) SVal {
	if v.Kind != KVoid && v.Type == to {
		return v
	}
	return f.convert(v, to)
}

// spStmtStore writes v into a frame slot of declared type ty. Aggregates
// (>8 bytes) are byte-copied from the KMem source; small struct values
// already in a register (e.g. a <=8-byte struct call result in rax) are
// stored directly. Byte stores are bounced through R10 (see spStmtByteReg).
func (f *funcState) spStmtStore(dst Addr, v SVal, ty CType) {
	switch ty.Size {
	case 1:
		dst.Size = 1
		if v.Kind == KConst {
			f.store(dst, v)
			return
		}
		if v.Kind != KReg && v.Kind != KMem {
			panic(unsupported("byte value not in memory/register", nil))
		}
		// Byte traffic pins a fixed register, but never one that dst
		// itself uses as base/index: reusing it would clobber the
		// address being stored through (bell assets_load_texture crash:
		// value and base both ended up in R10).
		vr := spStmtByteReg
		for _, dr := range addrRegs(dst) {
			if dr == vr {
				vr = spStmtByteReg2
				break
			}
		}
		if vr == spStmtByteReg2 {
			for _, dr := range addrRegs(dst) {
				if dr == vr {
					vr = REG_RAX
					break
				}
			}
		}
		if v.Kind == KMem {
			f.encMovRegMem(vr, v.Mem, spStmtChunkTy(1))
		} else if v.Reg != vr {
			f.encMovRegReg(vr, v.Reg, 8)
		}
		bt := v.Type
		bt.Size = 1
		f.encMovMemReg(dst, vr, bt)
		return
	case 2, 4, 8:
		dst.Size = ty.Size
		// A struct-typed value arriving in a register (small struct call
		// result) stores like an integer of the same width.
		if v.Kind == KReg {
			cp := v
			cp.Type = spStmtChunkTy(ty.Size)
			f.store(dst, cp)
			return
		}
		if v.Kind == KMem {
			// The skeleton store() only accepts register/const
			// sources; materialize memory values first.
			v = f.materialize(v)
		}
		f.store(dst, v)
		return
	default:
		if v.Kind != KMem {
			panic(unsupported("aggregate value not in memory", nil))
		}
		f.spStmtCopy(dst, v.Mem, ty.Size)
	}
}

// spStmtCopy copies size bytes from src to dst. Neither address is held in
// a register across chunk iterations (scratch regs are round-robin, so a
// held reg would be clobbered on long copies); Sym bases are re-materialized
// per chunk with LEA since Sym addressing drops displacements.
func (f *funcState) spStmtCopy(dst, src Addr, size int) {
	// Reserve register bases for the whole loop (see reserveScratch):
	// per-chunk scratch picks must never re-issue a live base.
	release := f.reserveRegs(append(addrRegs(dst), addrRegs(src)...)...)
	defer release()
	off := 0
	for off < size {
		chunk := 8
		for chunk > 1 && size-off < chunk {
			chunk /= 2
		}
		sa := src
		if sa.Sym != "" {
			rb := f.scratchReg()
			if chunk == 1 {
				rb = spStmtByteReg2
			}
			f.encLeaRegMem(rb, Addr{Sym: sa.Sym, RegBase: -1, Disp: 0, Index: -1, Scale: 1, Size: 8})
			sa = Addr{RegBase: rb, Disp: off, Index: -1, Scale: 1, Size: chunk}
		} else {
			sa.Disp += off
			sa.Size = chunk
		}
		da := dst
		if da.Sym != "" {
			rb := f.scratchReg()
			if chunk == 1 {
				rb = spStmtByteReg2
			}
			f.encLeaRegMem(rb, Addr{Sym: da.Sym, RegBase: -1, Disp: 0, Index: -1, Scale: 1, Size: 8})
			da = Addr{RegBase: rb, Disp: off, Index: -1, Scale: 1, Size: chunk}
		} else {
			da.Disp += off
			da.Size = chunk
		}
		// Byte traffic uses a fixed high register (see spStmtByteReg);
		// wider chunks use the scratch dispenser.
		r := f.scratchReg()
		if chunk == 1 {
			r = spStmtByteReg
		}
		f.encMovRegMem(r, sa, spStmtChunkTy(chunk))
		f.encMovMemReg(da, r, spStmtChunkTy(chunk))
		off += chunk
	}
}

// spStmtCopySret copies a large struct return value (KMem src) into the
// hidden buffer whose pointer lives at f.sretHome. The pointer is reloaded
// from its home slot per chunk so no register is held across iterations.
func (f *funcState) spStmtCopySret(src Addr, size int) {
	ptrTy := CType{Name: "void*", Size: 8, IsPtr: true}
	pslot := f.tempSlot(8)
	pslot.Size = 8
	rp := f.scratchReg()
	f.encMovRegMem(rp, Addr{RegBase: REG_RBP, Disp: f.sretHome, Index: -1, Scale: 1, Size: 8}, ptrTy)
	f.encMovMemReg(pslot, rp, ptrTy)
	off := 0
	for off < size {
		chunk := 8
		for chunk > 1 && size-off < chunk {
			chunk /= 2
		}
		rb := f.scratchReg()
		f.encMovRegMem(rb, pslot, ptrTy)
		sa := src
		if sa.Sym != "" {
			rl := f.scratchReg()
			if chunk == 1 {
				rl = spStmtByteReg2
			}
			f.encLeaRegMem(rl, Addr{Sym: sa.Sym, RegBase: -1, Disp: 0, Index: -1, Scale: 1, Size: 8})
			sa = Addr{RegBase: rl, Disp: off, Index: -1, Scale: 1, Size: chunk}
		} else {
			sa.Disp += off
			sa.Size = chunk
		}
		rv := f.scratchReg()
		if chunk == 1 {
			rv = spStmtByteReg
		}
		f.encMovRegMem(rv, sa, spStmtChunkTy(chunk))
		f.encMovMemReg(Addr{RegBase: rb, Disp: off, Index: -1, Scale: 1, Size: chunk}, rv, spStmtChunkTy(chunk))
		off += chunk
	}
}

// spStmtZero zeroes size bytes at dst (C partial-initializer semantics).
func (f *funcState) spStmtZero(dst Addr, size int) {
	off := 0
	for off < size {
		chunk := 8
		for chunk > 1 && size-off < chunk {
			chunk /= 2
		}
		da := dst
		da.Disp += off
		da.Size = chunk
		f.encMovMemImm(da, "0", spStmtChunkTy(chunk))
		off += chunk
	}
}

// spStmtChunkTy returns a copy-width CType for byte moves (unsigned so
// loads zero-extend; the same width is stored back).
func spStmtChunkTy(size int) CType {
	switch size {
	case 8:
		return CType{Name: "long", Size: 8, Signed: true}
	case 4:
		return CType{Name: "unsigned int", Size: 4}
	case 2:
		return CType{Name: "unsigned short", Size: 2}
	default:
		return CType{Name: "unsigned char", Size: 1}
	}
}

// spStmtStripQuotes removes surrounding double quotes if present. Lexer
// STRING lexemes already exclude quotes, but this keeps the helper robust
// to both shapes.
func spStmtStripQuotes(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") {
		return s[1 : len(s)-1]
	}
	return s
}
