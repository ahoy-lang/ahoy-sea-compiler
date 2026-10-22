// spass_encode: structured x86-64 byte encoder for the single-pass backend.
//
// Unlike fastasm (which parses assembly *text*), every function here takes
// analyzed operands, so there is no re-parse: REX/ModRM/SIB are computed
// directly. Fixups (forward jumps, RIP refs, TU-local calls) are recorded
// in funcState and patched at function end (locals) or TU merge (globals).
//
// Codegen emits a canonical subset only (we control the generator).
package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Labels and fixups
// ---------------------------------------------------------------------------

// encLabel defines a local text label at the current offset. Global
// function symbols are registered separately (see genFunction).
func (f *funcState) encLabel(name string) {
	f.labels[name] = uint64(len(f.text))
}

// defineLabel is an alias used by control-flow codegen.
func (f *funcState) defineLabel(name string) {
	f.encLabel(name)
}

// addFixup records a 32-bit field at pos to be patched.
func (f *funcState) addFixup(pos uint64, label, sym string, kind int, opPos uint64, isJcc bool, cond byte) {
	f.fixups = append(f.fixups, spFixup{
		pos: pos, label: label, sym: sym, kind: kind,
		opPos: opPos, isJcc: isJcc, cond: cond,
	})
}

// patchU32 writes a little-endian uint32 at an absolute text position.
func (f *funcState) patchU32(pos uint64, val uint32) {
	f.text[pos] = byte(val)
	f.text[pos+1] = byte(val >> 8)
	f.text[pos+2] = byte(val >> 16)
	f.text[pos+3] = byte(val >> 24)
}

// ---------------------------------------------------------------------------
// REX / ModRM construction (single place, used by everything below)
// ---------------------------------------------------------------------------

// regField returns the 3-bit ModRM field and the REX extension bit.
func regField(reg int) (field byte, ext bool) {
	return byte(reg & 7), reg > 7
}

// rexByte builds a REX byte 0100WRXB, or nil when no REX is needed.
func rexByte(w, r, x, b bool) []byte {
	if !w && !r && !x && !b {
		return nil
	}
	v := byte(0x40)
	if w {
		v |= 0x08
	}
	if r {
		v |= 0x04
	}
	if x {
		v |= 0x02
	}
	if b {
		v |= 0x01
	}
	return []byte{v}
}

// encRM emits [legacy-prefix] [REX] OPCODE... ModRM [SIB] [disp] for a
// reg+Addr pair. regOp is the ModRM reg field with rExt as its REX.R bit.
// w selects 64-bit operand size. When a.Sym != "", RIP-relative mode is
// used and a merge-time fixup is recorded (symKey identifies the target).
func (f *funcState) encRM(forceRex bool, legacy byte, opcodes []byte, w bool, reg int, a Addr, symKey string) {
	regOp, rExt := regField(reg)
	if legacy != 0 {
		f.text = append(f.text, legacy)
	}
	base, bExt := regField(a.RegBase)
	idxExt := false
	var idx byte = 4
	if a.Index >= 0 {
		idx, idxExt = regField(a.Index)
	}
	// forceRex selects low-byte regs (spl/bpl/sil/dil, codes 4-7): without
	// any REX the CPU reads ah/bh/ch/dh instead. A bare 0x40 (all REX
	// bits clear) suffices; callers pass it exactly when the ModRM reg
	// operand is an unextended 4-7 code in an 8-bit move. (Setting REX.R
	// instead would select r15b etc. — the R bit adds 8 to the field.)
	rb := rexByte(w, rExt, idxExt, bExt)
	if forceRex && rb == nil {
		rb = []byte{0x40}
	}
	if a.Sym != "" {
		f.text = append(f.text, rb...)
		f.text = append(f.text, opcodes...)
		// ModRM: mod=00 reg=regOp r/m=101.
		f.text = append(f.text, (regOp<<3)|0x05)
		pos := uint64(len(f.text))
		f.text = append(f.text, 0, 0, 0, 0)
		f.roFixups = append(f.roFixups, spRoFix{pos: pos, key: symKey})
		return
	}
	needSIB := a.Index >= 0 || base == 4
	disp := a.Disp
	var mod byte
	switch {
	case disp == 0 && base != 5:
		mod = 0
	case disp >= -128 && disp <= 127:
		mod = 1
	default:
		mod = 2
	}
	if base == 5 && mod == 0 {
		mod = 1
	}
	f.text = append(f.text, rb...)
	f.text = append(f.text, opcodes...)
	rm := base
	if needSIB {
		rm = 4
	}
	f.text = append(f.text, (mod<<6)|(regOp<<3)|rm)
	if needSIB {
		var sc byte
		switch a.Scale {
		case 2:
			sc = 1
		case 4:
			sc = 2
		case 8:
			sc = 3
		}
		ib := idx
		if a.Index < 0 {
			ib = 4
		}
		f.text = append(f.text, (sc<<6)|(ib<<3)|base)
	}
	switch mod {
	case 1:
		f.text = append(f.text, byte(disp))
	case 2:
		f.text = faAppendU32(f.text, uint64(uint32(int32(disp))))
	}
}

// encRR emits [REX] OPCODE ModRM for two GPRs. reg feeds the ModRM reg
// field (REX.R), rm feeds the r/m field (REX.B). For 0x89-style opcodes
// (mov r/m, r; add r/m, r; ...) reg=src, rm=dst.
func (f *funcState) encRR(opcode byte, w bool, reg, rm int) {
	rf, re := regField(reg)
	mf, me := regField(rm)
	f.text = append(f.text, rexByte(w, re, false, me)...)
	f.text = append(f.text, opcode, 0xC0|(rf<<3)|mf)
}

// ---------------------------------------------------------------------------
// Jumps, calls, returns (fixup-based; local labels patched at func end)
// ---------------------------------------------------------------------------

// encJmp emits an unconditional jump. O0: always near (E9).
func (f *funcState) encJmp(label string) {
	opPos := uint64(len(f.text))
	if f.sp.oLevel >= 1 {
		// Emit short placeholder; the O1 fixup pass widens if needed.
		f.text = append(f.text, 0xEB, 0x00)
		f.addFixup(opPos+1, label, "", fixJump, opPos, false, 0)
		return
	}
	f.text = append(f.text, 0xE9, 0, 0, 0, 0)
	f.addFixup(opPos+1, label, "", fixJump, opPos, false, 0)
}

// encJcc emits a conditional jump. cond is the 0x8x second opcode byte
// (e.g. 0x84 for e, 0x8C for l). O0: always near (0F 8x).
func (f *funcState) encJcc(cond byte, label string) {
	opPos := uint64(len(f.text))
	if f.sp.oLevel >= 1 {
		// Short form: 7x + disp8.
		f.text = append(f.text, 0x70|(cond&0x0F), 0x00)
		f.addFixup(opPos+1, label, "", fixJump, opPos, true, cond)
		return
	}
	f.text = append(f.text, 0x0F, cond, 0, 0, 0, 0)
	f.addFixup(opPos+2, label, "", fixJump, opPos, true, cond)
}

// ---------------------------------------------------------------------------
// Integer moves
// ---------------------------------------------------------------------------

// encMovRegReg emits mov dst, src (GPR/GPR, width 8 or 4).
func (f *funcState) encMovRegReg(dst, src, width int) {
	if width == 4 {
		f.encRR(0x89, false, src, dst)
		return
	}
	f.encRR(0x89, true, src, dst)
}

// encMovRegMem emits mov dstReg, [addr]; width/sign from ty (movslq for
// signed 32-bit, matching the default backend's width-correct loads).
func (f *funcState) encMovRegMem(dst int, a Addr, ty CType) {
	symKey := dataKeyOf(a)
	switch ty.Size {
	case 8:
		f.encRM(false, 0, []byte{0x8B}, true, dst, a, symKey)
	case 4:
		if ty.Signed {
			f.encRM(false, 0, []byte{0x63}, true, dst, a, symKey) // movslq
		} else {
			f.encRM(false, 0, []byte{0x8B}, false, dst, a, symKey) // movl (zero-ext)
		}
	case 2:
		if ty.Signed {
			f.encRM(false, 0, []byte{0x0F, 0xBF}, true, dst, a, symKey) // movsxw
		} else {
			f.encRM(false, 0, []byte{0x0F, 0xB7}, true, dst, a, symKey) // movzxw
		}
	default:
		if ty.Signed {
			f.encRM(false, 0, []byte{0x0F, 0xBE}, true, dst, a, symKey) // movsxb
		} else {
			f.encRM(false, 0, []byte{0x0F, 0xB6}, true, dst, a, symKey) // movzxb
		}
	}
}

// encMovMemReg emits mov [addr], srcReg.
func (f *funcState) encMovMemReg(a Addr, src int, ty CType) {
	symKey := dataKeyOf(a)
	switch ty.Size {
	case 8:
		f.encRM(false, 0, []byte{0x89}, true, src, a, symKey)
	case 4:
		f.encRM(false, 0, []byte{0x89}, false, src, a, symKey)
	case 2:
		f.encRM(false, 0x66, []byte{0x89}, false, src, a, symKey)
	default:
		// 8-bit store: force REX for spl/bpl/sil/dil (codes 4-7).
		sc, se := regField(src)
		f.encRM(sc >= 4 && !se, 0, []byte{0x88}, false, src, a, symKey)
	}
}

// dataKeyOf returns the merge-fixup key for a Sym-addressed operand.
// Frame/stack addresses (Sym == "") need no fixup.
func dataKeyOf(a Addr) string {
	return a.Sym
}

// parseIntLit parses a C integer literal spelling (decimal, 0x hex,
// 0-octal, char literal, U/L suffixes) into two's-complement uint64 plus
// whether the SIGNED value fits an imm32 (GAS/C7 selection rule).
func parseIntLit(imm string) (uint64, bool) {
	s := strings.TrimSpace(imm)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	var mag uint64
	var sv int64
	var err error
	unsigned := false
	switch {
	case len(s) >= 3 && s[0] == '\'':
		// Char literal (with simple escapes).
		var cv uint64
		cv, err = parseCharLit(s)
		mag, sv = cv, int64(cv)
	case strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X"):
		mag, err = strconv.ParseUint(s[2:], 16, 64)
		unsigned = true
	case len(s) > 1 && s[0] == '0' && s[1] >= '0' && s[1] <= '7':
		mag, err = strconv.ParseUint(s, 8, 64)
		unsigned = true
	default:
		// Decimal with possible U/L suffixes.
		t := s
		for len(t) > 0 {
			c := t[len(t)-1]
			if c == 'u' || c == 'U' || c == 'l' || c == 'L' {
				t = t[:len(t)-1]
			} else {
				break
			}
		}
		mag, err = strconv.ParseUint(t, 10, 64)
	}
	if err != nil {
		panic(fmt.Sprintf("spass: bad integer literal %q", imm))
	}
	if neg {
		sv = -int64(mag)
		mag = uint64(sv)
	} else if unsigned {
		sv = int64(mag) // wraps for huge values; fits32 false below
	} else {
		sv = int64(mag)
	}
	fits := sv >= -0x80000000 && sv <= 0x7FFFFFFF
	// Unsigned hex/octal literals above INT64_MAX are "positive" values
	// that need movabs (GAS agrees: movq $0xFFF..., %reg is movabs).
	if unsigned && mag > 0x7FFFFFFFFFFFFFFF {
		fits = false
	}
	return mag, fits
}

// parseCharLit folds a C character literal to its value.
func parseCharLit(s string) (uint64, error) {
	// s like 'a', '\n', '\\', '\0'.
	if len(s) < 3 || s[0] != '\'' || s[len(s)-1] != '\'' {
		return 0, fmt.Errorf("bad char literal %q", s)
	}
	inner := s[1 : len(s)-1]
	if len(inner) == 1 {
		return uint64(inner[0]), nil
	}
	if len(inner) == 2 && inner[0] == '\\' {
		switch inner[1] {
		case 'n':
			return 10, nil
		case 't':
			return 9, nil
		case 'r':
			return 13, nil
		case '0':
			return 0, nil
		case '\\':
			return 92, nil
		case '\'':
			return 39, nil
		case '"':
			return 34, nil
		}
	}
	return 0, fmt.Errorf("bad char literal %q", s)
}

// encMovRegImm emits mov reg, imm (C literal spelling, integer).
func (f *funcState) encMovRegImm(reg int, imm string, ty CType) {
	v, fits32 := parseIntLit(imm)
	rf, re := regField(reg)
	if ty.Size == 8 && !fits32 {
		// Doesn't fit a sign-extended imm32: movabs.
		f.text = append(f.text, rexByte(true, false, false, re)...)
		f.text = append(f.text, 0xB8+rf)
		f.text = faAppendU64(f.text, v)
		return
	}
	if ty.Size == 4 {
		// movl (zero-extends into the 64-bit reg, matching movl semantics).
		f.text = append(f.text, rexByte(false, false, false, re)...)
		f.text = append(f.text, 0xB8+rf)
		f.text = faAppendU32(f.text, v)
		return
	}
	// C7 /0 io with sign-extended imm32.
	f.text = append(f.text, rexByte(true, false, false, re)...)
	f.text = append(f.text, 0xC7, 0xC0|rf)
	f.text = faAppendU32(f.text, v)
}

// encMovMemImm emits mov [addr], imm32 (C7 /0). Wide constants must be
// materialized through a register by the caller.
func (f *funcState) encMovMemImm(a Addr, imm string, ty CType) {
	v, fits32 := parseIntLit(imm)
	if ty.Size == 8 && !fits32 {
		panic("spass: wide immediate to memory must go via register")
	}
	// The disp32 is NOT the last field here (the immediate follows), so a
	// Sym fixup's addend must also cover the trailing immediate bytes.
	// (EXPR-agent encoder fix: the old code left Addend at -4, storing 4
	// past every RIP-relative destination. The Size-2 path also emitted a
	// 4-byte immediate; movw takes imm16.)
	nf := len(f.roFixups)
	var trail int64
	switch ty.Size {
	case 8:
		f.encRM(false, 0, []byte{0xC7}, true, 0, a, dataKeyOf(a))
		f.text = faAppendU32(f.text, v)
		trail = 4
	case 4:
		f.encRM(false, 0, []byte{0xC7}, false, 0, a, dataKeyOf(a))
		f.text = faAppendU32(f.text, v)
		trail = 4
	case 2:
		f.encRM(false, 0x66, []byte{0xC7}, false, 0, a, dataKeyOf(a))
		f.text = append(f.text, byte(v), byte(v>>8))
		trail = 2
	default:
		f.encRM(false, 0, []byte{0xC6}, false, 0, a, dataKeyOf(a))
		f.text = append(f.text, byte(v))
		trail = 1
	}
	if len(f.roFixups) > nf {
		f.roFixups[len(f.roFixups)-1].add -= trail
	}
}

// encMovExtend emits movzx/movsx dst(32/64), src(8/16) in either width.
func (f *funcState) encMovExtend(dst, src int, fromSize, toSize int, signed bool) {
	// 32-bit sources need movslq (signed) or movl (zero-extending), not
	// the 0F Bx forms (those are 8/16-bit only).
	if fromSize == 4 {
		df, de := regField(dst)
		sf, se := regField(src)
		if signed {
			// movslq dst64, src32.
			f.text = append(f.text, rexByte(true, de, false, se)...)
			f.text = append(f.text, 0x63, 0xC0|(df<<3)|sf)
		} else {
			// movl (upper 32 of dst zeroed by definition).
			f.encRR(0x89, false, src, dst)
		}
		return
	}
	var op2 byte
	if signed {
		if fromSize == 1 {
			op2 = 0xBE
		} else {
			op2 = 0xBF
		}
	} else {
		if fromSize == 1 {
			op2 = 0xB6
		} else {
			op2 = 0xB7
		}
	}
	df, de := regField(dst)
	sf, se := regField(src)
	// 8-bit low regs (spl/bpl/sil/dil) as the r/m source need a bare REX;
	// without it the CPU reads ah/bh/ch/dh. (REX.B must stay clear: setting
	// it would select r14b etc. instead of sil/dil.)
	force := fromSize == 1 && sf >= 4 && !se
	rb := rexByte(toSize == 8, de, false, se)
	if force && rb == nil {
		rb = []byte{0x40}
	}
	f.text = append(f.text, rb...)
	f.text = append(f.text, 0x0F, op2, 0xC0|(df<<3)|sf)
}

// encLeaRegMem emits lea reg, [addr].
func (f *funcState) encLeaRegMem(reg int, a Addr) {
	f.encRM(false, 0, []byte{0x8D}, true, reg, a, dataKeyOf(a))
}

// ---------------------------------------------------------------------------
// Stack, push/pop, calls, returns
// ---------------------------------------------------------------------------

func (f *funcState) encPush(reg int) {
	rf, re := regField(reg)
	if re {
		f.text = append(f.text, 0x41)
	}
	f.text = append(f.text, 0x50+rf)
}

func (f *funcState) encPop(reg int) {
	rf, re := regField(reg)
	if re {
		f.text = append(f.text, 0x41)
	}
	f.text = append(f.text, 0x58+rf)
}

func (f *funcState) encRet() {
	f.text = append(f.text, 0xC3)
}

// encSubRsp emits sub rsp, imm32 (81 /5). Used with 0 + later patch for
// the frame reservation (always 7 bytes: 48 81 EC xx xx xx xx).
func (f *funcState) encSubRsp(n int) {
	f.text = append(f.text, 0x48, 0x81, 0xEC)
	f.text = faAppendU32(f.text, uint64(uint32(n)))
}

func (f *funcState) encAddRsp(n int) {
	f.text = append(f.text, 0x48, 0x81, 0xC4)
	f.text = faAppendU32(f.text, uint64(uint32(n)))
}

// encCallSym emits a direct call. TU-local targets become merge-time
// fixups; externals become PLT32 relocations immediately.
func (f *funcState) encCallSym(name string) {
	opPos := uint64(len(f.text))
	f.text = append(f.text, 0xE8, 0, 0, 0, 0)
	f.sp.mu.Lock()
	_, local := f.sp.definedFuncs[name]
	f.sp.mu.Unlock()
	if local {
		f.callFixups = append(f.callFixups, spCallFix{pos: opPos + 1, target: name})
		return
	}
	f.relocs = append(f.relocs, ObjReloc{
		Section: ".text", Offset: opPos + 1, Symbol: name,
		Type: R_X86_64_PLT32, Addend: -4,
	})
}

// encCallReg emits call *%reg.
func (f *funcState) encCallReg(reg int) {
	rf, re := regField(reg)
	f.text = append(f.text, rexByte(false, false, false, re)...)
	f.text = append(f.text, 0xFF, 0xD0|rf)
}

// encMovMemStackParam copies an incoming stack argument (positive rbp
// offset) to its frame home. Uses RAX as schedule-free scratch (prologue
// runs before any expression evaluation).
func (f *funcState) encMovMemStackParam(slotOff, stackOff, size int) {
	switch size {
	case 8:
		f.encMovRegMem(REG_RAX, Addr{RegBase: REG_RBP, Disp: stackOff, Index: -1, Scale: 1, Size: 8}, CType{Name: "long", Size: 8, Signed: true})
		f.encMovMemReg(Addr{RegBase: REG_RBP, Disp: slotOff, Index: -1, Scale: 1, Size: 8}, REG_RAX, CType{Name: "long", Size: 8, Signed: true})
	case 4:
		f.encMovRegMem(REG_RAX, Addr{RegBase: REG_RBP, Disp: stackOff, Index: -1, Scale: 1, Size: 4}, CType{Name: "int", Size: 4, Signed: true})
		f.encMovMemReg(Addr{RegBase: REG_RBP, Disp: slotOff, Index: -1, Scale: 1, Size: 4}, REG_RAX, CType{Name: "int", Size: 4, Signed: true})
	default:
		// Narrow/stack-passed aggregates: byte copy loop via scratch.
		for i := 0; i < size; i++ {
			f.encMovRegMem(REG_RAX, Addr{RegBase: REG_RBP, Disp: stackOff + i, Index: -1, Scale: 1, Size: 1}, CType{Name: "char", Size: 1, Signed: true})
			f.encMovMemReg(Addr{RegBase: REG_RBP, Disp: slotOff + i, Index: -1, Scale: 1, Size: 1}, REG_RAX, CType{Name: "char", Size: 1, Signed: true})
		}
	}
}

// ---------------------------------------------------------------------------
// Integer ALU
// ---------------------------------------------------------------------------

// aluOpcodes maps our op names to (opcode, /digit) for r/m forms.
func aluOp(op string) (byte, byte, bool) {
	switch op {
	case "add":
		return 0x01, 0, true
	case "or":
		return 0x09, 1, true
	case "and":
		return 0x21, 4, true
	case "sub":
		return 0x29, 5, true
	case "xor":
		return 0x31, 6, true
	case "cmp":
		return 0x39, 7, true
	}
	return 0, 0, false
}

// encALURegReg emits op dst, src (both GPR, 64-bit).
func (f *funcState) encALURegReg(op string, dst, src int) {
	oc, _, ok := aluOp(op)
	if !ok {
		panic("spass: bad alu op " + op)
	}
	// 01 /r style: reg=src, r/m=dst.
	f.encRR(oc, true, src, dst)
}

// encALURegMem emits op dstReg, [addr] (64-bit).
func (f *funcState) encALURegMem(op string, dst int, a Addr) {
	oc, _, ok := aluOp(op)
	if !ok {
		panic("spass: bad alu op " + op)
	}
	// 03 /r style (03 for add/or/and/xor/cmp... note: 0x03 is the
	// load direction for all of add/or/adc/sbb/and/sub/xor/cmp).
	load := oc + 2 // 01->03, 09->0B, 21->23, 29->2B, 31->33, 39->3B
	f.encRM(false, 0, []byte{load}, true, dst, a, dataKeyOf(a))
}

// encALURegImm emits op reg, imm32 (81 /digit). Caller ensures fit.
func (f *funcState) encALURegImm(op string, reg int, v uint64) {
	_, digit, ok := aluOp(op)
	if !ok {
		panic("spass: bad alu op " + op)
	}
	rf, re := regField(reg)
	// Match GAS's size selection: 83 /digit ib for int8, accumulator
	// short form (05-group) for %rax, else 81 /digit id.
	if int64(v) >= -128 && int64(v) <= 127 {
		f.text = append(f.text, rexByte(true, false, false, re)...)
		f.text = append(f.text, 0x83, 0xC0|(digit<<3)|rf, byte(int8(int64(v))))
		return
	}
	if reg == REG_RAX {
		var acc byte
		switch op {
		case "add":
			acc = 0x05
		case "or":
			acc = 0x0D
		case "and":
			acc = 0x25
		case "sub":
			acc = 0x2D
		case "xor":
			acc = 0x35
		case "cmp":
			acc = 0x3D
		}
		f.text = append(f.text, rexByte(true, false, false, false)...)
		f.text = append(f.text, acc)
		f.text = faAppendU32(f.text, v)
		return
	}
	f.text = append(f.text, rexByte(true, false, false, re)...)
	f.text = append(f.text, 0x81, 0xC0|(digit<<3)|rf)
	f.text = faAppendU32(f.text, v)
}

// encTestRegReg emits test a, b (64-bit).
func (f *funcState) encTestRegReg(a, b int) {
	af, ae := regField(a)
	bf, be := regField(b)
	f.text = append(f.text, rexByte(true, be, false, ae)...)
	f.text = append(f.text, 0x85, 0xC0|(bf<<3)|af)
}

// encNeg emits neg reg (F7 /3).
func (f *funcState) encNeg(reg int) {
	rf, re := regField(reg)
	f.text = append(f.text, rexByte(true, false, false, re)...)
	f.text = append(f.text, 0xF7, 0xD8|rf)
}

// encNot emits not reg (F7 /2).
func (f *funcState) encNot(reg int) {
	rf, re := regField(reg)
	f.text = append(f.text, rexByte(true, false, false, re)...)
	f.text = append(f.text, 0xF7, 0xD0|rf)
}

// encImulRegReg emits imul dst, src (0F AF /r, 64-bit).
func (f *funcState) encImulRegReg(dst, src int) {
	df, de := regField(dst)
	sf, se := regField(src)
	f.text = append(f.text, rexByte(true, de, false, se)...)
	f.text = append(f.text, 0x0F, 0xAF, 0xC0|(df<<3)|sf)
}

// encImulRegImm emits imul dst, src, imm32 (6B/69). Uses 8-bit form when
// the value fits int8.
func (f *funcState) encImulRegImm(dst, src int, v uint64) {
	df, de := regField(dst)
	sf, se := regField(src)
	f.text = append(f.text, rexByte(true, de, false, se)...)
	if int64(v) >= -128 && int64(v) <= 127 {
		f.text = append(f.text, 0x6B, 0xC0|(df<<3)|sf, byte(int8(int64(v))))
		return
	}
	f.text = append(f.text, 0x69, 0xC0|(df<<3)|sf)
	f.text = faAppendU32(f.text, v)
}

// encShift emits sal/sar/shr reg, count (count is imm8 or %cl).
// op: "sal", "sar", "shr". width8 selects 64-bit.
func (f *funcState) encShift(op string, reg int, countImm *uint64, width8 bool) {
	var digit byte
	switch op {
	case "sal":
		digit = 4
	case "shr":
		digit = 5
	case "sar":
		digit = 7
	default:
		panic("spass: bad shift op " + op)
	}
	rf, re := regField(reg)
	f.text = append(f.text, rexByte(width8, false, false, re)...)
	if countImm == nil {
		// D3 /digit (count in %cl).
		f.text = append(f.text, 0xD3, 0xC0|(digit<<3)|rf)
		return
	}
	c := *countImm & 0x3F
	if c == 1 {
		// D1 /digit (count 1).
		f.text = append(f.text, 0xD1, 0xC0|(digit<<3)|rf)
		return
	}
	// C1 /digit ib.
	f.text = append(f.text, 0xC1, 0xC0|(digit<<3)|rf, byte(c))
}

// encSetcc emits set<cond> %reg8 (0F 9x /0).
func (f *funcState) encSetcc(cond byte, reg int) {
	rf, re := regField(reg)
	// setcc with r8-r15b or sil/dil/spl/bpl needs REX (a bare 0x40 selects
	// the low byte; without it the CPU addresses ah/bh/ch/dh).
	needRex := re || reg == REG_RSI || reg == REG_RDI || reg == REG_RSP || reg == REG_RBP
	if needRex {
		rb := rexByte(false, false, false, re)
		if rb == nil {
			rb = []byte{0x40}
		}
		f.text = append(f.text, rb...)
	}
	f.text = append(f.text, 0x0F, cond, 0xC0|rf)
}

// encCqo emits cqo (48 99).
func (f *funcState) encCqo() {
	f.text = append(f.text, 0x48, 0x99)
}

// encCdq emits cdq (99).
func (f *funcState) encCdq() {
	f.text = append(f.text, 0x99)
}

// encDiv emits div/idiv r/m (F7 /6|/7). Caller sets rax[/rdx].
func (f *funcState) encDiv(reg int, width8, signed bool) {
	rf, re := regField(reg)
	f.text = append(f.text, rexByte(width8, false, false, re)...)
	op2 := byte(0xF0)
	if signed {
		op2 = 0xF8
	}
	f.text = append(f.text, 0xF7, op2|rf)
}

// ---------------------------------------------------------------------------
// XMM / floating point
// ---------------------------------------------------------------------------

// encMovXMMMem emits movsd/movss xmm, [addr] (load=true) or movsd/movss
// [addr], xmm (load=false). The SSE prefix travels as encRM's legacy byte
// so REX lands after it (prefix-in-opcodes misorders REX with extended
// regs; EXPR-agent encoder fix).
func (f *funcState) encMovXMMMem(xmm int, a Addr, load bool) {
	xf := byte(xmm & 7)
	xe := xmm > 7
	prefix := byte(0xF2) // sd
	if a.Size == 4 {
		prefix = 0xF3 // ss
	}
	if load {
		f.encRM(false, prefix, []byte{0x0F, 0x10}, false, xmm, a, dataKeyOf(a))
	} else {
		f.encRM(false, prefix, []byte{0x0F, 0x11}, false, xmm, a, dataKeyOf(a))
	}
	_ = xf
	_ = xe
}

// encMovXMMRip emits movsd/movss xmm, [label+rip] with a merge fixup.
func (f *funcState) encMovXMMRip(xmm int, label string, size int) {
	f.encMovXMMMem(xmm, Addr{Sym: label, Disp: 0, Index: -1, Scale: 1, Size: size}, true)
}

// encMovXMMReg emits movsd/movss xmmDst, xmmSrc (register copy).
func (f *funcState) encMovXMMReg(dst, src int) {
	df := byte(dst & 7)
	sf := byte(src & 7)
	f.text = append(f.text, rexByte(false, dst > 7, false, src > 7)...)
	f.text = append(f.text, 0xF2, 0x0F, 0x10, 0xC0|(df<<3)|sf)
}

// encSSE emits a packed/scalar SSE op: prefix REX 0F opcode ModRM.
func (f *funcState) encSSE(prefix, opcode byte, dst, src int) {
	df := byte(dst & 7)
	sf := byte(src & 7)
	f.text = append(f.text, prefix)
	f.text = append(f.text, rexByte(false, dst > 7, false, src > 7)...)
	f.text = append(f.text, 0x0F, opcode, 0xC0|(df<<3)|sf)
}

// encUcomisd emits ucomisd xmmDst, xmmSrc (66 0F 2E).
func (f *funcState) encUcomisd(dst, src int) {
	f.encSSE(0x66, 0x2E, dst, src)
}

// encCvtSi2Sd emits cvtsi2sd[l] xmm, r/m (F2 0F 2A).
func (f *funcState) encCvtSi2Sd(xmm int, a Addr, is64 bool) {
	op := []byte{0x0F, 0x2A}
	if is64 {
		f.encRM(false, 0xF2, op, true, xmm, a, dataKeyOf(a))
	} else {
		f.encRM(false, 0xF2, op, false, xmm, a, dataKeyOf(a))
	}
}

// encCvtSi2SdReg emits cvtsi2sd[l] xmm, reg (register source).
func (f *funcState) encCvtSi2SdReg(xmm, reg int, is64 bool) {
	xf := byte(xmm & 7)
	rf := byte(reg & 7)
	f.text = append(f.text, 0xF2)
	f.text = append(f.text, rexByte(is64, xmm > 7, false, reg > 7)...)
	f.text = append(f.text, 0x0F, 0x2A, 0xC0|(xf<<3)|rf)
}

// encCvtSd2Si emits cvttsd2si reg, xmm (F2 REX 0F 2C).
func (f *funcState) encCvtSd2Si(reg, xmm int, is64 bool) {
	rf := byte(reg & 7)
	xf := byte(xmm & 7)
	f.text = append(f.text, 0xF2)
	f.text = append(f.text, rexByte(is64, reg > 7, false, xmm > 7)...)
	f.text = append(f.text, 0x0F, 0x2C, 0xC0|(rf<<3)|xf)
}

// encCvtSs2Sd emits cvtss2sd xmmDst, xmmSrc (F3 0F 5A).
func (f *funcState) encCvtSs2Sd(dst, src int) {
	f.encSSE(0xF3, 0x5A, dst, src)
}

// encCvtSd2Ss emits cvtsd2ss xmmDst, xmmSrc (F2 0F 5A).
func (f *funcState) encCvtSd2Ss(dst, src int) {
	f.encSSE(0xF2, 0x5A, dst, src)
}

// encMovdXMM emits movd xmm, r32 (66 0F 6E).
func (f *funcState) encMovdXMM(xmm, reg int) {
	xf := byte(xmm & 7)
	rf := byte(reg & 7)
	f.text = append(f.text, 0x66)
	f.text = append(f.text, rexByte(false, xmm > 7, false, reg > 7)...)
	f.text = append(f.text, 0x0F, 0x6E, 0xC0|(xf<<3)|rf)
}

// encMovdReg emits movd r32, xmm (66 0F 7E).
func (f *funcState) encMovdReg(reg, xmm int) {
	rf := byte(reg & 7)
	xf := byte(xmm & 7)
	f.text = append(f.text, 0x66)
	f.text = append(f.text, rexByte(false, reg > 7, false, xmm > 7)...)
	f.text = append(f.text, 0x0F, 0x7E, 0xC0|(rf<<3)|xf)
}

// encMovqXMM emits movq xmm, r64 (66 0F 6E + REX.W).
func (f *funcState) encMovqXMM(xmm, reg int) {
	xf := byte(xmm & 7)
	rf := byte(reg & 7)
	f.text = append(f.text, 0x66)
	f.text = append(f.text, rexByte(true, xmm > 7, false, reg > 7)...)
	f.text = append(f.text, 0x0F, 0x6E, 0xC0|(xf<<3)|rf)
}

// encMovqReg emits movq r64, xmm (66 0F 7E + REX.W).
func (f *funcState) encMovqReg(reg, xmm int) {
	rf := byte(reg & 7)
	xf := byte(xmm & 7)
	f.text = append(f.text, 0x66)
	f.text = append(f.text, rexByte(true, reg > 7, false, xmm > 7)...)
	f.text = append(f.text, 0x0F, 0x7E, 0xC0|(rf<<3)|xf)
}

// encIntToXMM converts an integer SVal to xmm (cvtsi2sd[q]).
func (f *funcState) encIntToXMM(xmm int, v SVal) {
	switch v.Kind {
	case KReg:
		f.encCvtSi2SdReg(xmm, v.Reg, v.Type.Size == 8)
	case KConst:
		r := f.scratchReg()
		f.encMovRegImm(r, v.Imm, v.Type)
		f.encIntToXMM(xmm, SVal{Kind: KReg, Type: v.Type, Reg: r})
	default:
		// KMem: direct cvt from memory.
		f.encCvtSi2Sd(xmm, v.Mem, v.Type.Size == 8)
	}
}

// encXMMToInt converts an xmm SVal to a GPR (cvttsd2si / movd).
func (f *funcState) encXMMToInt(reg int, v SVal) {
	if v.Type.Size == 4 && v.Type.IsFloat {
		// float: cvttss2si (F3 0F 2C).
		rf := byte(reg & 7)
		xf := byte(v.Reg & 7)
		f.text = append(f.text, 0xF3)
		f.text = append(f.text, rexByte(false, reg > 7, false, v.Reg > 7)...)
		f.text = append(f.text, 0x0F, 0x2C, 0xC0|(rf<<3)|xf)
		return
	}
	f.encCvtSd2Si(reg, v.Reg, true)
}
