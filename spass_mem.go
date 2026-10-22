// spass_mem: memory model, conversions, and global data for single-pass.
//
// Owns the MEM agent surface:
//
//	addrOf, convert, genDataInit, genCompoundInit, globalAddr,
//	structMemberOff, plus copyStruct for aggregate assignment.
//
// Lvalue/reg discipline (sanctioned by the skeleton contract): several
// paths below materialize an address into a scratch register and return
// Addr{RegBase: reg}. The register is safe because every consumer (load,
// store, copyStruct, call-arg homes) uses it IMMEDIATELY in the same
// expression lowering with no intervening calls and no statement boundary
// in between; the rotating scratch dispenser hands out a fresh register
// for each subsequent scratchReg() call, so the base cannot be clobbered
// by the single load/store that follows. Callers MUST NOT hold such an
// Addr across a call emission or a statement boundary.
//
// AST-shape notes (verified against parser.go; the spec's NodeDereference
// / NodeAddressOf do NOT exist in parser output):
//   - `*p` and `&x` parse as NodeUnaryOp with Operator "*" and "&".
//     addrOf handles those forms (plus the never-produced node types).
//   - Parens produce no AST node; member `.`/`->` is NodeMemberAccess
//     (Operator "./->", IsPointer, MemberName); indexing is NodeArrayAccess
//     with Children {base, index}; multi-dim decls live in NodeVarDecl.Dims
//     (element count totals in ArraySize); struct members never contain
//     dots (anonymous members are inlined by the parser), but a dotted
//     fallback is kept in structMemberOff.
//   - Identifier/locale type spellings may carry "[N]" suffixes or be
//     bare element types; dims recovery order: MemNoteArray side table,
//     bracket suffixes, single-dim fallback.
package main

import (
	"fmt"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Local array dims side table (coordination point with STMT).
// ---------------------------------------------------------------------------

// memArrDecl records how a frame-resident array was declared: its element
// type plus per-dimension sizes. STMT's VarDecl lowering MUST call
// MemNoteArray for every array-typed local/param it declares; without the
// entry, addrOf can only treat the object as a single-dim array of its
// element type (pointer-typed locals are still exact via IsPtr).
type memArrDecl struct {
	elem CType
	dims []int
}

var memLocalArrays = struct {
	sync.Mutex
	m map[*funcState]map[string]memArrDecl
}{m: make(map[*funcState]map[string]memArrDecl)}

// MemNoteArray records an array-typed local or param for multi-dim index
// lowering. STMT calls this right after allocating the frame slot.
// A bare pointer with unknown dims is NOT an array object and is refused:
// recording it would make decay checks (EXPR exIsArrayName) treat `p[i]`
// as `(&p)[i]`. True arrays (non-pointer element or real dims) record.
func MemNoteArray(f *funcState, name string, elem CType, dims []int) {
	if len(dims) == 0 && elem.IsPtr {
		return
	}
	cp := append([]int(nil), dims...)
	memLocalArrays.Lock()
	t, ok := memLocalArrays.m[f]
	if !ok {
		t = make(map[string]memArrDecl)
		memLocalArrays.m[f] = t
	}
	t[name] = memArrDecl{elem: elem, dims: cp}
	memLocalArrays.Unlock()
}

func memLookupArray(f *funcState, name string) (memArrDecl, bool) {
	memLocalArrays.Lock()
	defer memLocalArrays.Unlock()
	t, ok := memLocalArrays.m[f]
	if !ok {
		return memArrDecl{}, false
	}
	d, ok := t[name]
	return d, ok
}

// memForgetFunc drops a finished function's entries (frame reuse safety).
func memForgetFunc(f *funcState) {
	memLocalArrays.Lock()
	delete(memLocalArrays.m, f)
	memLocalArrays.Unlock()
}

// ---------------------------------------------------------------------------
// Address materialization
// ---------------------------------------------------------------------------

// memNormAddr repairs zero-value Addr literals: Index 0 with Scale 0
// can only come from a missing initializer (a real RAX index always has
// Scale 1/2/4/8), and Scale 0 is never encodable.
func memNormAddr(a Addr) Addr {
	if a.Scale <= 0 {
		a.Scale = 1
		if a.Index == 0 {
			a.Index = -1
		}
	}
	return a
}

// memAddrToReg emits LEA of an arbitrary address into a fresh scratch reg.
// Sym bases fold their Disp into the RIP fixup addend (encRM's Sym path
// ignores Disp/Index, so they must never reach load/store directly).
// See the file header for the lifetime rule of the returned register.
func (f *funcState) memAddrToReg(a Addr) int {
	a = memNormAddr(a)
	idx, sc := a.Index, a.Scale
	base := a
	base.Index, base.Scale = -1, 1
	r := f.scratchReg()
	f.encLeaRegMem(r, base)
	if base.Sym != "" && base.Disp != 0 && len(f.roFixups) > 0 {
		f.roFixups[len(f.roFixups)-1].add += int64(base.Disp)
	}
	if idx >= 0 {
		if sc != 1 && sc != 2 && sc != 4 && sc != 8 {
			sc = 1
		}
		t := f.scratchReg()
		f.encMovRegReg(t, idx, 8)
		if sc != 1 {
			f.encImulRegImm(t, t, uint64(sc))
		}
		f.encALURegReg("add", r, t)
	}
	return r
}

// ---------------------------------------------------------------------------
// Type spelling / struct-name synthesis for expression nodes
// ---------------------------------------------------------------------------

// memSpellingOfType renders a CType back to a parseable spelling.
func memSpellingOfType(ty CType) string {
	if ty.IsPtr && !strings.HasSuffix(strings.TrimSpace(ty.Name), "*") {
		return strings.TrimSpace(ty.Name) + "*"
	}
	if idx := strings.Index(ty.Name, "["); idx >= 0 {
		return strings.TrimSpace(ty.Name[:idx])
	}
	return ty.Name
}

// memStripOnePtr removes one level of pointer indirection after resolving
// typedefs. "void*" (or unresolvable) strips to "void".
func memStripOnePtr(s *SPass, spelling string) string {
	t := strings.TrimSpace(spelling)
	for i := 0; i < 32; i++ {
		if aliased, ok := s.typedefs[t]; ok {
			t = strings.TrimSpace(stripStorageClass(aliased))
			continue
		}
		break
	}
	t = strings.TrimSpace(t)
	if strings.HasSuffix(t, "*") {
		t = strings.TrimSpace(strings.TrimSuffix(t, "*"))
		if t == "" {
			return "void"
		}
		return t
	}
	return t
}

// memStructBase resolves a type spelling to its struct/union definition
// name ("", false when not a struct/union).
func memStructBase(sp *SPass, spelling string) (string, bool) {
	t := strings.TrimSpace(stripStorageClass(spelling))
	for i := 0; i < 32; i++ {
		if aliased, ok := sp.typedefs[t]; ok {
			t = strings.TrimSpace(stripStorageClass(aliased))
			continue
		}
		break
	}
	if strings.HasPrefix(t, "struct ") {
		return strings.TrimSpace(t[len("struct "):]), true
	}
	if strings.HasPrefix(t, "union ") {
		return strings.TrimSpace(t[len("union "):]), true
	}
	if _, ok := sp.structs[t]; ok {
		return t, true
	}
	return "", false
}

// memSpelling synthesizes the C type spelling of an expression node.
func (f *funcState) memSpelling(n *ASTNode) string {
	if n == nil {
		return ""
	}
	if n.DataType != "" {
		return n.DataType
	}
	sp := f.sp
	switch n.Type {
	case NodeIdentifier:
		if lv, ok := f.locals[n.VarName]; ok {
			return memSpellingOfType(lv.Type)
		}
		for i := range f.params {
			if f.params[i].Name == n.VarName {
				return memSpellingOfType(f.params[i].Type)
			}
		}
		if g, ok := sp.globals[n.VarName]; ok {
			return g.Type
		}
		if _, ok := sp.enums[n.VarName]; ok {
			return "int"
		}
		return "void*"
	case NodeUnaryOp:
		if len(n.Children) == 0 {
			return ""
		}
		switch n.Operator {
		case "*":
			return memStripOnePtr(sp, f.memSpelling(n.Children[0]))
		case "&":
			if s := f.memSpelling(n.Children[0]); s != "" {
				return s + "*"
			}
			return "void*"
		}
		return ""
	case NodeDereference:
		if len(n.Children) == 0 {
			return ""
		}
		return memStripOnePtr(sp, f.memSpelling(n.Children[0]))
	case NodeArrayAccess:
		if len(n.Children) == 0 {
			return ""
		}
		// Element spelling: peel one pointer level, else the base
		// spelling already names the element type.
		bs := strings.TrimSpace(f.memSpelling(n.Children[0]))
		if strings.HasSuffix(bs, "*") {
			return memStripOnePtr(sp, bs)
		}
		return bs
	case NodeMemberAccess:
		if len(n.Children) == 0 {
			return ""
		}
		st, ok := f.memStructNameOf(n.Children[0], n.IsPointer)
		if !ok {
			return ""
		}
		if _, _, ty, _, ok := memMemberInfo(sp, st, n.MemberName); ok {
			return memSpellingOfType(ty)
		}
		return ""
	case NodeCall:
		if n.Target == nil {
			if sig, ok := sp.funcSigs[n.Name]; ok && sig.ReturnType != "" {
				return sig.ReturnType
			}
		}
		return "int"
	case NodeString:
		return "char*"
	case NodeNumber:
		if isFloatSpelling(n.Value) {
			return "double"
		}
		return "int"
	}
	return ""
}

// memNodeType resolves an expression node's CType (8-byte fallback).
func (f *funcState) memNodeType(n *ASTNode) CType {
	if s := f.memSpelling(n); s != "" {
		if ty, ok := ctypeOf(s, f.sp); ok {
			if ty.Size <= 0 {
				ty.Size = 8
			}
			return ty
		}
	}
	return CType{Name: "long", Size: 8, Signed: true}
}

// memStructNameOf resolves the struct/union definition behind a member
// base. When throughPtr, the base spelling is a pointer (strip first).
func (f *funcState) memStructNameOf(base *ASTNode, throughPtr bool) (string, bool) {
	s := f.memSpelling(base)
	if s == "" {
		// Rvalue bases memSpelling can't spell (statement-expression
		// blocks, calls): ask EXPR's type query instead.
		if ty, ok := f.exRValType(base); ok {
			s = ty.Name
		}
	}
	if throughPtr {
		s = memStripOnePtr(f.sp, s)
	}
	return memStructBase(f.sp, s)
}

// memMemberInfo returns full member info: offset, total member size,
// element CType, and array dims.
func memMemberInfo(sp *SPass, structName, member string) (off, size int, ty CType, dims []int, ok bool) {
	def, found := sp.structs[structName]
	if !found {
		return 0, 0, CType{}, nil, false
	}
	for i := range def.Members {
		m := &def.Members[i]
		if m.Name == member {
			ty, valid := ctypeOf(m.Type, sp)
			if !valid {
				ty = CType{Name: m.Type, Size: m.Size}
				if ty.Size <= 0 {
					ty.Size = 8
				}
			}
			return m.Offset, m.Size, ty, append([]int(nil), m.Dims...), true
		}
	}
	// Dotted fallback (e.g. "a.b" if a frontend ever flattens them).
	if strings.Contains(member, ".") {
		parts := strings.Split(member, ".")
		cur := structName
		acc := 0
		for i, p := range parts {
			d, found := sp.structs[cur]
			if !found {
				return 0, 0, CType{}, nil, false
			}
			advanced := false
			for j := range d.Members {
				m := &d.Members[j]
				if m.Name == p {
					acc += m.Offset
					if i == len(parts)-1 {
						ty, valid := ctypeOf(m.Type, sp)
						if !valid {
							ty = CType{Name: m.Type, Size: m.Size}
							if ty.Size <= 0 {
								ty.Size = 8
							}
						}
						return acc, m.Size, ty, append([]int(nil), m.Dims...), true
					}
					nxt, ok := memStructBase(sp, m.Type)
					if !ok {
						return 0, 0, CType{}, nil, false
					}
					cur = nxt
					advanced = true
					break
				}
			}
			if !advanced {
				return 0, 0, CType{}, nil, false
			}
		}
	}
	return 0, 0, CType{}, nil, false
}

// structMemberOff walks the parser StructDef member list by name.
// Anonymous members are inlined by the parser (plain names); a dotted
// "a.b" path is additionally resolved through nested definitions.
func structMemberOff(sp *SPass, structName, member string) (off, size int, ok bool) {
	off, size, _, _, ok = memMemberInfo(sp, structName, member)
	return off, size, ok
}

// ---------------------------------------------------------------------------
// addrOf
// ---------------------------------------------------------------------------

// addrOf computes the address of an lvalue. No loads are emitted, except
// that pointer-chasing bases (dereference, ->, decayed arrays/pointers)
// necessarily materialize the pointer value into a scratch register first.
func (f *funcState) addrOf(n *ASTNode) Addr {
	if n == nil {
		panic(unsupported("null lvalue", n))
	}
	switch n.Type {
	case NodeIdentifier:
		return f.memAddrOfIdent(n.VarName, n)
	case NodeDereference:
		if len(n.Children) < 1 {
			panic(unsupported("empty dereference", n))
		}
		return f.memAddrOfDeref(n.Children[0], n)
	case NodeUnaryOp:
		if len(n.Children) < 1 {
			panic(unsupported("empty unary lvalue", n))
		}
		switch n.Operator {
		case "*":
			// Parser form of *p.
			return f.memAddrOfDeref(n.Children[0], n)
		case "&":
			// Parser form of &x: the address of the operand.
			return f.addrOf(n.Children[0])
		}
		panic(unsupported("unary lvalue", n))
	case NodeAddressOf:
		if len(n.Children) < 1 {
			panic(unsupported("empty address-of", n))
		}
		return f.addrOf(n.Children[0])
	case NodeArrayAccess:
		return f.memAddrOfIndex(n)
	case NodeMemberAccess:
		return f.memAddrOfMember(n)
	default:
		// Rvalue bases: call results (f()[i], f().x), arithmetic, etc.
		// have no address — materialize into a temp home and address
		// that. KMem values (struct rvalues) already have one.
		// (Bell bring-up: box2d f().x accessors need this.)
		v := f.genRVal(n)
		if v.Kind == KVoid {
			panic(unsupported("address of void", n))
		}
		if v.Kind == KMem {
			return v.Mem
		}
		sz := v.Type.Size
		if sz <= 0 {
			sz = 8
		}
		slot := f.tempSlot(sz)
		slot.Size = sz
		f.store(slot, v)
		return slot
	}
}

// memAddrOfIdent resolves locals (frame), params (homed slots), globals
// (RIP), and function designators. Locals shadow globals.
func (f *funcState) memAddrOfIdent(name string, n *ASTNode) Addr {
	if lv, ok := f.locals[name]; ok {
		sz := lv.Type.Size
		if sz <= 0 {
			sz = 8
		}
		return Addr{RegBase: REG_RBP, Disp: lv.Offset, Index: -1, Scale: 1, Size: sz}
	}
	for i := range f.params {
		if f.params[i].Name == name {
			sz := f.params[i].Type.Size
			if sz <= 0 {
				sz = 8
			}
			return Addr{RegBase: REG_RBP, Disp: f.params[i].Offset, Index: -1, Scale: 1, Size: sz}
		}
	}
	if a, _, ok := f.sp.globalAddr(name); ok {
		return a
	}
	f.sp.mu.Lock()
	_, defined := f.sp.definedFuncs[name]
	_, declared := f.sp.funcSigs[name]
	f.sp.mu.Unlock()
	if defined || declared {
		return Addr{Sym: name, RegBase: -1, Disp: 0, Index: -1, Scale: 1, Size: 8}
	}
	panic(unsupported("unknown identifier "+name, n))
}

// memAddrOfDeref evaluates a pointer expression and returns an address
// through the register holding the pointer (see header lifetime rule).
func (f *funcState) memAddrOfDeref(ptr *ASTNode, n *ASTNode) Addr {
	pv := f.materialize(f.genRVal(ptr))
	if pv.Kind != KReg {
		panic(unsupported("non-pointer dereference", n))
	}
	ty := f.memNodeType(n)
	if ty.Size <= 0 {
		ty.Size = 8
	}
	return Addr{RegBase: pv.Reg, Disp: 0, Index: -1, Scale: 1, Size: ty.Size}
}

// memAddrOfMember handles `.` (base address + offset) and `->`
// (pointer value in scratch + offset). Nested chains recurse.
func (f *funcState) memAddrOfMember(n *ASTNode) Addr {
	if len(n.Children) < 1 {
		panic(unsupported("empty member access", n))
	}
	base := n.Children[0]
	st, ok := f.memStructNameOf(base, n.IsPointer)
	if !ok {
		panic(unsupported("member of non-struct", n))
	}
	off, msize, _, _, ok := memMemberInfo(f.sp, st, n.MemberName)
	if !ok {
		panic(unsupported("unknown member "+n.MemberName, n))
	}
	if msize <= 0 {
		msize = 8
	}
	if n.IsPointer {
		pv := f.materialize(f.genRVal(base))
		if pv.Kind != KReg {
			panic(unsupported("non-pointer ->", n))
		}
		return Addr{RegBase: pv.Reg, Disp: off, Index: -1, Scale: 1, Size: msize}
	}
	ba := f.addrOf(base)
	if ba.Sym != "" {
		if off == 0 {
			ba.Size = msize
			return ba
		}
		r := f.memAddrToReg(ba)
		f.encALURegImm("add", r, uint64(int64(off)))
		return Addr{RegBase: r, Disp: 0, Index: -1, Scale: 1, Size: msize}
	}
	ba.Disp += off
	ba.Size = msize
	return ba
}

// ---------------------------------------------------------------------------
// Array indexing with stride flattening
// ---------------------------------------------------------------------------

// memRoot describes the indexed root: a true array (address known) or a
// pointer (value must be loaded).
type memRoot struct {
	isArray   bool
	elem      CType
	dims      []int // valid when isArray && knownDims
	knownDims bool
	addr      Addr     // valid when isArray
	ptrNode   *ASTNode // valid when !isArray: expression yielding the pointer
	stride0   int      // fallback stride when dims unknown (single level)
}

// memRootInfo classifies the spine root of an indexing chain.
func (f *funcState) memRootInfo(root *ASTNode) memRoot {
	sp := f.sp
	// Identifier root.
	if root.Type == NodeIdentifier {
		name := root.VarName
		if d, ok := memLookupArray(f, name); ok && len(d.dims) > 0 {
			return memRoot{isArray: true, elem: d.elem, dims: d.dims, knownDims: true,
				addr: f.memAddrOfIdent(name, root)}
		}
		if lv, ok := f.locals[name]; ok {
			if lv.Type.IsPtr {
				return memRoot{ptrNode: root, elem: f.memPointed(lv.Type)}
			}
			elem := lv.Type
			if idx := strings.Index(elem.Name, "["); idx >= 0 {
				if ty, ok := ctypeOf(strings.TrimSpace(elem.Name[:idx]), sp); ok {
					elem = ty
				}
			}
			return memRoot{isArray: true, elem: elem, knownDims: false,
				stride0: elem.Size, addr: f.memAddrOfIdent(name, root)}
		}
		for i := range f.params {
			if f.params[i].Name == name {
				if d, ok := memLookupArray(f, name); ok && len(d.dims) > 0 {
					return memRoot{isArray: true, elem: d.elem, dims: d.dims, knownDims: true,
						addr: f.memAddrOfIdent(name, root)}
				}
				pt := f.params[i].Type
				if pt.IsPtr {
					return memRoot{ptrNode: root, elem: f.memPointed(pt)}
				}
				return memRoot{ptrNode: root, elem: pt}
			}
		}
		if g, ok := sp.globals[name]; ok {
			ty, valid := ctypeOf(g.Type, sp)
			if !valid {
				ty = CType{Name: g.Type, Size: g.Size}
			}
			if g.ArraySize > 0 || len(g.Dims) > 0 {
				return memRoot{isArray: true, elem: ty, dims: append([]int(nil), g.Dims...),
					knownDims: len(g.Dims) > 0, stride0: ty.Size,
					addr: f.memAddrOfIdent(name, root)}
			}
			if ty.IsPtr {
				return memRoot{ptrNode: root, elem: f.memPointed(ty)}
			}
			panic(unsupported("indexing non-array "+name, root))
		}
		a, ty, ok := sp.globalAddr(name)
		if ok {
			_ = a
			if ty.IsPtr {
				return memRoot{ptrNode: root, elem: f.memPointed(ty)}
			}
			return memRoot{isArray: true, elem: ty, knownDims: false,
				stride0: ty.Size, addr: f.memAddrOfIdent(name, root)}
		}
		panic(unsupported("unknown identifier "+name, root))
	}
	// Struct-member root: true array, or pointer-typed member.
	if root.Type == NodeMemberAccess && len(root.Children) >= 1 {
		st, ok := f.memStructNameOf(root.Children[0], root.IsPointer)
		if !ok {
			panic(unsupported("member of non-struct", root))
		}
		off, msize, mty, mdims, ok := memMemberInfo(sp, st, root.MemberName)
		if !ok {
			panic(unsupported("unknown member "+root.MemberName, root))
		}
		_ = off
		_ = msize
		if len(mdims) > 0 {
			return memRoot{isArray: true, elem: mty, dims: mdims, knownDims: true,
				addr: f.addrOf(root)}
		}
		if mty.IsPtr {
			return memRoot{ptrNode: root, elem: f.memPointed(mty)}
		}
		panic(unsupported("indexing non-array member "+root.MemberName, root))
	}
	// String literal root decays to its base address.
	if root.Type == NodeString {
		return memRoot{ptrNode: root, elem: CType{Name: "char", Size: 1, Signed: true}}
	}
	// Anything else (dereference, call, cast, ...): must be pointer-typed.
	elemSpell := memStripOnePtr(sp, f.memSpelling(root))
	elem, ok := ctypeOf(elemSpell, sp)
	if !ok || elem.Size <= 0 {
		elem = CType{Name: elemSpell, Size: 8}
	}
	return memRoot{ptrNode: root, elem: elem}
}

// memPointed returns the pointed-to CType for a pointer type.
func (f *funcState) memPointed(pt CType) CType {
	spelled := memStripOnePtr(f.sp, memSpellingOfType(pt))
	ty, ok := ctypeOf(spelled, f.sp)
	if !ok || ty.Size <= 0 {
		ty = CType{Name: spelled, Size: 8}
		if spelled == "void" {
			ty.Size = 1
		}
	}
	return ty
}

// memAddrOfIndex lowers a[i] (and multi-dim chains) to an address.
// Single dynamic index with a power-of-two stride uses Index/Scale;
// everything else is folded into base-reg + displacement arithmetic.
func (f *funcState) memAddrOfIndex(n *ASTNode) Addr {
	var idxs []*ASTNode
	root := n
	for root != nil && root.Type == NodeArrayAccess {
		if len(root.Children) < 2 {
			panic(unsupported("empty index", root))
		}
		idxs = append(idxs, root.Children[1])
		root = root.Children[0]
	}
	if root == nil {
		panic(unsupported("empty array base", n))
	}
	// Innermost-first order.
	for i, j := 0, len(idxs)-1; i < j; i, j = i+1, j-1 {
		idxs[i], idxs[j] = idxs[j], idxs[i]
	}
	info := f.memRootInfo(root)

	// Cursor type state as levels peel.
	curIsArray := info.isArray
	curDims := info.dims
	curKnown := info.knownDims
	curElem := info.elem
	curIsPtr := !info.isArray // pointer kind (element reached through load)

	// Accumulator: base (address or reg) + static + one dynamic term.
	var baseAddr Addr
	baseReg := -1
	hasBaseAddr := false
	if info.isArray {
		baseAddr = info.addr
		hasBaseAddr = true
	} else {
		pv := f.genRVal(info.ptrNode)
		switch pv.Kind {
		case KReg:
			baseReg = pv.Reg
		case KMem:
			baseReg = f.memAddrToReg(pv.Mem)
		default:
			m := f.materialize(pv)
			if m.Kind != KReg {
				panic(unsupported("non-pointer index base", root))
			}
			baseReg = m.Reg
		}
		// String-literal base arrives as KMem to the bytes; memAddrToReg
		// above already took its address. Anything else KMem is a pointer
		// variable: load its value instead.
		if pv.Kind == KMem && info.ptrNode.Type != NodeString {
			a := pv.Mem
			a.Size = 8
			r := f.scratchReg()
			f.encMovRegMem(r, a, CType{Name: "ptr", Size: 8})
			baseReg = r
		}
	}

	stat := 0
	dyn := -1
	dynStride := 0
	finalSize := 8
	nLevels := len(idxs)

	for l, idx := range idxs {
		last := l == nLevels-1
		stride := 0
		if curIsArray && curKnown {
			if l >= len(curDims) {
				panic(unsupported("too many indices", n))
			}
			stride = curElem.Size
			for _, d := range curDims[l+1:] {
				if d > 0 {
					stride *= d
				}
			}
			if stride <= 0 {
				stride = curElem.Size
			}
			if l+1 == len(curDims) {
				curIsArray = false
				curIsPtr = curElem.IsPtr
			}
			// Remaining size after this level.
			rem := curElem.Size
			for _, d := range curDims[l+1:] {
				if d > 0 {
					rem *= d
				}
			}
			finalSize = rem
		} else if curIsArray && !curKnown {
			if l > 0 {
				panic(&spassError{what: "multi-dim local without MemNoteArray dims", node: n})
			}
			stride = info.stride0
			if stride <= 0 {
				stride = curElem.Size
			}
			curIsArray = false
			curIsPtr = curElem.IsPtr
			finalSize = curElem.Size
		} else {
			// Pointer level.
			stride = curElem.Size
			if stride <= 0 {
				stride = 8
			}
			next := f.memPointed(curElem)
			curElem = next
			curIsPtr = false // scalar (or refreshed below if still ptr)
			if strings.HasSuffix(strings.TrimSpace(memSpellingOfType(next)), "*") || next.IsPtr {
				curIsPtr = true
			}
			finalSize = next.Size
			if finalSize <= 0 {
				finalSize = 8
			}
		}

		iv := f.genRVal(idx)
		if c, ok := memConstSVal(iv); ok {
			stat += int(c) * stride
		} else {
			m := f.materialize(iv)
			if m.Kind != KReg {
				panic(unsupported("non-integer index", idx))
			}
			r := m.Reg
			// Fast path: a single dynamic index with a SIB-scale stride
			// stays raw (the Index/Scale form scales it); anything else
			// is pre-scaled here and added explicitly at collapse.
			fast := nLevels == 1 && (stride == 1 || stride == 2 || stride == 4 || stride == 8)
			if !fast && stride != 1 {
				f.encImulRegImm(r, r, uint64(stride))
			}
			if dyn < 0 {
				dyn, dynStride = r, stride
			} else {
				f.encALURegReg("add", dyn, r)
			}
		}

		if !last {
			// Peel one level: arrays stay address arithmetic; pointers
			// (and arrays whose element is a pointer) need a load.
			needLoad := !curIsArray
			if needLoad {
				lr := f.memCollapseLoad(hasBaseAddr, baseAddr, baseReg, stat, dyn)
				baseReg, hasBaseAddr, stat, dyn = lr, false, 0, -1
				// After a load the cursor is the loaded value's kind.
				if !curIsPtr {
					curElem, curIsPtr = f.memPointed(curElem), false
				}
			}
		}
	}
	if finalSize <= 0 {
		finalSize = 8
	}
	return f.memCollapseAddr(hasBaseAddr, baseAddr, baseReg, stat, dyn, dynStride, nLevels, finalSize)
}

// memCollapseLoad materializes base+stat+dyn, loads the 8-byte pointer
// there, and returns the register holding it.
func (f *funcState) memCollapseLoad(hasAddr bool, ba Addr, br, stat, dyn int) int {
	a := f.memCollapseAddr(hasAddr, ba, br, stat, dyn, 0, 2, 8)
	a.Size = 8
	r := f.scratchReg()
	f.encMovRegMem(r, a, CType{Name: "ptr", Size: 8})
	return r
}

// memCollapseAddr folds the accumulator into a skeleton-safe address:
// Sym bases never carry Disp/Index (LEA first); a single dynamic term
// with a 1/2/4/8 stride on a simple base uses Index/Scale.
func (f *funcState) memCollapseAddr(hasAddr bool, ba Addr, br, stat, dyn, dynStride, nLevels, size int) Addr {
	ba = memNormAddr(ba)
	if size <= 0 {
		size = 8
	}
	useIndex := dyn >= 0 && nLevels == 1 &&
		(dynStride == 1 || dynStride == 2 || dynStride == 4 || dynStride == 8)
	if hasAddr && ba.Sym != "" && (stat != 0 || dyn >= 0) {
		br = f.memAddrToReg(ba)
		hasAddr = false
	}
	if dyn >= 0 {
		var brBase int
		if hasAddr {
			if useIndex && ba.Index < 0 {
				ba.Disp += stat
				ba.Index, ba.Scale, ba.Size = dyn, dynStride, size
				if ba.Scale == 0 {
					ba.Scale = 1
				}
				return ba
			}
			brBase = f.memAddrToReg(ba)
			hasAddr = false
		} else {
			brBase = br
		}
		if useIndex && !hasAddr {
			return Addr{RegBase: brBase, Disp: stat, Index: dyn, Scale: dynStride, Size: size}
		}
		r := f.scratchReg()
		f.encMovRegReg(r, brBase, 8)
		f.encALURegReg("add", r, dyn)
		if stat != 0 {
			return Addr{RegBase: r, Disp: stat, Index: -1, Scale: 1, Size: size}
		}
		return Addr{RegBase: r, Disp: 0, Index: -1, Scale: 1, Size: size}
	}
	if hasAddr {
		ba.Disp += stat
		ba.Size = size
		return ba
	}
	return Addr{RegBase: br, Disp: stat, Index: -1, Scale: 1, Size: size}
}

// memConstSVal folds an already-evaluated index/constant to int64.
func memConstSVal(v SVal) (int64, bool) {
	if v.Kind != KConst {
		return 0, false
	}
	u, _ := parseIntLit(v.Imm)
	return int64(u), true
}

// ---------------------------------------------------------------------------
// Aggregate copies and zeroing
// ---------------------------------------------------------------------------

// copyStruct copies size bytes src -> dst with a qword loop plus a 4/2/1
// tail. No calls are emitted; two fixed scratch regs are used for the
// whole copy so rotation cannot disturb them.
func (f *funcState) copyStruct(dst, src Addr, size int) {
	if size <= 0 {
		return
	}
	// Reserve register bases/indexes for the whole loop: the shared value
	// scratch below must never be re-issued over a live base (the
	// assets_load_model crash: round-robin reuse after ~9 chunks).
	release := f.reserveRegs(append(addrRegs(dst), addrRegs(src)...)...)
	defer release()
	dd, ss := memNormAddr(dst), memNormAddr(src)
	if dd.Sym != "" || dd.Index >= 0 {
		dd2 := Addr{RegBase: f.memAddrToReg(dd), Disp: 0, Index: -1, Scale: 1, Size: 8}
		release2 := f.reserveRegs(dd2.RegBase)
		defer release2()
		dd = dd2
	}
	if ss.Sym != "" || ss.Index >= 0 {
		ss2 := Addr{RegBase: f.memAddrToReg(ss), Disp: 0, Index: -1, Scale: 1, Size: 8}
		release3 := f.reserveRegs(ss2.RegBase)
		defer release3()
		ss = ss2
	}
	q := CType{Name: "long", Size: 8, Signed: true}
	r := f.scratchReg()
	off := 0
	for ; off+8 <= size; off += 8 {
		s := ss
		s.Disp += off
		s.Size = 8
		d := dd
		d.Disp += off
		d.Size = 8
		f.encMovRegMem(r, s, q)
		f.encMovMemReg(d, r, q)
	}
	for _, w := range []int{4, 2, 1} {
		if size-off < w {
			continue
		}
		var t CType
		switch w {
		case 4:
			t = CType{Name: "int", Size: 4, Signed: true}
		case 2:
			t = CType{Name: "short", Size: 2, Signed: true}
		default:
			t = CType{Name: "char", Size: 1, Signed: true}
		}
		s := ss
		s.Disp += off
		s.Size = w
		d := dd
		d.Disp += off
		d.Size = w
		f.encMovRegMem(r, s, t)
		f.encMovMemReg(d, r, t)
		off += w
	}
}

// memZeroRange zeroes size bytes at base (qword stores + 4/2/1 tail).
func (f *funcState) memZeroRange(base Addr, size int) {
	if size <= 0 {
		return
	}
	b := memNormAddr(base)
	if b.Sym != "" || b.Index >= 0 {
		b = Addr{RegBase: f.memAddrToReg(b), Disp: 0, Index: -1, Scale: 1, Size: 8}
	}
	off := 0
	for ; off+8 <= size; off += 8 {
		a := b
		a.Disp += off
		a.Size = 8
		f.encMovMemImm(a, "0", CType{Name: "long", Size: 8, Signed: true})
	}
	for _, w := range []int{4, 2, 1} {
		if size-off < w {
			continue
		}
		var t CType
		switch w {
		case 4:
			t = CType{Name: "int", Size: 4, Signed: true}
		case 2:
			t = CType{Name: "short", Size: 2, Signed: true}
		default:
			t = CType{Name: "char", Size: 1, Signed: true}
		}
		a := b
		a.Disp += off
		a.Size = w
		f.encMovMemImm(a, "0", t)
		off += w
	}
}

// memStoreBytes stores compile-time-known bytes (string tails, etc.).
func (f *funcState) memStoreBytes(dst Addr, data []byte) {
	dst = memNormAddr(dst)
	off := 0
	for ; off+8 <= len(data); off += 8 {
		v := uint64(data[off]) | uint64(data[off+1])<<8 | uint64(data[off+2])<<16 |
			uint64(data[off+3])<<24 | uint64(data[off+4])<<32 | uint64(data[off+5])<<40 |
			uint64(data[off+6])<<48 | uint64(data[off+7])<<56
		r := f.scratchReg()
		f.encMovRegImm(r, fmt.Sprintf("%d", int64(v)), CType{Name: "long", Size: 8, Signed: true})
		a := dst
		a.Disp += off
		a.Size = 8
		f.encMovMemReg(a, r, CType{Name: "long", Size: 8, Signed: true})
	}
	for _, w := range []int{4, 2, 1} {
		if len(data)-off < w {
			continue
		}
		var v uint64
		var t CType
		switch w {
		case 4:
			v = uint64(data[off]) | uint64(data[off+1])<<8 | uint64(data[off+2])<<16 | uint64(data[off+3])<<24
			t = CType{Name: "int", Size: 4, Signed: true}
		case 2:
			v = uint64(data[off]) | uint64(data[off+1])<<8
			t = CType{Name: "short", Size: 2, Signed: true}
		default:
			v = uint64(data[off])
			t = CType{Name: "char", Size: 1, Signed: true}
		}
		a := dst
		a.Disp += off
		a.Size = w
		f.encMovMemImm(a, fmt.Sprintf("%d", v), t)
		off += w
	}
}

// ---------------------------------------------------------------------------
// convert
// ---------------------------------------------------------------------------

func memIsBool(ty CType) bool {
	return !ty.IsFloat && !ty.IsPtr && !ty.IsVoid && ty.Size == 1 &&
		(ty.Name == "_Bool" || ty.Name == "bool")
}

func memIsStruct(sp *SPass, ty CType) bool {
	if ty.IsFloat || ty.IsPtr || ty.IsVoid {
		return false
	}
	if strings.HasPrefix(strings.TrimSpace(ty.Name), "struct ") ||
		strings.HasPrefix(strings.TrimSpace(ty.Name), "union ") {
		return true
	}
	if _, ok := memStructBase(sp, ty.Name); ok {
		return true
	}
	return false
}

func memSameType(a, b CType) bool {
	return a.Name == b.Name && a.Size == b.Size && a.Signed == b.Signed &&
		a.IsFloat == b.IsFloat && a.IsPtr == b.IsPtr && a.IsVoid == b.IsVoid
}

// memCCSetNE is the setcc condition byte for != (0x95). NOTE: the
// skeleton's ccNE constant (0x85) is the Jcc second-opcode form and is
// wrong for setcc; bool lowering here uses the correct byte.
const memCCSetNE = 0x95

// memBoolNorm reduces any scalar to 0/1 in an integer register (NaN is
// false, like the skeleton truthy but with a correct setcc encoding).
func (f *funcState) memBoolNorm(v SVal, to CType) SVal {
	m := f.materialize(v)
	r := f.scratchReg()
	one := CType{Name: "int", Size: 4, Signed: true}
	switch m.Kind {
	case KReg:
		f.encTestRegReg(m.Reg, m.Reg)
		f.encSetcc(memCCSetNE, r)
		f.encMovExtend(r, r, 1, 4, false)
		return SVal{Kind: KReg, Type: to, Reg: r}
	case KXMM:
		f.encMovRegImm(r, "0", one)
		// Compare against +0.0 (never self-compare: x==x is trivially
		// true and NaN handling needs an ordered zero).
		z := f.memScratchXMM()
		f.encSSE(0x66, 0x57, z, z) // xorpd: +0.0
		f.encUcomisd(m.Reg, z)
		done := f.localLabel("nan")
		f.encJcc(0x8A, done) // jp (0F 8A): unordered (NaN) -> stays 0
		f.encSetcc(memCCSetNE, r)
		f.defineLabel(done)
		f.encMovExtend(r, r, 1, 4, false)
		return SVal{Kind: KReg, Type: to, Reg: r}
	}
	panic(&spassError{what: "bad bool source"})
}

// memScratchXMM hands out xmm2..xmm7 for convert results. It never
// returns xmm0/xmm1: EXPR evaluates binary float operands there, so a
// convert scratch must not collide with a live operand (the round-robin
// skeleton scratchXMM alternates 0/1 and demonstrably clobbers). It
// shares the skeleton's xmmIdx counter (perturbing its 0/1 phase is
// harmless) so no extra state is needed.
func (f *funcState) memScratchXMM() int {
	r := 2 + f.xmmIdx%6
	f.xmmIdx++
	return r
}

// convert emits a C conversion. Same-type (and same-struct) is a no-op;
// struct mismatches panic. Integer results are always canonically
// extended to 64 bits (sign per target); _Bool yields 0/1.
func (f *funcState) convert(v SVal, to CType) SVal {
	if v.Kind == KVoid || to.IsVoid {
		return SVal{Kind: KVoid}
	}
	from := v.Type
	if memSameType(from, to) {
		v.Type = to
		return v
	}
	fromStruct, toStruct := memIsStruct(f.sp, from), memIsStruct(f.sp, to)
	if fromStruct || toStruct {
		if fromStruct && toStruct && strings.TrimSpace(from.Name) == strings.TrimSpace(to.Name) {
			v.Type = to
			return v
		}
		panic(&spassError{what: "unsupported struct conversion " + from.Name + "->" + to.Name})
	}
	if memIsBool(to) {
		return f.memBoolNorm(v, to)
	}
	// float -> float
	if from.IsFloat && to.IsFloat {
		if v.Kind == KConst {
			v.Type = to
			return v
		}
		m := f.materialize(v)
		if m.Kind != KXMM {
			panic(&spassError{what: "bad float value"})
		}
		if from.Size == to.Size {
			m.Type = to
			return m
		}
		// In place: never take a fresh xmm — a sibling operand may be
		// live in xmm0/xmm1 across this conversion.
		if to.Size == 4 {
			f.encCvtSd2Ss(m.Reg, m.Reg)
		} else {
			f.encCvtSs2Sd(m.Reg, m.Reg)
		}
		return SVal{Kind: KXMM, Type: to, Reg: m.Reg}
	}
	// int/ptr -> float
	if !from.IsFloat && to.IsFloat {
		if v.Kind == KMem && (from.Size == 8 || (from.Size == 4 && from.Signed && !from.IsPtr)) {
			x := f.memScratchXMM()
			a := v.Mem
			a.Size = from.Size
			f.encCvtSi2Sd(x, a, from.Size == 8)
			if to.Size == 4 {
				// Same register: cvtsd2ss is safely in-place.
				f.encCvtSd2Ss(x, x)
			}
			return SVal{Kind: KXMM, Type: to, Reg: x}
		}
		m := f.materialize(v)
		if m.Kind != KReg {
			panic(&spassError{what: "bad int->float source"})
		}
		is64 := from.Size == 8 || (!from.Signed && from.Size == 4 && !from.IsPtr)
		if from.IsPtr {
			is64 = true
		}
		x := f.memScratchXMM()
		f.encCvtSi2SdReg(x, m.Reg, is64)
		if to.Size == 4 {
			f.encCvtSd2Ss(x, x)
		}
		return SVal{Kind: KXMM, Type: to, Reg: x}
	}
	// float -> int
	if from.IsFloat && !to.IsFloat {
		m := f.materialize(v)
		if m.Kind != KXMM {
			panic(&spassError{what: "bad float->int source"})
		}
		r := f.scratchReg()
		f.encXMMToInt(r, m)
		fromSz := 8
		if m.Type.Size == 4 {
			fromSz = 4 // cvttss2si yields 32 bits
		}
		return f.memIntToIntReg(r, fromSz, true, to)
	}
	// int/ptr -> int/ptr
	m := f.materialize(v)
	if m.Kind != KReg {
		panic(&spassError{what: "bad int conversion"})
	}
	signed := from.Signed && !from.IsPtr
	return f.memIntToIntReg(m.Reg, from.Size, signed, to)
}

// memIntToIntReg normalizes a GPR value to the target int/ptr type:
// 64-bit canonical form, sign/zero extended per target.
func (f *funcState) memIntToIntReg(r, fromSize int, fromSigned bool, to CType) SVal {
	if to.Size == 8 {
		switch {
		case fromSize >= 8:
			// No-op: full width already.
		case fromSize == 4:
			if fromSigned {
				f.encRR(0x63, true, r, r) // movslq
			} else {
				f.encRR(0x89, false, r, r) // movl (zero-extends)
			}
		default:
			f.encMovExtend(r, r, fromSize, 8, fromSigned)
		}
		return SVal{Kind: KReg, Type: to, Reg: r}
	}
	if to.Size == 4 {
		if fromSize > 4 {
			f.encMovRegReg(r, r, 4) // truncate; movl zeroes high 32
		}
		// Smaller sources are already canonical; low 32 holds the value.
		return SVal{Kind: KReg, Type: to, Reg: r}
	}
	// 1/2-byte targets: truncate to 32, then extend per target sign.
	if fromSize > 4 {
		f.encMovRegReg(r, r, 4)
	}
	f.encMovExtend(r, r, to.Size, 8, to.Signed)
	return SVal{Kind: KReg, Type: to, Reg: r}
}

// ---------------------------------------------------------------------------
// Globals: lookup, section rule, data folding
// ---------------------------------------------------------------------------

// globalAddr returns the RIP-relative address and type of a global.
// Locals shadow globals: genRVal/addrOf check f.lookup first.
func (s *SPass) globalAddr(name string) (Addr, CType, bool) {
	if s.globals != nil {
		if g, ok := s.globals[name]; ok && g != nil {
			ty, valid := ctypeOf(g.Type, s)
			if !valid {
				ty = CType{Name: g.Type, Size: g.Size}
				if ty.Size <= 0 {
					ty.Size = 8
				}
			}
			sz := g.Size
			if sz <= 0 {
				sz = ty.Size
			}
			return Addr{Sym: name, RegBase: -1, Disp: 0, Index: -1, Scale: 1, Size: sz}, ty, true
		}
	}
	return Addr{}, CType{}, false
}

// memAlignOf returns the natural alignment (capped at 8) for a spelling.
func memAlignOf(sp *SPass, spelling string, size int) int {
	if st, ok := memStructBase(sp, spelling); ok {
		if def, ok := sp.structs[st]; ok {
			a := def.Align
			if a <= 0 {
				a = 1
			}
			if a > 8 {
				a = 8
			}
			return a
		}
	}
	a := size
	if a > 8 {
		a = 8
	}
	for _, p := range []int{8, 4, 2, 1} {
		if a >= p {
			return p
		}
	}
	return 1
}

func memAlignUp(v uint64, a int) uint64 {
	if a <= 1 {
		return v
	}
	return (v + uint64(a) - 1) &^ (uint64(a) - 1)
}

// memGlobalsEmitted tracks symbols already placed (tentative .bss entries
// upgraded in place when a later definition carries an initializer).
var memGlobalsEmitted = struct {
	sync.Mutex
	m map[*SPass]map[string]int // sp -> name -> index into sp.syms
}{m: make(map[*SPass]map[string]int)}

// genDataInit emits a file-scope declaration. Section rule (mirrors the
// default backend's .comm/.data split, modernized: tentative/uninit go to
// .bss instead of COMMON, matching gcc -fno-common):
//   - extern without init  -> undefined symtab entry (no storage).
//   - no init              -> .bss (linker-zeroed).
//   - const-foldable init  -> .data (+ R_X86_64_64 relocs for address
//     constants: string literals, &global+off,
//     function designators).
//   - non-foldable init    -> .bss fallback (invalid C; keeps linking).
//
// sym "" means file-scope node n (NodeVarDecl with VarName/IsGlobal).
func (s *SPass) genDataInit(sym string, n *ASTNode) error {
	name := sym
	var decl *ASTNode
	if n != nil && n.Type == NodeVarDecl {
		decl = n
		if name == "" {
			name = n.VarName
		}
	}
	if name == "" {
		return unsupported("anonymous global", n)
	}

	var spelling string
	var dims []int
	var arrSize int
	var init *ASTNode
	var isStatic, isExtern bool
	if s.globals != nil {
		if g, ok := s.globals[name]; ok && g != nil {
			spelling, dims, arrSize, init = g.Type, g.Dims, g.ArraySize, g.Init
			isStatic, isExtern = g.IsStatic, g.IsExternal
		}
	}
	if decl != nil {
		if spelling == "" {
			spelling = decl.DataType
		}
		if dims == nil {
			dims = decl.Dims
		}
		if arrSize == 0 {
			arrSize = decl.ArraySize
		}
		if init == nil && len(decl.Children) > 0 {
			init = decl.Children[0]
		}
		if funcHasWord(decl.DataType, "static") {
			isStatic = true
		}
		if funcHasWord(decl.DataType, "extern") {
			isExtern = true
		}
	}
	spelling = strings.TrimSpace(spelling)
	if spelling == "" {
		return unsupported("global without type "+name, n)
	}
	ty, ok := ctypeOf(spelling, s)
	if !ok {
		return unsupported("global type "+spelling, n)
	}
	size := ty.Size
	if arrSize > 0 {
		size = arrSize * ty.Size
	}
	if size <= 0 {
		size = ty.Size
		if size <= 0 {
			size = 8
		}
	}
	// Unsized char arrays from string literals (`char s[] = "hi"`): the
	// parser records dims [0] with no inference; size from the literal.
	if init != nil && init.Type == NodeString && !ty.IsPtr && ty.Size == 1 {
		cooked := spassUnescape(init.Value)
		if size < len(cooked)+1 {
			size = len(cooked) + 1
		}
		if len(dims) == 1 && dims[0] <= 0 {
			dims = []int{len(cooked) + 1}
		}
	}

	// Register in s.globals so the skeleton lookup and later globalAddr
	// calls see file-scope names even when the driver passed nil.
	if s.globals == nil {
		s.globals = make(map[string]*Symbol)
	}
	if _, present := s.globals[name]; !present {
		s.globals[name] = &Symbol{
			Name: name, IsGlobal: true, IsStatic: isStatic,
			IsExternal: isExtern, Size: size, Type: spelling,
			ArraySize: arrSize, Dims: append([]int(nil), dims...), Init: init,
		}
	}

	// Extern reference: undefined entry so data/text relocs resolve.
	if isExtern && init == nil {
		s.memEnsureSym(name, false)
		return nil
	}
	if init == nil {
		s.memPlaceBSS(name, size, memAlignOf(s, spelling, size), isStatic)
		return nil
	}
	buf := make([]byte, size)
	var refs []memDataRef
	if !s.memFoldInit(buf, 0, init, spelling, dims, size, &refs) {
		s.memPlaceBSS(name, size, memAlignOf(s, spelling, size), isStatic)
		return nil
	}
	al := memAlignOf(s, spelling, size)
	off := memAlignUp(uint64(len(s.data)), al)
	for uint64(len(s.data)) < off {
		s.data = append(s.data, 0)
	}
	s.data = append(s.data, buf...)
	s.memPlaceSym(name, ".data", off, size, !isStatic)
	for _, r := range refs {
		s.relocs = append(s.relocs, ObjReloc{
			Section: ".data", Offset: off + uint64(r.off),
			Symbol: r.sym, Type: R_X86_64_64, Addend: r.add,
		})
		s.memEnsureSym(r.sym, false)
	}
	return nil
}

// memPlaceBSS allocates zeroed storage, upgrading a previous tentative
// entry when this is a redefinition.
func (s *SPass) memPlaceBSS(name string, size int, al int, isStatic bool) {
	off := memAlignUp(s.bss, al)
	s.bss = off + uint64(size)
	s.memPlaceSym(name, ".bss", off, size, !isStatic)
}

// memPlaceSym appends (or upgrades on redefinition) a symbol entry.
func (s *SPass) memPlaceSym(name, section string, off uint64, size int, glob bool) {
	memGlobalsEmitted.Lock()
	byName, ok := memGlobalsEmitted.m[s]
	if !ok {
		byName = make(map[string]int)
		memGlobalsEmitted.m[s] = byName
	}
	if idx, dup := byName[name]; dup {
		e := &s.syms[idx]
		e.Section, e.Offset, e.Size, e.Global = section, off, uint64(size), glob
		memGlobalsEmitted.Unlock()
		return
	}
	byName[name] = len(s.syms)
	memGlobalsEmitted.Unlock()
	s.syms = append(s.syms, ObjSymbol{
		Name: name, Section: section, Offset: off, Global: glob, Size: uint64(size),
	})
}

// memEnsureSym appends an undefined symtab entry for a referenced name
// (extern data, TU-external functions) unless a definition is present or
// forthcoming. Interned labels (".SPS"/".SPC") always get real entries
// from layoutRodata; TU-defined functions get theirs at merge.
func (s *SPass) memEnsureSym(name string, glob bool) {
	_ = glob
	if strings.HasPrefix(name, ".") {
		return
	}
	for _, e := range s.syms {
		if e.Name == name {
			return
		}
	}
	s.mu.Lock()
	_, defined := s.definedFuncs[name]
	s.mu.Unlock()
	if defined {
		return
	}
	s.syms = append(s.syms, ObjSymbol{Name: name})
}

// memDataRef is one absolute address constant inside .data.
type memDataRef struct {
	off int
	sym string
	add int64
}

// memFoldInit folds an initializer into buf[base:base+total]. Array mode
// iff len(dims) > 0; struct mode iff spelling names a definition.
func (s *SPass) memFoldInit(buf []byte, base int, node *ASTNode, spelling string, dims []int, total int, refs *[]memDataRef) bool {
	if node == nil || base < 0 || base+total > len(buf) {
		return node == nil
	}
	ty, ok := ctypeOf(spelling, s)
	if !ok {
		return false
	}
	// Array mode.
	if len(dims) > 0 {
		elemSize := ty.Size
		if elemSize <= 0 {
			elemSize = 8
		}
		count := total / elemSize
		if node.Type == NodeString && elemSize == 1 {
			cooked := spassUnescape(node.Value)
			for i := 0; i < len(cooked)+1 && i < total; i++ {
				var b byte
				if i < len(cooked) {
					b = cooked[i]
				}
				buf[base+i] = b
			}
			return true
		}
		if node.Type != NodeCompoundLiteral {
			// Single value initializes element 0 (rest stay zero).
			if elemSize > total {
				return false
			}
			return s.memFoldInit(buf, base, node, spelling, nil, elemSize, refs)
		}
		for i, ch := range node.Children {
			if i >= count {
				return false // excess elements
			}
			if !s.memFoldInit(buf, base+i*elemSize, ch, spelling, dims[1:], elemSize, refs) {
				return false
			}
		}
		return true
	}
	// Struct mode.
	if st, isStruct := memStructBase(s, spelling); isStruct {
		def, found := s.structs[st]
		if !found {
			return false
		}
		if node.Type != NodeCompoundLiteral {
			// GNU `= {0}`-style scalar zero.
			if v, ok := s.memConstUint(node); ok && v == 0 {
				return true
			}
			return false
		}
		nextPos := 0
		for i, ch := range node.Children {
			var m *StructMember
			if i < len(node.InitFields) && node.InitFields[i] != "" {
				want := node.InitFields[i]
				// Strip a dotted prefix if present ("a.b" -> member "b"
				// must already exist by that plain name; try full first).
				found := -1
				for j := range def.Members {
					if def.Members[j].Name == want {
						found = j
						break
					}
				}
				if found < 0 {
					if idx := strings.LastIndex(want, "."); idx >= 0 {
						short := want[idx+1:]
						for j := range def.Members {
							if def.Members[j].Name == short {
								found = j
								break
							}
						}
					}
				}
				if found < 0 {
					return false
				}
				m = &def.Members[found]
				nextPos = found + 1
			} else {
				// Excess positional elements: skip (GCC warns and keeps
				// the rest; failing the whole fold would zero everything).
				if nextPos >= len(def.Members) {
					continue
				}
				m = &def.Members[nextPos]
				nextPos++
			}
			if !s.memFoldInit(buf, base+m.Offset, ch, m.Type, m.Dims, m.Size, refs) {
				return false
			}
		}
		return true
	}
	// Scalar mode.
	return s.memFoldScalar(buf, base, node, spelling, ty, total, refs)
}

// memFoldScalar folds one scalar initializer (width = min(total, ty.Size)).
func (s *SPass) memFoldScalar(buf []byte, base int, node *ASTNode, spelling string, ty CType, total int, refs *[]memDataRef) bool {
	width := ty.Size
	if width > total {
		width = total
	}
	if width <= 0 || width > 8 {
		return false
	}
	// Pointer slots: address constants (string, &x, decayed arrays,
	// functions, forward refs) or plain zero/non-zero numbers.
	// Unknown names are assumed to be forward-declared globals (their
	// definitions register later in pass 1; relocs resolve at link).
	if ty.IsPtr {
		switch {
		case node.Type == NodeString:
			lab := s.strLit(spassUnescape(node.Value))
			*refs = append(*refs, memDataRef{off: base, sym: lab})
			return true
		case node.Type == NodeAddressOf && len(node.Children) == 1:
			sym, add, ok := s.memStaticAddr(node.Children[0])
			if !ok {
				return false
			}
			*refs = append(*refs, memDataRef{off: base, sym: sym, add: add})
			return true
		case node.Type == NodeIdentifier:
			if _, isFn := s.funcSigs[node.VarName]; isFn {
				*refs = append(*refs, memDataRef{off: base, sym: node.VarName})
				return true
			}
			s.mu.Lock()
			_, defined := s.definedFuncs[node.VarName]
			s.mu.Unlock()
			if defined {
				*refs = append(*refs, memDataRef{off: base, sym: node.VarName})
				return true
			}
			v, ok := s.memConstUint(node)
			if ok {
				for i := 0; i < width; i++ {
					buf[base+i] = byte(v >> (8 * i))
				}
				return true
			}
			// Forward-declared global (or extern): link-time address.
			*refs = append(*refs, memDataRef{off: base, sym: node.VarName})
			return true
		case node.Type == NodeCast && len(node.Children) == 1:
			return s.memFoldScalar(buf, base, node.Children[0], spelling, ty, total, refs)
		default:
			v, ok := s.memConstUint(node)
			if !ok {
				return false
			}
			for i := 0; i < width; i++ {
				buf[base+i] = byte(v >> (8 * i))
			}
			return true
		}
	}
	switch {
	case ty.IsFloat:
		bits, ok := s.memConstFloat(node, ty)
		if !ok {
			return false
		}
		for i := 0; i < width; i++ {
			buf[base+i] = byte(bits >> (8 * i))
		}
		return true
	case node.Type == NodeCast && len(node.Children) == 1:
		return s.memFoldScalar(buf, base, node.Children[0], spelling, ty, total, refs)
	default:
		// Integer/enum constants (address constants in non-pointer
		// slots are invalid C and fail the fold).
		v, ok := s.memConstUint(node)
		if !ok {
			return false
		}
		for i := 0; i < width; i++ {
			buf[base+i] = byte(v >> (8 * i))
		}
		return true
	}
}

// memStaticAddr resolves an address constant to symbol + addend.
func (s *SPass) memStaticAddr(n *ASTNode) (string, int64, bool) {
	switch n.Type {
	case NodeIdentifier:
		if _, ok := s.funcSigs[n.VarName]; ok {
			return n.VarName, 0, true
		}
		s.mu.Lock()
		_, defined := s.definedFuncs[n.VarName]
		s.mu.Unlock()
		if defined {
			return n.VarName, 0, true
		}
		if _, ok := s.globals[n.VarName]; ok {
			return n.VarName, 0, true
		}
		// Forward-declared global (definition registers later in pass 1):
		// resolve at link time.
		return n.VarName, 0, true
	case NodeUnaryOp:
		// Parser forms: &x nests inside memFoldScalar's AddressOf case,
		// *p is not constant.
		if n.Operator == "&" && len(n.Children) == 1 {
			return s.memStaticAddr(n.Children[0])
		}
		return "", 0, false
	case NodeMemberAccess:
		if len(n.Children) < 1 {
			return "", 0, false
		}
		if n.IsPointer {
			return "", 0, false
		}
		baseSym, baseAdd, ok := s.memStaticAddr(n.Children[0])
		if !ok {
			return "", 0, false
		}
		// Struct name from globals (file scope: base is an identifier).
		var st string
		if id, isId := n.Children[0], n.Children[0].Type == NodeIdentifier; isId {
			if g, ok := s.globals[id.VarName]; ok {
				st, _ = memStructBase(s, g.Type)
			}
		}
		if st == "" {
			return "", 0, false
		}
		off, _, ok := structMemberOff(s, st, n.MemberName)
		if !ok {
			return "", 0, false
		}
		return baseSym, baseAdd + int64(off), true
	case NodeArrayAccess:
		if len(n.Children) < 2 {
			return "", 0, false
		}
		baseSym, baseAdd, ok := s.memStaticAddr(n.Children[0])
		if !ok {
			return "", 0, false
		}
		// Const-fold all index levels for global roots.
		cur := n.Children[0]
		levels := []int64{}
		for cur != nil && cur.Type == NodeArrayAccess {
			v, ok := s.memConstInt(cur.Children[1])
			if !ok {
				return "", 0, false
			}
			levels = append([]int64{v}, levels...)
			cur = cur.Children[0]
		}
		elemSize, strides, ok := s.memStaticStrides(cur, len(levels))
		if !ok {
			return "", 0, false
		}
		_ = elemSize
		acc := baseAdd
		for i, lv := range levels {
			acc += lv * int64(strides[i])
		}
		return baseSym, acc, true
	}
	return "", 0, false
}

// memStaticStrides returns per-level strides for a global array root.
func (s *SPass) memStaticStrides(root *ASTNode, nLevels int) (elemSize int, strides []int, ok bool) {
	var dims []int
	var ty CType
	switch root.Type {
	case NodeIdentifier:
		g, found := s.globals[root.VarName]
		if !found || (g.ArraySize == 0 && len(g.Dims) == 0) {
			return 0, nil, false
		}
		var valid bool
		ty, valid = ctypeOf(g.Type, s)
		if !valid {
			return 0, nil, false
		}
		dims = g.Dims
	case NodeMemberAccess:
		return 0, nil, false
	default:
		return 0, nil, false
	}
	if len(dims) < nLevels {
		return 0, nil, false
	}
	strides = make([]int, nLevels)
	for l := 0; l < nLevels; l++ {
		st := ty.Size
		for _, d := range dims[l+1:] {
			st *= d
		}
		strides[l] = st
	}
	return ty.Size, strides, true
}

// memConstUint folds an integer constant expression (two's-complement).
func (s *SPass) memConstUint(n *ASTNode) (uint64, bool) {
	if n == nil {
		return 0, false
	}
	switch n.Type {
	case NodeNumber:
		if isFloatSpelling(n.Value) {
			return 0, false
		}
		v, _ := parseIntLit(n.Value)
		return v, true
	case NodeUnaryOp:
		if len(n.Children) != 1 {
			return 0, false
		}
		v, ok := s.memConstUint(n.Children[0])
		if !ok {
			return 0, false
		}
		switch n.Operator {
		case "-":
			return uint64(-int64(v)), true
		case "+":
			return v, true
		case "~":
			return ^v, true
		case "!":
			if v == 0 {
				return 1, true
			}
			return 0, true
		}
		return 0, false
	case NodeBinaryOp:
		if len(n.Children) != 2 {
			return 0, false
		}
		a, ok := s.memConstUint(n.Children[0])
		if !ok {
			return 0, false
		}
		b, ok := s.memConstUint(n.Children[1])
		if !ok {
			return 0, false
		}
		sa, sb := int64(a), int64(b)
		switch n.Operator {
		case "+":
			return a + b, true
		case "-":
			return a - b, true
		case "*":
			return a * b, true
		case "/":
			if sb == 0 {
				return 0, false
			}
			return uint64(sa / sb), true
		case "%":
			if sb == 0 {
				return 0, false
			}
			return uint64(sa % sb), true
		case "<<":
			return a << (b & 63), true
		case ">>":
			return uint64(sa >> (b & 63)), true
		case "&":
			return a & b, true
		case "|":
			return a | b, true
		case "^":
			return a ^ b, true
		}
		return 0, false
	case NodeIdentifier:
		if v, ok := s.enums[n.VarName]; ok {
			return uint64(int64(v)), true
		}
		return 0, false
	case NodeCast:
		if len(n.Children) != 1 {
			return 0, false
		}
		return s.memConstUint(n.Children[0])
	case NodeSizeof:
		if len(n.Children) != 1 {
			return 0, false
		}
		if sz, ok := s.memSizeof(n.Children[0]); ok {
			return uint64(sz), true
		}
		return 0, false
	}
	return 0, false
}

// memConstInt is the signed view of memConstUint.
func (s *SPass) memConstInt(n *ASTNode) (int64, bool) {
	v, ok := s.memConstUint(n)
	return int64(v), ok
}

// memConstFloat folds a float constant to IEEE-754 bits.
func (s *SPass) memConstFloat(n *ASTNode, ty CType) (uint64, bool) {
	if n == nil {
		return 0, false
	}
	if n.Type == NodeUnaryOp && len(n.Children) == 1 &&
		(n.Operator == "-" || n.Operator == "+") {
		bits, ok := s.memConstFloat(n.Children[0], ty)
		if !ok {
			return 0, false
		}
		if n.Operator == "-" {
			if ty.Size == 4 {
				bits ^= 1 << 31
			} else {
				bits ^= 1 << 63
			}
		}
		return bits, true
	}
	if n.Type == NodeCast && len(n.Children) == 1 {
		return s.memConstFloat(n.Children[0], ty)
	}
	if n.Type != NodeNumber || !isFloatSpelling(n.Value) {
		// Integer literal in float context.
		if v, ok := s.memConstUint(n); ok {
			val := n.Value
			_ = val
			if ty.Size == 4 {
				return uint64(floatBits32(fmt.Sprintf("%d", int64(v)))), true
			}
			return doubleBits64(fmt.Sprintf("%d", int64(v))), true
		}
		return 0, false
	}
	bits, _ := floatBitsOf(n.Value, ty)
	return bits, true
}

// memSizeof computes a static sizeof for simple expressions.
func (s *SPass) memSizeof(n *ASTNode) (int, bool) {
	if n == nil {
		return 0, false
	}
	if n.DataType != "" {
		if ty, ok := ctypeOf(n.DataType, s); ok {
			return ty.Size, true
		}
	}
	switch n.Type {
	case NodeIdentifier:
		if g, ok := s.globals[n.VarName]; ok && g != nil && g.Size > 0 {
			return g.Size, true
		}
		return 0, false
	case NodeMemberAccess:
		return 0, false
	case NodeString:
		return len(spassUnescape(n.Value)) + 1, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// genCompoundInit: runtime aggregate initialization
// ---------------------------------------------------------------------------

// genCompoundInit initializes the aggregate at dest from a compound
// literal (or a lone scalar/string for degenerate calls). The destination
// is zeroed first, so gaps, padding, and tails are always correct.
// dest.Size should carry the total object size for array inits (structs
// use their definition size); the element count additionally falls back
// to the number of initializer children.
func (f *funcState) genCompoundInit(n *ASTNode, dest Addr) {
	if n == nil {
		return
	}
	dest = memNormAddr(dest)
	base := dest
	if base.Sym != "" || base.Index >= 0 {
		base = Addr{RegBase: f.memAddrToReg(base), Disp: 0, Index: -1, Scale: 1, Size: dest.Size}
	}
	// String init always means a char array here (STMT routes
	// `char *p = "s"` through its own pointer store, never here).
	if n.Type == NodeString {
		cooked := spassUnescape(n.Value) + "\x00"
		total := dest.Size
		if total < len(cooked) {
			total = len(cooked)
		}
		if total <= 0 {
			total = len(cooked)
		}
		if len(cooked) > total {
			cooked = cooked[:total]
		}
		f.memZeroRange(base, total)
		f.memStoreBytes(base, []byte(cooked))
		return
	}
	if n.Type != NodeCompoundLiteral {
		if dest.Size > 8 {
			// Scalar zero into an aggregate = whole-zero (already the
			// common parser placeholder for skipped nested braces).
			if v, ok := f.sp.memConstUint(n); ok && v == 0 {
				total := dest.Size
				if total <= 0 {
					total = 8
				}
				f.memZeroRange(base, total)
				return
			}
		}
		f.store(dest, f.genRVal(n))
		return
	}
	spelling := strings.TrimSpace(n.DataType)
	if st, isStruct := memStructBase(f.sp, spelling); isStruct {
		def, found := f.sp.structs[st]
		if !found {
			panic(unsupported("unknown struct "+spelling, n))
		}
		f.memZeroRange(base, def.Size)
		nextPos := 0
		for i, ch := range n.Children {
			var m *StructMember
			if i < len(n.InitFields) && n.InitFields[i] != "" {
				want := n.InitFields[i]
				found := -1
				for j := range def.Members {
					if def.Members[j].Name == want {
						found = j
						break
					}
				}
				if found < 0 {
					panic(unsupported("unknown field "+want, n))
				}
				m = &def.Members[found]
				nextPos = found + 1
			} else {
				if nextPos >= len(def.Members) {
					panic(unsupported("excess struct initializer", n))
				}
				m = &def.Members[nextPos]
				nextPos++
			}
			f.memInitMember(base, m, ch, n)
		}
		return
	}
	// Array init. Element type from the literal's (element) spelling.
	elemTy, ok := ctypeOf(spelling, f.sp)
	if !ok || elemTy.Size <= 0 {
		panic(unsupported("array init type "+spelling, n))
	}
	count := len(n.Children)
	total := dest.Size
	if total < count*elemTy.Size {
		total = count * elemTy.Size
	}
	if total <= 0 {
		total = elemTy.Size
	}
	if count == 0 {
		f.memZeroRange(base, total)
		return
	}
	f.memZeroRange(base, total)
	limit := count
	if max := total / elemTy.Size; limit > max {
		limit = max
	}
	for i := 0; i < limit; i++ {
		ea := base
		ea.Disp += i * elemTy.Size
		ea.Size = elemTy.Size
		f.memInitElem(ea, n.Children[i], spelling, elemTy, n)
	}
}

// memInitMember initializes one struct member slot.
func (f *funcState) memInitMember(base Addr, m *StructMember, ch *ASTNode, n *ASTNode) {
	ma := base
	ma.Disp += m.Offset
	ma.Size = m.Size
	// Nested struct literal.
	if ch.Type == NodeCompoundLiteral {
		if st, ok := memStructBase(f.sp, m.Type); ok {
			if _, found := f.sp.structs[st]; found && len(m.Dims) == 0 {
				ma.Size = m.Size
				f.genCompoundInit(ch, ma)
				return
			}
		}
		// Array member with brace init.
		if len(m.Dims) > 0 {
			elemTy, ok := ctypeOf(m.Type, f.sp)
			if !ok || elemTy.Size <= 0 {
				panic(unsupported("array member type "+m.Type, n))
			}
			elems := ch.Children
			limit := len(elems)
			if max := m.Size / elemTy.Size; limit > max {
				limit = max
			}
			for i := 0; i < limit; i++ {
				ea := ma
				ea.Disp += i * elemTy.Size
				ea.Size = elemTy.Size
				f.memInitElem(ea, elems[i], m.Type, elemTy, n)
			}
			return
		}
		// Scalar-typed member with braces: take child 0 if any.
		if len(ch.Children) > 0 {
			f.memInitMember(base, m, ch.Children[0], n)
		}
		return
	}
	// Const-zero into an aggregate member: already zeroed.
	if m.Size > 8 || len(m.Dims) > 0 {
		if v, ok := f.sp.memConstUint(ch); ok && v == 0 {
			return
		}
	}
	// char array from string.
	if ch.Type == NodeString && len(m.Dims) > 0 {
		if mt, ok := ctypeOf(m.Type, f.sp); ok && mt.Size == 1 {
			cooked := spassUnescape(ch.Value) + "\x00"
			if len(cooked) > m.Size {
				cooked = cooked[:m.Size]
			}
			f.memStoreBytes(ma, []byte(cooked))
			return
		}
	}
	f.memInitElem(ma, ch, m.Type, CType{}, n)
}

// memInitElem initializes one scalar/pointer/aggregate element slot:
// compounds recurse, strings take addresses for pointer slots, &x takes
// the address, struct rvalues copy, everything else converts + stores.
func (f *funcState) memInitElem(ea Addr, ch *ASTNode, spelling string, elemTy CType, n *ASTNode) {
	if elemTy.Size == 0 {
		if t, ok := ctypeOf(spelling, f.sp); ok {
			elemTy = t
		}
	}
	if ch.Type == NodeCompoundLiteral {
		slot := ea.Size
		if slot <= 0 {
			slot = elemTy.Size
		}
		cp := ea
		cp.Size = slot
		f.genCompoundInit(ch, cp)
		return
	}
	if ch.Type == NodeString && elemTy.IsPtr {
		lab := f.sp.strLit(spassUnescape(ch.Value))
		r := f.scratchReg()
		f.encLeaRegMem(r, Addr{Sym: lab, RegBase: -1, Disp: 0, Index: -1, Scale: 1, Size: 8})
		ea.Size = 8
		f.encMovMemReg(ea, r, CType{Name: "ptr", Size: 8})
		return
	}
	if (ch.Type == NodeAddressOf || (ch.Type == NodeUnaryOp && ch.Operator == "&")) && len(ch.Children) == 1 {
		a := f.addrOf(ch.Children[0])
		r := f.memAddrToReg(a)
		ea.Size = 8
		f.encMovMemReg(ea, r, CType{Name: "ptr", Size: 8})
		return
	}
	if ch.Type == NodeIdentifier {
		if _, isFn := f.sp.funcSigs[ch.VarName]; isFn {
			r := f.scratchReg()
			f.encLeaRegMem(r, Addr{Sym: ch.VarName, RegBase: -1, Disp: 0, Index: -1, Scale: 1, Size: 8})
			ea.Size = 8
			f.encMovMemReg(ea, r, CType{Name: "ptr", Size: 8})
			return
		}
	}
	v := f.genRVal(ch)
	if memIsStruct(f.sp, v.Type) {
		if v.Kind != KMem {
			panic(&spassError{what: "struct value not in memory", node: ch})
		}
		sz := ea.Size
		if sz <= 0 {
			sz = v.Mem.Size
		}
		f.copyStruct(ea, v.Mem, sz)
		return
	}
	if mt, ok := ctypeOf(spelling, f.sp); ok && mt.Size > 0 && mt.Size <= 8 {
		slot := ea.Size
		_ = slot
		v = f.convert(v, mt)
		ea.Size = mt.Size
	}
	f.store(ea, v)
}
