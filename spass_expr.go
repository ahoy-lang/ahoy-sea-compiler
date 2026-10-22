// spass_expr.go: rvalue expression codegen for the single-pass backend.
//
// EXPR agent. Evaluates every C expression node to an SVal (constant,
// register, xmm, or memory) with O0 spill discipline: at most one scratch
// value is live across any recursive genRVal call. Binary operators evaluate
// LEFT, spill it to a temp slot, evaluate RIGHT, then compute (TCC-style).
// Calls evaluate args left-to-right, spilling every KReg/KXMM arg to a temp
// slot immediately, then emit the call.
//
// Cross-agent contracts (see also the integration report):
//
//	STMT (spass_stmt.go): genStmt, spStmtMaybeConvert, spStmtStore,
//	  spStmtCopy, spStmtZero, spStmtChunkTy. EXPR reuses all of these.
//	  STMT must call MemNoteArray for array locals (bare-array decay and
//	  multi-dim indexing depend on it).
//	MEM (spass_mem.go): addrOf, convert, genDataInit, genCompoundInit,
//	  MemNoteArray/memLookupArray, globalAddr, structMemberOff.
//	  convert must not clobber a KReg/KXMM source and must sign/zero-extend
//	  int<->int per the target type (EXPR defensively narrows signed-32
//	  results via exConv).
//	Skeleton fixes applied by EXPR (all load-bearing, see report): frame
//	reserve + epilogue rewind, finish() UND synthesis, genTU Block unwrap,
//	store() KMem materialization, genFunction error return, encMovMemImm
//	RIP addend + imm16 size, float compare/branch zero-basis, JP opcode,
//	REX for 8-bit regs, XMM REX-field and prefix-order fixes.
package main

import (
	"strings"
)

// ---------------------------------------------------------------------------
// Small shared types
// ---------------------------------------------------------------------------

var (
	exIntTy    = CType{Name: "int", Size: 4, Signed: true}
	exLongTy   = CType{Name: "long", Size: 8, Signed: true}
	exULongTy  = CType{Name: "unsigned long", Size: 8}
	exDoubleTy = CType{Name: "double", Size: 8, IsFloat: true}
	exFloatTy  = CType{Name: "float", Size: 4, IsFloat: true}
	exPtrTy    = CType{Name: "ptr", Size: 8, IsPtr: true}
)

// ---------------------------------------------------------------------------
// Entry point (replaces the stub in spass.go; signature is frozen)
// ---------------------------------------------------------------------------

// genRVal compiles an rvalue expression to an SVal.
func (f *funcState) genRVal(n *ASTNode) SVal {
	if n == nil {
		return SVal{Kind: KVoid}
	}
	f.exEnsureParams()
	switch n.Type {
	case NodeNumber:
		return f.exNumber(n)
	case NodeIdentifier:
		return f.exIdentifier(n)
	case NodeString:
		return f.exStringLit(n)
	case NodeBinaryOp:
		return f.exBinary(n)
	case NodeUnaryOp:
		return f.exUnary(n)
	case NodeDereference:
		if len(n.Children) < 1 {
			panic(unsupported("dereference without operand", n))
		}
		return f.exDerefVal(n.Children[0])
	case NodeAddressOf:
		if len(n.Children) < 1 {
			panic(unsupported("address-of without operand", n))
		}
		return f.exAddrOfVal(n.Children[0])
	case NodeCast:
		return f.exCast(n)
	case NodeTernary:
		return f.exTernary(n)
	case NodeComma:
		return f.exComma(n)
	case NodeSizeof:
		return f.exSizeof(n)
	case NodeOffsetof:
		return f.exOffsetof(n)
	case NodeCall:
		return f.exCall(n)
	case NodeCompoundLiteral:
		return f.exCompound(n)
	case NodeBlock:
		return f.exBlock(n)
	case NodeArrayAccess, NodeMemberAccess:
		return f.exLValLoad(n)
	case NodeAssignment:
		return f.exAssign(n)
	default:
		panic(unsupported("expression", n))
	}
}

// exEnsureParams registers function params in f.locals (idempotent).
// The frozen genFunction homes params but never binds their names, so
// lookup() would fail on every parameter use. Absent-only insertion keeps
// shadowing intact: an inner declaration wins and is left alone.
func (f *funcState) exEnsureParams() {
	for i := range f.params {
		p := &f.params[i]
		if p.Name == "" || p.Name == "void" {
			continue
		}
		if _, ok := f.locals[p.Name]; ok {
			continue
		}
		lv := &LocalVar{Name: p.Name, Type: p.Type, Offset: p.Offset}
		f.locals[p.Name] = lv
	}
}

// ---------------------------------------------------------------------------
// Leaves: numbers, identifiers, strings
// ---------------------------------------------------------------------------

func (f *funcState) exNumber(n *ASTNode) SVal {
	ty, ok := f.sp.typeOfNode(n)
	if !ok {
		ty = exIntTy
	}
	imm := n.Value
	if imm == "" {
		imm = itoaSp(n.IntValue)
	}
	return SVal{Kind: KConst, Type: ty, Imm: imm}
}

func (f *funcState) exIdentifier(n *ASTNode) SVal {
	name := n.VarName
	if name == "" {
		name = n.Name
	}
	if lv, ok := f.lookup(name); ok {
		base := Addr{Index: -1, Scale: 1}
		if lv.Global {
			base.Sym = lv.Sym
		} else {
			base.RegBase = REG_RBP
			base.Disp = lv.Offset
		}
		base.Size = lv.Type.Size
		// Bare arrays decay to a pointer to the first element.
		if f.exIsArrayName(name, lv) {
			elem := lv.Type.Name
			if idx := strings.Index(elem, "["); idx >= 0 {
				elem = strings.TrimSpace(elem[:idx])
			}
			if elem == "" {
				elem = "char"
			}
			r := f.scratchReg()
			f.encLeaRegMem(r, base)
			return SVal{Kind: KReg, Type: CType{Name: elem + "*", Size: 8, IsPtr: true}, Reg: r}
		}
		// Struct/union objects stay in memory (member access and struct
		// assignment need the address, not a register copy).
		if exIsStructTy(lv.Type, f.sp) {
			return SVal{Kind: KMem, Type: lv.Type, Mem: base}
		}
		return f.load(base, lv.Type)
	}
	if v, ok := f.sp.enums[name]; ok {
		return SVal{Kind: KConst, Type: exIntTy, Imm: itoaSp(v)}
	}
	if _, ok := f.sp.funcSigs[name]; ok {
		// Function designator: address of the text symbol.
		r := f.scratchReg()
		f.encLeaRegMem(r, Addr{Sym: name, RegBase: -1, Index: -1, Scale: 1, Size: 8})
		return SVal{Kind: KReg, Type: exPtrTy, Reg: r}
	}
	panic(unsupported("unknown identifier "+name, n))
}

// exStringLit interns the literal and returns its ADDRESS in a register.
// (A KMem value would load the first 8 character bytes as an integer, which
// is wrong everywhere a string flows: stores, calls, pointer arithmetic.
// The address form is correct in all of those positions.)
func (f *funcState) exStringLit(n *ASTNode) SVal {
	raw := n.Value
	// Lexer lexemes exclude quotes, but tolerate quoted shapes too.
	if len(raw) >= 2 && strings.HasPrefix(raw, "\"") {
		// Strip all adjacent quoted segments: `"a""b"` -> `a""b` -> `ab`.
		inner := raw[1:]
		if strings.HasSuffix(inner, "\"") {
			inner = inner[:len(inner)-1]
		}
		raw = strings.ReplaceAll(inner, "\"\"", "")
	}
	cooked := spassUnescape(raw)
	lab := f.sp.strLit(cooked)
	r := f.scratchReg()
	f.encLeaRegMem(r, Addr{Sym: lab, RegBase: -1, Index: -1, Scale: 1, Size: 8})
	return SVal{Kind: KReg, Type: CType{Name: "char*", Size: 8, IsPtr: true}, Reg: r}
}

// ---------------------------------------------------------------------------
// Types: promotion, usual conversions, struct/pointer helpers
// ---------------------------------------------------------------------------

// exIsUnsigned mirrors is.isUnsignedIntType: unsigned AND rank >= int.
// Pointers and sub-int types never count (they promote to signed int).
func exIsUnsigned(t CType) bool {
	if t.IsPtr || t.IsFloat || t.IsVoid {
		return false
	}
	if t.Size < 4 {
		return false
	}
	return !t.Signed
}

func exIs64(t CType) bool { return t.Size == 8 || t.IsPtr }

// exPromote applies C integer promotion: sub-int types become int.
func exPromote(t CType) CType {
	if t.IsPtr || t.IsFloat || t.IsVoid || t.Size >= 4 {
		return t
	}
	return exIntTy
}

// exArithType mirrors propagateArithType (instruction_selection.go) for the
// non-pointer, non-float lattice: double > float > 64-bit/unsigned rules.
func exArithType(op string, lt, rt CType) CType {
	if lt.IsFloat || rt.IsFloat {
		if (lt.IsFloat && lt.Size == 8) || (rt.IsFloat && rt.Size == 8) {
			return exDoubleTy
		}
		return exFloatTy
	}
	if op == "/" || op == "%" {
		l64, r64 := exIs64(lt), exIs64(rt)
		lU, rU := exIsUnsigned(lt), exIsUnsigned(rt)
		if l64 || r64 {
			if lU || rU {
				return exULongTy
			}
			return exLongTy
		}
		if lU || rU {
			if lU {
				return lt
			}
			return rt
		}
		if lt.Name != "" {
			return exPromote(lt)
		}
		return exPromote(rt)
	}
	lU, rU := exIsUnsigned(lt), exIsUnsigned(rt)
	if lU || rU {
		if lU {
			return lt
		}
		return rt
	}
	if lt.Name != "" {
		return exPromote(lt)
	}
	return exPromote(rt)
}

// exPointee strips one pointer level: "int*" -> "int".
func exPointee(sp *SPass, ptrTy CType, n *ASTNode) CType {
	base := strings.TrimSpace(ptrTy.Name)
	if !strings.HasSuffix(base, "*") {
		panic(unsupported("dereference of non-pointer "+ptrTy.Name, n))
	}
	elem := strings.TrimSpace(strings.TrimSuffix(base, "*"))
	if elem == "" {
		panic(unsupported("dereference of abstract pointer", n))
	}
	ty, ok := ctypeOf(elem, sp)
	if !ok {
		panic(unsupported("unknown pointee type "+elem, n))
	}
	return ty
}

// exElemSize returns the scaled size for pointer arithmetic.
func exElemSize(sp *SPass, ptrTy CType, n *ASTNode) int {
	et := exPointee(sp, ptrTy, n)
	if et.Size <= 0 {
		panic(unsupported("pointer arithmetic on incomplete type", n))
	}
	return et.Size
}

// exIsStructTy reports whether ty spells a struct/union (through typedefs).
func exIsStructTy(ty CType, sp *SPass) bool {
	if ty.IsPtr || ty.IsFloat || ty.IsVoid {
		return false
	}
	t := strings.TrimSpace(ty.Name)
	for i := 0; i < 32; i++ {
		if aliased, ok := sp.typedefs[t]; ok {
			t = strings.TrimSpace(aliased)
			continue
		}
		break
	}
	for _, q := range []string{"const ", "volatile ", "static ", "extern ", "register "} {
		t = strings.TrimSpace(strings.TrimPrefix(t, q))
	}
	return strings.HasPrefix(t, "struct ") || strings.HasPrefix(t, "union ")
}

// exStructMember resolves member name in the struct/union spelled by base.
// Array members decay to element pointers (isArr). Delegates to MEM's
// memMemberInfo for the single source of layout truth.
func exStructMember(sp *SPass, base, member string) (off, size int, ty CType, isArr bool, ok bool) {
	t := strings.TrimSpace(base)
	for i := 0; i < 32; i++ {
		if aliased, ok := sp.typedefs[t]; ok {
			t = strings.TrimSpace(aliased)
			continue
		}
		break
	}
	for len(t) > 0 && t[len(t)-1] == '*' {
		t = strings.TrimSpace(t[:len(t)-1])
	}
	t = stripStorageClass(t)
	sname := t
	if strings.HasPrefix(t, "struct ") {
		sname = strings.TrimSpace(t[len("struct "):])
	} else if strings.HasPrefix(t, "union ") {
		sname = strings.TrimSpace(t[len("union "):])
	} else {
		return 0, 0, CType{}, false, false
	}
	off, size, ty, dims, ok := memMemberInfo(sp, sname, member)
	if !ok {
		return 0, 0, CType{}, false, false
	}
	if len(dims) > 0 {
		elem := ty.Name
		if idx := strings.Index(elem, "["); idx >= 0 {
			elem = strings.TrimSpace(elem[:idx])
		}
		if elem == "" {
			elem = "char"
		}
		return off, size, CType{Name: elem + "*", Size: 8, IsPtr: true}, true, true
	}
	return off, size, ty, false, true
}

// ---------------------------------------------------------------------------
// Lvalue/rvalue type queries (no emission)
// ---------------------------------------------------------------------------

// exArrayResultIsArray reports whether indexing n yields a still-array
// value (address, not load): true when the index chain consumes fewer
// levels than the base's declared dimensions. Genuine pointers (including
// pointers-to-pointers and call results — C functions can't return
// arrays) always load. Unknown bases keep the legacy decay behavior.
func (f *funcState) exArrayResultIsArray(n *ASTNode) bool {
	// Count indices in the full chain and find the root.
	k := 0
	b := n
	for b.Type == NodeArrayAccess && len(b.Children) > 0 {
		k++
		b = b.Children[0]
	}
	// Identifier root: dims from MEM's side table, globals, params, or
	// bracket-carrying type spellings.
	if b.Type == NodeIdentifier {
		name := b.VarName
		if name == "" {
			name = b.Name
		}
		if d, ok := memLookupArray(f, name); ok && len(d.dims) > 0 {
			return k < len(d.dims)
		}
		if g, ok := f.sp.globals[name]; ok && g != nil && len(g.Dims) > 0 {
			return k < len(g.Dims)
		}
		if lv, ok := f.lookup(name); ok {
			if c := strings.Count(lv.Type.Name, "["); c > 0 {
				return k < c
			}
		}
		for i := range f.params {
			if f.params[i].Name == name {
				if c := strings.Count(f.params[i].Type.Name, "["); c > 0 {
					return k < c
				}
			}
		}
		return false
	}
	// Member root: array members carry dims in the struct layout.
	if b.Type == NodeMemberAccess && len(b.Children) > 0 {
		if st, ok := f.memStructNameOf(b.Children[0], b.IsPointer); ok {
			if _, _, _, mdims, ok := memMemberInfo(f.sp, st, b.MemberName); ok && len(mdims) > 0 {
				return k < len(mdims)
			}
		}
		return false
	}
	// Dereference/call/compound roots are pointers, never arrays.
	if b.Type == NodeDereference || b.Type == NodeCall || b.Type == NodeCompoundLiteral {
		return false
	}
	return true
}

// exLValType returns the type of the object denoted by an lvalue node.
func (f *funcState) exLValType(n *ASTNode) (CType, bool) {
	switch n.Type {
	case NodeIdentifier:
		name := n.VarName
		if name == "" {
			name = n.Name
		}
		if lv, ok := f.lookup(name); ok {
			return lv.Type, true
		}
		// File-scope globals (same fallback as exRValType).
		if _, gty, ok := f.sp.globalAddr(name); ok {
			return gty, true
		}
		return CType{}, false
	case NodeArrayAccess:
		if len(n.Children) < 1 {
			return CType{}, false
		}
		base := n.Children[0]
		// Tag indexed array bases for the decay table (best effort; STMT's
		// declaration hook with real dims takes precedence).
		if base.Type == NodeIdentifier {
			bname := base.VarName
			if bname == "" {
				bname = base.Name
			}
			f.exNoteArrayName(bname)
		}
		// Identifier base: LocalVar.Type is already the element type.
		if base.Type == NodeIdentifier {
			t, ok := f.exLValType(base)
			if !ok {
				return CType{}, false
			}
			// Multi-dim partial index (this node consumes one level but
			// dimensions remain): decay to an element pointer so callers
			// take the address instead of loading a bogus scalar.
			if dims := f.exBaseDims(base); len(dims) > 1 {
				elem := t.Name
				if idx := strings.Index(elem, "["); idx >= 0 {
					elem = strings.TrimSpace(elem[:idx])
				}
				if elem == "" {
					elem = "char"
				}
				return CType{Name: elem + "*", Size: 8, IsPtr: true}, true
			}
			// Single-dim array: the element itself (no deref), even when
			// that element is itself a pointer (arrays of pointers).
			// Only genuine pointers need pointee resolution: IsPtr
			// alone can't tell them apart (bell: global/local arrays
			// of opaque struct pointers).
			if f.exBaseIsArray(base) {
				return t, true
			}
			if t.IsPtr {
				return f.exPointeeOf(t, n)
			}
			// Multi-dim elements stay arrays: decay to pointer.
			if strings.Contains(t.Name, "[") {
				return CType{Name: t.Name + "*", Size: 8, IsPtr: true}, true
			}
			return t, true
		}
		t, ok := f.exRValType(base)
		if !ok {
			return CType{}, false
		}
		if t.IsPtr {
			return f.exPointeeOf(t, n)
		}
		return t, true
	case NodeMemberAccess:
		return f.exMemberType(n)
	case NodeDereference:
		if len(n.Children) < 1 {
			return CType{}, false
		}
		t, ok := f.exRValType(n.Children[0])
		if !ok || !t.IsPtr {
			return CType{}, false
		}
		return f.exPointeeOf(t, n)
	case NodeUnaryOp:
		if n.Operator == "*" && len(n.Children) > 0 {
			t, ok := f.exRValType(n.Children[0])
			if !ok || !t.IsPtr {
				return CType{}, false
			}
			return f.exPointeeOf(t, n)
		}
		return CType{}, false
	case NodeCompoundLiteral:
		if n.DataType == "" {
			return CType{}, false
		}
		return ctypeOf(n.DataType, f.sp)
	default:
		return CType{}, false
	}
}

func (f *funcState) exPointeeOf(t CType, n *ASTNode) (CType, bool) {
	base := strings.TrimSpace(t.Name)
	if !strings.HasSuffix(base, "*") {
		return CType{}, false
	}
	elem := strings.TrimSpace(strings.TrimSuffix(base, "*"))
	if elem == "" {
		return CType{}, false
	}
	// Pointers to arrays decay: the object is address-only.
	if strings.Contains(elem, "[") {
		return CType{Name: elem + "*", Size: 8, IsPtr: true}, true
	}
	ty, ok := ctypeOf(elem, f.sp)
	if !ok {
		return CType{}, false
	}
	return ty, true
}

// exMemberType resolves a member access to the member's (possibly decayed)
// type: array members decay to element pointers.
func (f *funcState) exMemberType(n *ASTNode) (CType, bool) {
	if len(n.Children) < 1 {
		return CType{}, false
	}
	base := n.Children[0]
	var spelling string
	if n.IsPointer {
		t, ok := f.exRValType(base)
		if !ok || !t.IsPtr {
			return CType{}, false
		}
		spelling = strings.TrimSpace(t.Name)
		for len(spelling) > 0 && spelling[len(spelling)-1] == '*' {
			spelling = strings.TrimSpace(spelling[:len(spelling)-1])
		}
	} else {
		t, ok := f.exLValType(base)
		if !ok {
			// Base may be an rvalue struct (call result, ternary...).
			t2, ok2 := f.exRValType(base)
			if !ok2 {
				return CType{}, false
			}
			t = t2
		}
		spelling = t.Name
	}
	_, _, mty, _, ok := exStructMember(f.sp, spelling, n.MemberName)
	if !ok {
		return CType{}, false
	}
	return mty, true
}

// exMemberIsArray reports whether a member access denotes an array object
// (as opposed to a genuine pointer member): same resolution as
// exMemberType but returns the decay flag instead of the type.
func (f *funcState) exMemberIsArray(n *ASTNode) bool {
	if n.Type != NodeMemberAccess || len(n.Children) == 0 {
		return false
	}
	base := n.Children[0]
	var spelling string
	if n.IsPointer {
		t, ok := f.exRValType(base)
		if !ok || !t.IsPtr {
			return false
		}
		spelling = strings.TrimSpace(t.Name)
		for len(spelling) > 0 && spelling[len(spelling)-1] == '*' {
			spelling = strings.TrimSpace(spelling[:len(spelling)-1])
		}
	} else {
		t, ok := f.exLValType(base)
		if !ok {
			t2, ok2 := f.exRValType(base)
			if !ok2 {
				return false
			}
			t = t2
		}
		spelling = t.Name
	}
	_, _, _, isArr, ok := exStructMember(f.sp, spelling, n.MemberName)
	return ok && isArr
}
// ok=false means "unknown" (callers fall back to runtime SVal types).
func (f *funcState) exRValType(n *ASTNode) (CType, bool) {
	if n == nil {
		return CType{}, false
	}
	switch n.Type {
	case NodeNumber:
		return f.sp.typeOfNode(n)
	case NodeString:
		return CType{Name: "char*", Size: 8, IsPtr: true}, true
	case NodeIdentifier:
		name := n.VarName
		if name == "" {
			name = n.Name
		}
		if lv, ok := f.lookup(name); ok {
			if f.exIsArrayName(name, lv) {
				elem := lv.Type.Name
				if idx := strings.Index(elem, "["); idx >= 0 {
					elem = strings.TrimSpace(elem[:idx])
				}
				if elem == "" {
					elem = "char"
				}
				return CType{Name: elem + "*", Size: 8, IsPtr: true}, true
			}
			return lv.Type, true
		}
		if _, ok := f.sp.enums[name]; ok {
			return exIntTy, true
		}
		// File-scope globals (MEM's table): locals shadow them, same as
		// genRVal's identifier path.
		if _, gty, ok := f.sp.globalAddr(name); ok {
			return gty, true
		}
		return CType{}, false
	case NodeBlock:
		// Statement expression: value type = last meaningful child.
		if len(n.Children) == 0 {
			return CType{}, false
		}
		last := n.Children[len(n.Children)-1]
		if last.Type == NodeExprStmt && len(last.Children) > 0 {
			last = last.Children[0]
		}
		return f.exRValType(last)
	case NodeCall:
		return f.exCallRetType(n)
	case NodeCast:
		if n.DataType == "" {
			return CType{}, false
		}
		return ctypeOf(n.DataType, f.sp)
	case NodeUnaryOp:
		switch n.Operator {
		case "!":
			return exIntTy, true
		case "~":
			if len(n.Children) == 0 {
				return CType{}, false
			}
			t, ok := f.exRValType(n.Children[0])
			if !ok {
				return CType{}, false
			}
			return exPromote(t), true
		case "-", "+":
			if len(n.Children) == 0 {
				return CType{}, false
			}
			return f.exRValType(n.Children[0])
		case "&":
			if len(n.Children) == 0 {
				return CType{}, false
			}
			t, ok := f.exLValType(n.Children[0])
			if !ok {
				t2, ok2 := f.exRValType(n.Children[0])
				if !ok2 {
					return CType{}, false
				}
				t = t2
			}
			return CType{Name: t.Name + "*", Size: 8, IsPtr: true}, true
		case "*", "++", "--", "++_post", "--_post":
			if len(n.Children) == 0 {
				return CType{}, false
			}
			if n.Operator == "*" {
				t, ok := f.exRValType(n.Children[0])
				if !ok || !t.IsPtr {
					return CType{}, false
				}
				return f.exPointeeOf(t, n)
			}
			return f.exRValType(n.Children[0])
		}
		return CType{}, false
	case NodeDereference:
		if len(n.Children) == 0 {
			return CType{}, false
		}
		t, ok := f.exRValType(n.Children[0])
		if !ok || !t.IsPtr {
			return CType{}, false
		}
		return f.exPointeeOf(t, n)
	case NodeBinaryOp:
		switch n.Operator {
		case "==", "!=", "<", "<=", ">", ">=", "&&", "||":
			return exIntTy, true
		}
		if len(n.Children) != 2 {
			return CType{}, false
		}
		lt, ok1 := f.exRValType(n.Children[0])
		rt, ok2 := f.exRValType(n.Children[1])
		if !ok1 || !ok2 {
			return CType{}, false
		}
		if lt.IsPtr || rt.IsPtr {
			if lt.IsPtr && rt.IsPtr {
				if n.Operator == "-" {
					return exLongTy, true
				}
				return exIntTy, true
			}
			if lt.IsPtr {
				return lt, true
			}
			return rt, true
		}
		return exArithType(n.Operator, lt, rt), true
	case NodeTernary:
		if len(n.Children) != 3 {
			return CType{}, false
		}
		t1, ok1 := f.exRValType(n.Children[1])
		t2, ok2 := f.exRValType(n.Children[2])
		return exCommonOf(t1, t2, ok1, ok2), true
	case NodeComma:
		if len(n.Children) == 0 {
			return CType{}, false
		}
		return f.exRValType(n.Children[len(n.Children)-1])
	case NodeArrayAccess, NodeMemberAccess:
		return f.exLValType(n)
	case NodeSizeof, NodeOffsetof:
		return exULongTy, true
	case NodeAssignment:
		if len(n.Children) == 0 {
			return CType{}, false
		}
		return f.exLValType(n.Children[0])
	case NodeCompoundLiteral:
		if n.DataType == "" {
			return CType{}, false
		}
		return ctypeOf(n.DataType, f.sp)
	default:
		return CType{}, false
	}
}

// ---------------------------------------------------------------------------
// Array decay detection (backed by MEM's dims side table)
// ---------------------------------------------------------------------------

func (f *funcState) exNoteArrayName(name string) {
	// Best-effort tag for bases seen through indexing. STMT's VarDecl
	// lowering must call MemNoteArray with real dims (per MEM's header
	// contract); this only fills the gap for use-before-note, and never
	// overwrites a real entry.
	if _, ok := memLookupArray(f, name); ok {
		return
	}
	if lv, ok := f.lookup(name); ok {
		MemNoteArray(f, name, lv.Type, nil)
	}
}

// exDecayIdent forces a bare identifier to its address (array decay for
// un-noted arrays, &x semantics for scalars). Used in pointer contexts.
func (f *funcState) exDecayIdent(n *ASTNode) SVal {
	name := n.VarName
	if name == "" {
		name = n.Name
	}
	if _, isEnum := f.sp.enums[name]; isEnum {
		panic(unsupported("enum in pointer context "+name, n))
	}
	lv, ok := f.lookup(name)
	if !ok {
		panic(unsupported("unknown identifier "+name, n))
	}
	base := Addr{Index: -1, Scale: 1, Size: 8}
	if lv.Global {
		base.Sym = lv.Sym
	} else {
		base.RegBase = REG_RBP
		base.Disp = lv.Offset
	}
	elem := lv.Type.Name
	if idx := strings.Index(elem, "["); idx >= 0 {
		elem = strings.TrimSpace(elem[:idx])
	}
	if elem == "" {
		elem = "char"
	}
	r := f.scratchReg()
	f.encLeaRegMem(r, base)
	return SVal{Kind: KReg, Type: CType{Name: elem + "*", Size: 8, IsPtr: true}, Reg: r}
}

// exIsArrayName reports whether a local is an array object (decays to a
// pointer in rvalue position). Detection: MEM's dims table, or bracket
// spellings in LocalVar.Type.Name (forward-compatible if STMT records them).
func (f *funcState) exIsArrayName(name string, lv *LocalVar) bool {
	if lv != nil {
		if strings.Contains(lv.Type.Name, "[") {
			return true
		}
		if _, ok := memLookupArray(f, name); ok {
			return true
		}
	}
	// File-scope arrays live only in the TU table (MEM's genDataInit);
	// without this, globals decay to scalar loads (bell's ahoy_glue_dir).
	if g, ok := f.sp.globals[name]; ok && g != nil && len(g.Dims) > 0 {
		return true
	}
	return false
}

// exArrayDims returns the declared dimensions of an ArrayAccess root
// (locals via MEM's side table, globals via the TU table), or nil.
func (f *funcState) exArrayDims(n *ASTNode) []int {
	b := n
	for b.Type == NodeArrayAccess && len(b.Children) > 0 {
		b = b.Children[0]
	}
	if b.Type != NodeIdentifier {
		return nil
	}
	name := b.VarName
	if name == "" {
		name = b.Name
	}
	if d, ok := memLookupArray(f, name); ok && len(d.dims) > 0 {
		return d.dims
	}
	if g, ok := f.sp.globals[name]; ok && g != nil && len(g.Dims) > 0 {
		return g.Dims
	}
	return nil
}

// exArrayDepth counts index levels in an ArrayAccess chain.
func (f *funcState) exArrayDepth(n *ASTNode) int {
	k := 0
	for b := n; b.Type == NodeArrayAccess && len(b.Children) > 0; {
		k++
		b = b.Children[0]
	}
	return k
}

// exArrayElem returns the element type of an array-typed node.
func (f *funcState) exArrayElem(n *ASTNode) (CType, bool) {
	t, ok := f.exLValType(n)
	if !ok {
		return CType{}, false
	}
	// Strip one array level: if t already decayed to pointer, the
	// pointee is the element.
	if t.IsPtr {
		base := strings.TrimSpace(t.Name)
		for len(base) > 0 && base[len(base)-1] == '*' {
			base = strings.TrimSpace(base[:len(base)-1])
		}
		if et, ok := ctypeOf(base, f.sp); ok {
			return et, true
		}
	}
	return t, true
}

// exBaseDims returns the declared dimensions of an identifier array base
// (locals via MEM's side table, globals via the TU table, brackets as a
// fallback), or nil if unknown/not an array.
func (f *funcState) exBaseDims(n *ASTNode) []int {
	name := n.VarName
	if name == "" {
		name = n.Name
	}
	if d, ok := memLookupArray(f, name); ok && len(d.dims) > 0 {
		return d.dims
	}
	if g, ok := f.sp.globals[name]; ok && g != nil && len(g.Dims) > 0 {
		return g.Dims
	}
	if lv, ok := f.lookup(name); ok {
		if idx := strings.Index(lv.Type.Name, "["); idx >= 0 {
			// Bracket-carrying spelling: count dimensions (best effort).
			rest := lv.Type.Name[idx:]
			n := strings.Count(rest, "[")
			if n > 0 {
				out := make([]int, n)
				for i := range out {
					out[i] = 1
				}
				return out
			}
		}
	}
	return nil
}

// exBaseIsArray reports whether an indexed identifier base is a true array
// (as opposed to a pointer): locals via MEM's dims side table, globals via
// their declared Dims. Bracket-carrying type names are decided by the
// caller; this only answers the dims-table question, requiring real dims
// (best-effort empty notes don't count).
func (f *funcState) exBaseIsArray(n *ASTNode) bool {
	if n.Type != NodeIdentifier {
		return false
	}
	name := n.VarName
	if name == "" {
		name = n.Name
	}
	if d, ok := memLookupArray(f, name); ok && len(d.dims) > 0 {
		return true
	}
	if g, ok := f.sp.globals[name]; ok && g != nil && len(g.Dims) > 0 {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Spill helpers, booleans, narrowing
// ---------------------------------------------------------------------------

// exSpill stores a register-held value to a temp slot (no-op otherwise).
func (f *funcState) exSpill(v SVal) SVal {
	if v.Kind != KReg && v.Kind != KXMM {
		return v
	}
	size := v.Type.Size
	if size <= 0 {
		size = 8
	}
	slot := f.tempSlot(size)
	slot.Size = size
	f.store(slot, v)
	return SVal{Kind: KMem, Type: v.Type, Mem: slot}
}

// exBool reduces any scalar to int 0/1 in a register. (Does NOT use the
// skeleton truthy(), whose setcc opcode is wrong; see file header.)
func (f *funcState) exBool(v SVal) SVal {
	m := f.materialize(v)
	r := f.scratchReg()
	switch m.Kind {
	case KReg:
		f.encTestRegReg(m.Reg, m.Reg)
		f.encSetcc(0x95, r) // setne
		f.encMovExtend(r, r, 1, 4, false)
		return SVal{Kind: KReg, Type: exIntTy, Reg: r}
	case KXMM:
		// Compare against +0.0 (a self-compare always reports equal).
		// Unordered (NaN) is false: jp skips the set, leaving 0.
		f.encMovRegImm(r, "0", exIntTy)
		z := f.scratchXMM()
		f.encSSE(0x66, 0x57, z, z)
		f.encUcomisd(m.Reg, z)
		done := f.localLabel("ex_nant")
		f.encJcc(0x8A, done)
		f.encSetcc(0x95, r)
		f.defineLabel(done)
		f.encMovExtend(r, r, 1, 4, false)
		return SVal{Kind: KReg, Type: exIntTy, Reg: r}
	default:
		panic(unsupported("truth value of "+m.Type.Name, nil))
	}
}

// exConv converts v to `to`, then re-establishes the 64-bit invariant for
// signed 32-bit results. Truncating 8->4-bit conversions can leave a
// zero-extended (dirty-high) value in the register; every 64-bit consumer
// below (cmp, div, shifts) needs sign-extension. Narrowing a clean value
// is a harmless no-op, so this is unconditional for KReg int32 results.
func (f *funcState) exConv(v SVal, to CType) SVal {
	r := f.spStmtMaybeConvert(v, to)
	if r.Kind == KReg && !to.IsFloat && !to.IsPtr && !to.IsVoid && to.Signed && to.Size == 4 {
		f.exNarrow(r.Reg, true)
		r.Type = to
	}
	return r
}

// exSetccOp converts a cmpCond jcc code to the matching setcc opcode byte.
func exSetccOp(op string, unsigned bool) byte {
	return 0x90 | (cmpCond(op, unsigned) & 0x0F)
}

// exNarrow re-establishes the 64-bit invariant after a 32-bit op: truncate
// to 32 bits, then sign-extend back for signed types.
func (f *funcState) exNarrow(r int, signed bool) {
	f.encMovRegReg(r, r, 4) // movl: truncate + zero high
	if signed {
		f.exEncMovslq(r, r)
	}
}

// exEncMovslq emits movslq dst, src (missing from the frozen encoder).
func (f *funcState) exEncMovslq(dst, src int) {
	df, de := regField(dst)
	sf, se := regField(src)
	f.text = append(f.text, rexByte(true, de, false, se)...)
	f.text = append(f.text, 0x63, 0xC0|(df<<3)|sf)
}

// exNormAddr folds a Sym+Dips address into a register base. The encoder
// ignores Disp/Index for Sym operands, so any nonzero offset must go
// through LEA. The result is used immediately (no calls between), so a
// round-robin scratch is safe here.
func (f *funcState) exNormAddr(a Addr) Addr {
	if a.Sym == "" || (a.Disp == 0 && a.Index < 0) {
		return a
	}
	r := f.scratchReg()
	f.encLeaRegMem(r, Addr{Sym: a.Sym, RegBase: -1, Index: -1, Scale: 1, Size: 8})
	return Addr{RegBase: r, Disp: a.Disp, Index: a.Index, Scale: a.Scale, Size: a.Size}
}

// exPinAddr spills scratch-held address components to a temp slot, yielding
// a frame-stable address that survives arbitrary recursion. Pure frame
// (RBP/RSP, no index) and clean Sym addresses pass through untouched.
func (f *funcState) exPinAddr(a Addr) Addr {
	stable := false
	if a.Sym != "" {
		stable = a.Disp == 0 && a.Index < 0
	} else if a.RegBase == REG_RBP || a.RegBase == REG_RSP {
		stable = a.Index < 0
	}
	if stable {
		return a
	}
	r := f.scratchReg()
	f.encLeaRegMem(r, a)
	slot := f.tempSlot(8)
	slot.Size = 8
	f.store(slot, SVal{Kind: KReg, Type: exPtrTy, Reg: r})
	return Addr{RegBase: REG_RBP, Disp: slot.Disp, Index: -1, Scale: 1, Size: a.Size}
}

// exLValLoad is the rvalue of a member/array access: addrOf + load, with
// array-element decay (an element that is itself an array yields its
// address, not a load).
func (f *funcState) exLValLoad(n *ASTNode) SVal {
	ty, ok := f.exLValType(n)
	if !ok {
		panic(unsupported("untyped access", n))
	}
	// Array results decay to addresses, not loads: indexed elements with
	// remaining dimensions (exArrayResultIsArray), and array-typed members
	// (e.g. entry->d_name, a struct's char array used as a pointer).
	decayed := false
	if ty.IsPtr && n.Type == NodeArrayAccess && f.exArrayResultIsArray(n) {
		decayed = true
	}
	if ty.IsPtr && n.Type == NodeMemberAccess && f.exMemberIsArray(n) {
		decayed = true
	}
	if decayed {
		// Decayed array: address value.
		a := f.exNormAddr(f.addrOf(n))
		r := f.scratchReg()
		f.encLeaRegMem(r, a)
		return SVal{Kind: KReg, Type: ty, Reg: r}
	}
	a := f.exNormAddr(f.addrOf(n))
	a.Size = ty.Size
	if ty.Size <= 0 {
		panic(unsupported("zero-size load", n))
	}
	// Struct objects stay in memory.
	if exIsStructTy(ty, f.sp) || ty.Size > 8 {
		a.Size = ty.Size
		return SVal{Kind: KMem, Type: ty, Mem: a}
	}
	return f.load(a, ty)
}

// ---------------------------------------------------------------------------
// Binary operators
// ---------------------------------------------------------------------------

func exIsCompareOp(op string) bool {
	switch op {
	case "==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}

func (f *funcState) exBinary(n *ASTNode) SVal {
	op := n.Operator
	if op == "&&" || op == "||" {
		return f.exLogical(n, op)
	}
	if len(n.Children) != 2 {
		panic(unsupported("binary operator "+op, n))
	}
	// Spill LEFT immediately: it must not sit in a register while the
	// right side evaluates (calls, idiv, ... clobber scratches).
	// exArith spills both sides again before converting (right side +
	// idempotent left), so conversions never see a live register.
	lv := f.exSpill(f.genRVal(n.Children[0]))
	rv := f.genRVal(n.Children[1])
	return f.exArith(op, lv, rv, n)
}

// exArith computes op on two evaluated operands (no further recursion).
// Shared by exBinary and compound assignment. Both sides are spilled
// before any conversion: conversions allocate scratches, and a live side
// would be clobbered (the 2-entry xmm pool wraps after two rounds).
func (f *funcState) exArith(op string, lv, rv SVal, n *ASTNode) SVal {
	lv = f.exSpill(lv)
	rv = f.exSpill(rv)
	lt, rt := lv.Type, rv.Type
	if lt.IsPtr || rt.IsPtr {
		return f.exPtrBin(op, lv, rv, n)
	}
	if lt.IsFloat || rt.IsFloat {
		ct := exDoubleTy
		if !(lt.IsFloat && lt.Size == 8) && !(rt.IsFloat && rt.Size == 8) {
			ct = exFloatTy
		}
		lc := f.spStmtMaybeConvert(lv, ct)
		rc := f.spStmtMaybeConvert(rv, ct)
		return f.exFloatBin(op, lc, rc, ct, n)
	}
	ct := exArithType(op, lt, rt)
	lc := f.exConv(lv, ct)
	rc := f.exConv(rv, ct)
	if exIsCompareOp(op) {
		return f.exIntCmp(op, lc, rc, ct, n)
	}
	return f.exIntBin(op, lc, rc, ct, n)
}

// exLogical evaluates && / || with short-circuit branches. Result int 0/1.
// The result register is allocated only after the left side is consumed:
// allocating it first could hand out the left value's own register and the
// zeroing mov would destroy it (e.g. a call result in RAX). The result
// itself lives in a temp SLOT across the right side: a scratch register
// would be clobbered by any call the right side contains (all scratches
// are caller-saved).
func (f *funcState) exLogical(n *ASTNode, op string) SVal {
	lv := f.genRVal(n.Children[0])
	slot := f.tempSlot(4)
	slot.Size = 4
	f.encMovMemImm(slot, "0", exIntTy)
	if op == "||" {
		trueLabel := f.localLabel("ex_or_t")
		endLabel := f.localLabel("ex_or_e")
		f.branchOn(lv, true, trueLabel)
		rv := f.genRVal(n.Children[1])
		f.branchOn(rv, true, trueLabel)
		f.encJmp(endLabel)
		f.defineLabel(trueLabel)
		f.encMovMemImm(slot, "1", exIntTy)
		f.defineLabel(endLabel)
		return f.load(slot, exIntTy)
	}
	falseLabel := f.localLabel("ex_and_f")
	endLabel := f.localLabel("ex_and_e")
	f.branchOn(lv, false, falseLabel)
	rv := f.genRVal(n.Children[1])
	f.branchOn(rv, false, falseLabel)
	f.encMovMemImm(slot, "1", exIntTy)
	f.encJmp(endLabel)
	f.defineLabel(falseLabel)
	f.defineLabel(endLabel)
	return f.load(slot, exIntTy)
}

// exIntCmp emits a 64-bit signed/unsigned comparison. Operands are already
// converted to the common type and are 64-bit-invariant-clean, so a single
// 64-bit cmp is exactly C semantics (verified: extensions preserve order).
func (f *funcState) exIntCmp(op string, lv, rv SVal, ct CType, n *ASTNode) SVal {
	lm := f.materialize(lv)
	rm := f.materialize(rv)
	if lm.Kind != KReg || rm.Kind != KReg {
		panic(unsupported("integer comparison", n))
	}
	unsigned := !ct.Signed && !ct.IsFloat
	f.encALURegReg("cmp", lm.Reg, rm.Reg)
	r := f.scratchReg()
	f.encSetcc(exSetccOp(op, unsigned), r)
	f.encMovExtend(r, r, 1, 4, false)
	return SVal{Kind: KReg, Type: exIntTy, Reg: r}
}

func (f *funcState) exIntBin(op string, lv, rv SVal, ct CType, n *ASTNode) SVal {
	lm := f.materialize(lv)
	rm := f.materialize(rv)
	if lm.Kind != KReg || rm.Kind != KReg {
		panic(unsupported("integer operator "+op, n))
	}
	lr, rr := lm.Reg, rm.Reg
	switch op {
	case "+", "-", "&", "|", "^":
		alu := map[string]string{"+": "add", "-": "sub", "&": "and", "|": "or", "^": "xor"}[op]
		f.encALURegReg(alu, lr, rr)
		if ct.Size < 8 {
			f.exNarrow(lr, ct.Signed)
		}
		return SVal{Kind: KReg, Type: ct, Reg: lr}
	case "*":
		f.encImulRegReg(lr, rr)
		if ct.Size < 8 {
			f.exNarrow(lr, ct.Signed)
		}
		return SVal{Kind: KReg, Type: ct, Reg: lr}
	case "<<", ">>":
		return f.exShift(op, lr, rv, ct, n)
	case "/", "%":
		return f.exDivMod(op, lr, rr, ct, n)
	default:
		panic(unsupported("integer operator "+op, n))
	}
}

// exShift emits sal/sar/shr with the count in %cl (or an immediate).
func (f *funcState) exShift(op string, lr int, rv SVal, ct CType, n *ASTNode) SVal {
	dir := "sal"
	if op == ">>" {
		if ct.Signed {
			dir = "sar"
		} else {
			dir = "shr"
		}
	}
	// Constant count: immediate form.
	if rv.Kind == KConst {
		if isFloatSpelling(rv.Imm) {
			panic(unsupported("float shift count", n))
		}
		v, _ := parseIntLit(rv.Imm)
		c := v & 0x3F
		f.encShift(dir, lr, &c, true)
		if ct.Size < 8 {
			f.exNarrow(lr, ct.Signed)
		}
		return SVal{Kind: KReg, Type: ct, Reg: lr}
	}
	rm := f.materialize(rv)
	if rm.Kind != KReg {
		panic(unsupported("shift count", n))
	}
	// Bounce the value out of RCX before installing the count there.
	if lr == REG_RCX {
		t := f.scratchReg()
		f.encMovRegReg(t, lr, 8)
		lr = t
	}
	if rm.Reg != REG_RCX {
		f.encMovRegReg(REG_RCX, rm.Reg, 8)
	}
	f.encShift(dir, lr, nil, true)
	if ct.Size < 8 {
		f.exNarrow(lr, ct.Signed)
	}
	return SVal{Kind: KReg, Type: ct, Reg: lr}
}

// exDivMod emits 64-bit idiv/div. 64-bit division of extended operands is
// exactly C 32-bit semantics (quotient/remainder low halves match, high
// halves are the correct extension), and avoids the INT_MIN/-1 fault.
func (f *funcState) exDivMod(op string, lr, rr int, ct CType, n *ASTNode) SVal {
	_ = n
	div := rr
	if div == REG_RAX || div == REG_RDX {
		t := f.scratchReg()
		for t == REG_RAX || t == REG_RDX {
			t = f.scratchReg()
		}
		f.encMovRegReg(t, div, 8)
		div = t
	}
	if lr != REG_RAX {
		f.encMovRegReg(REG_RAX, lr, 8)
	}
	if ct.Signed {
		f.encCqo()
	} else {
		f.encALURegReg("xor", REG_RDX, REG_RDX)
	}
	f.encDiv(div, true, ct.Signed)
	got := REG_RAX
	if op == "%" {
		got = REG_RDX
	}
	out := f.scratchReg()
	f.encMovRegReg(out, got, 8)
	return SVal{Kind: KReg, Type: ct, Reg: out}
}

// exFloatBin emits float arithmetic/comparison at the common type.
func (f *funcState) exFloatBin(op string, lv, rv SVal, ct CType, n *ASTNode) SVal {
	lm := f.materialize(lv)
	rm := f.materialize(rv)
	if lm.Kind != KXMM || rm.Kind != KXMM {
		panic(unsupported("float operator "+op, n))
	}
	if exIsCompareOp(op) {
		// ucomisd reads 64 bits; float operands carry garbage in the
		// high half, so compare in double (cvtss2sd is exact). Pin the
		// left conversion in a temp slot first: each conversion takes
		// two xmm dispenser rounds, and the 2-entry pool would wrap
		// around and clobber the left side otherwise.
		if ct.Size == 4 {
			lc := f.spStmtMaybeConvert(lv, exDoubleTy)
			lcs := f.exSpill(lc)
			rc := f.spStmtMaybeConvert(rv, exDoubleTy)
			rm := f.materialize(rc)
			lm = f.materialize(lcs)
			if lm.Kind != KXMM || rm.Kind != KXMM {
				panic(unsupported("float comparison", n))
			}
			return f.exFloatCmp(op, lm.Reg, rm.Reg)
		}
		return f.exFloatCmp(op, lm.Reg, rm.Reg)
	}
	var prefix byte = 0xF2
	if ct.Size == 4 {
		prefix = 0xF3
	}
	var opc byte
	switch op {
	case "+":
		opc = 0x58
	case "-":
		opc = 0x5C
	case "*":
		opc = 0x59
	case "/":
		opc = 0x5E
	default:
		panic(unsupported("float operator "+op, n))
	}
	f.encSSE(prefix, opc, lm.Reg, rm.Reg)
	return SVal{Kind: KXMM, Type: ct, Reg: lm.Reg}
}

// exFloatCmp mirrors emitFloatCompare NaN semantics with branches:
// ==,<,<= are false when unordered; != is true; >,>= fall out of flags.
func (f *funcState) exFloatCmp(op string, lr, rr int) SVal {
	f.encUcomisd(lr, rr)
	r := f.scratchReg()
	switch op {
	case "==", "<", "<=":
		set := map[string]byte{"==": 0x94, "<": 0x92, "<=": 0x96}[op]
		lf := f.localLabel("ex_fcf")
		le := f.localLabel("ex_fce")
		f.encJcc(0x8A, lf) // jp: unordered -> false
		f.encSetcc(set, r)
		f.encMovExtend(r, r, 1, 4, false)
		f.encJmp(le)
		f.defineLabel(lf)
		f.encALURegReg("xor", r, r)
		f.defineLabel(le)
	case "!=":
		lt := f.localLabel("ex_fnt")
		le := f.localLabel("ex_fne")
		f.encJcc(0x8A, lt) // jp: unordered -> true
		f.encSetcc(0x95, r)
		f.encMovExtend(r, r, 1, 4, false)
		f.encJmp(le)
		f.defineLabel(lt)
		f.encMovRegImm(r, "1", exIntTy)
		f.defineLabel(le)
	case ">", ">=":
		set := byte(0x97)
		if op == ">=" {
			set = 0x93
		}
		f.encSetcc(set, r)
		f.encMovExtend(r, r, 1, 4, false)
	default:
		panic(unsupported("float comparison "+op, nil))
	}
	return SVal{Kind: KReg, Type: exIntTy, Reg: r}
}

// exPtrBin handles pointer arithmetic and pointer comparisons.
func (f *funcState) exPtrBin(op string, lv, rv SVal, n *ASTNode) SVal {
	lt, rt := lv.Type, rv.Type
	if exIsCompareOp(op) {
		lm := f.materialize(lv)
		rm := f.materialize(rv)
		if lm.Kind != KReg || rm.Kind != KReg {
			panic(unsupported("pointer comparison", n))
		}
		f.encALURegReg("cmp", lm.Reg, rm.Reg)
		r := f.scratchReg()
		f.encSetcc(exSetccOp(op, true), r)
		f.encMovExtend(r, r, 1, 4, false)
		return SVal{Kind: KReg, Type: exIntTy, Reg: r}
	}
	// ptr +/- int, int + ptr, ptr - ptr.
	lPtr, rPtr := lt.IsPtr, rt.IsPtr
	if lPtr && rPtr {
		if op != "-" {
			panic(unsupported("pointer "+op+" pointer", n))
		}
		lm := f.materialize(lv)
		rm := f.materialize(rv)
		elem := exElemSize(f.sp, lt, n)
		if elem == 1 {
			f.encALURegReg("sub", lm.Reg, rm.Reg)
			return SVal{Kind: KReg, Type: exLongTy, Reg: lm.Reg}
		}
		f.encALURegReg("sub", lm.Reg, rm.Reg)
		if isPow2Sp(elem) {
			c := uint64(log2Sp(elem))
			f.encShift("sar", lm.Reg, &c, true)
			return SVal{Kind: KReg, Type: exLongTy, Reg: lm.Reg}
		}
		t := f.scratchReg()
		for t == REG_RAX || t == REG_RDX {
			// Fixed idiv protocol regs: keep the divisor elsewhere.
			t = f.scratchReg()
		}
		f.encMovRegImm(t, itoaSp(elem), exLongTy)
		f.encMovRegReg(REG_RAX, lm.Reg, 8)
		f.encCqo()
		f.encDiv(t, true, true)
		out := f.scratchReg()
		f.encMovRegReg(out, REG_RAX, 8)
		return SVal{Kind: KReg, Type: exLongTy, Reg: out}
	}
	var pv SVal
	var iv SVal
	var pty CType
	neg := false
	switch {
	case lPtr && op == "+":
		pv, iv, pty = lv, rv, lt
	case lPtr && op == "-":
		pv, iv, pty = lv, rv, lt
		neg = true
	case rPtr && op == "+":
		pv, iv, pty = rv, lv, rt
	default:
		panic(unsupported("pointer operator "+op, n))
	}
	elem := exElemSize(f.sp, pty, n)
	pm := f.materialize(pv)
	im := f.materialize(iv)
	if pm.Kind != KReg || im.Kind != KReg {
		panic(unsupported("pointer arithmetic", n))
	}
	if elem != 1 {
		t := f.scratchReg()
		f.encMovRegReg(t, im.Reg, 8)
		f.encImulRegImm(t, t, uint64(elem))
		if neg {
			f.encALURegReg("sub", pm.Reg, t)
		} else {
			f.encALURegReg("add", pm.Reg, t)
		}
	} else {
		if neg {
			f.encALURegReg("sub", pm.Reg, im.Reg)
		} else {
			f.encALURegReg("add", pm.Reg, im.Reg)
		}
	}
	return SVal{Kind: KReg, Type: pty, Reg: pm.Reg}
}

// ---------------------------------------------------------------------------
// Unary operators
// ---------------------------------------------------------------------------

func (f *funcState) exUnary(n *ASTNode) SVal {
	op := n.Operator
	if len(n.Children) < 1 && op != "++" && op != "--" {
		panic(unsupported("unary operator "+op, n))
	}
	switch op {
	case "&":
		return f.exAddrOfVal(n.Children[0])
	case "*":
		return f.exDerefVal(n.Children[0])
	case "!":
		return f.exNot(f.genRVal(n.Children[0]))
	case "~", "-", "+":
		return f.exArithUnary(op, f.exSpill(f.genRVal(n.Children[0])), n)
	case "++", "--", "++_post", "--_post":
		return f.exIncr(op, n.Children[0], n)
	default:
		panic(unsupported("unary operator "+op, n))
	}
}

// exNot implements logical NOT (branch-free): bool the value, xor with 1.
func (f *funcState) exNot(v SVal) SVal {
	b := f.exBool(v)
	f.encALURegImm("xor", b.Reg, 1)
	return b
}

func (f *funcState) exArithUnary(op string, v SVal, n *ASTNode) SVal {
	ty := v.Type
	if ty.IsPtr {
		panic(unsupported("unary "+op+" on pointer", n))
	}
	if ty.IsFloat {
		m := f.materialize(v)
		if m.Kind != KXMM {
			panic(unsupported("unary "+op+" on float", n))
		}
		if op == "+" {
			return m
		}
		if op == "~" {
			panic(unsupported("~ on float", n))
		}
		zero := f.scratchXMM()
		f.encSSE(0x66, 0x57, zero, zero) // xorpd: 0.0
		var prefix byte = 0xF2
		if m.Type.Size == 4 {
			prefix = 0xF3
		}
		f.encSSE(prefix, 0x5C, zero, m.Reg) // 0.0 - x
		return SVal{Kind: KXMM, Type: m.Type, Reg: zero}
	}
	ct := exPromote(ty)
	c := f.exConv(v, ct)
	m := f.materialize(c)
	if m.Kind != KReg {
		panic(unsupported("unary "+op, n))
	}
	switch op {
	case "+":
		if ct.Size < 8 {
			f.exNarrow(m.Reg, ct.Signed)
		}
		return SVal{Kind: KReg, Type: ct, Reg: m.Reg}
	case "-":
		f.encNeg(m.Reg)
		if ct.Size < 8 {
			f.exNarrow(m.Reg, ct.Signed)
		}
		return SVal{Kind: KReg, Type: ct, Reg: m.Reg}
	case "~":
		f.encNot(m.Reg)
		if ct.Size < 8 {
			f.exNarrow(m.Reg, ct.Signed)
		}
		return SVal{Kind: KReg, Type: ct, Reg: m.Reg}
	default:
		panic(unsupported("unary "+op, n))
	}
}

// exAddrOfVal evaluates &x to a register-held address.
func (f *funcState) exAddrOfVal(x *ASTNode) SVal {
	// &*p is just p's value; &function is the function itself.
	if x.Type == NodeUnaryOp && x.Operator == "*" && len(x.Children) > 0 {
		return f.genRVal(x.Children[0])
	}
	if x.Type == NodeIdentifier {
		name := x.VarName
		if name == "" {
			name = x.Name
		}
		if _, ok := f.sp.funcSigs[name]; ok {
			if _, ok2 := f.lookup(name); !ok2 {
				r := f.scratchReg()
				f.encLeaRegMem(r, Addr{Sym: name, RegBase: -1, Index: -1, Scale: 1, Size: 8})
				return SVal{Kind: KReg, Type: exPtrTy, Reg: r}
			}
		}
	}
	a := f.exNormAddr(f.addrOf(x))
	ty, ok := f.exLValType(x)
	var pty CType
	if ok {
		pty = CType{Name: ty.Name + "*", Size: 8, IsPtr: true}
	} else {
		pty = exPtrTy
	}
	r := f.scratchReg()
	f.encLeaRegMem(r, a)
	return SVal{Kind: KReg, Type: pty, Reg: r}
}

// exDerefVal evaluates *p (rvalue): materialize the pointer, load through
// it immediately. The scratch base is used with no intervening recursion,
// the one sanctioned exception to the spill rule.
func (f *funcState) exDerefVal(p *ASTNode) SVal {
	// *&x is x's value (no pointer traffic at all).
	if p.Type == NodeUnaryOp && p.Operator == "&" && len(p.Children) > 0 {
		return f.exLValLoad(p.Children[0])
	}
	pv := f.genRVal(p)
	m := f.materialize(pv)
	if m.Kind != KReg {
		panic(unsupported("dereference of non-pointer", p))
	}
	if m.Type.IsFloat {
		panic(unsupported("dereference of float", p))
	}
	// Non-pointer integer types here are addresses by construction (e.g.
	// `(char*)17` retyped by convert); exPointee validates the spelling.
	et := exPointee(f.sp, m.Type, p)
	a := Addr{RegBase: m.Reg, Disp: 0, Index: -1, Scale: 1, Size: et.Size}
	if exIsStructTy(et, f.sp) || et.Size > 8 {
		a.Size = et.Size
		return SVal{Kind: KMem, Type: et, Mem: a}
	}
	return f.load(a, et)
}

// exIncr implements prefix/postfix ++/-- (int, float, pointer).
func (f *funcState) exIncr(op string, x *ASTNode, n *ASTNode) SVal {
	post := op == "++_post" || op == "--_post"
	delta := op == "++" || op == "++_post"
	a := f.exNormAddr(f.addrOf(x))
	ty, ok := f.exLValType(x)
	if !ok {
		panic(unsupported("increment of untyped lvalue", n))
	}
	if exIsStructTy(ty, f.sp) || ty.Size > 8 {
		panic(unsupported("increment of aggregate", n))
	}
	a.Size = ty.Size
	cur := f.load(a, ty)
	if cur.Kind != KReg && cur.Kind != KXMM {
		panic(unsupported("increment load", n))
	}
	curReg := cur.Reg
	if ty.IsFloat {
		var prefix byte = 0xF2
		if ty.Size == 4 {
			prefix = 0xF3
		}
		nv := f.scratchXMM()
		f.encMovXMMReg(nv, curReg)
		one := f.materialize(SVal{Kind: KConst, Type: ty, Imm: "1"})
		if one.Kind != KXMM {
			panic(unsupported("float one", n))
		}
		if delta {
			f.encSSE(prefix, 0x58, nv, one.Reg)
		} else {
			f.encSSE(prefix, 0x5C, nv, one.Reg)
		}
		f.store(a, SVal{Kind: KXMM, Type: ty, Reg: nv})
		if post {
			return SVal{Kind: KXMM, Type: ty, Reg: curReg}
		}
		return SVal{Kind: KXMM, Type: ty, Reg: nv}
	}
	nv := f.scratchReg()
	f.encMovRegReg(nv, curReg, 8)
	step := uint64(1)
	if ty.IsPtr {
		step = uint64(exElemSize(f.sp, ty, n))
	}
	if delta {
		f.encALURegImm("add", nv, step)
	} else {
		f.encALURegImm("sub", nv, step)
	}
	if !ty.IsPtr && ty.Size < 8 {
		f.exNarrow(nv, ty.Signed)
	}
	f.store(a, SVal{Kind: KReg, Type: ty, Reg: nv})
	if post {
		return SVal{Kind: KReg, Type: ty, Reg: curReg}
	}
	return SVal{Kind: KReg, Type: ty, Reg: nv}
}

// ---------------------------------------------------------------------------
// Assignment (also serves STMT's ExprStmt/For paths via genRVal)
// ---------------------------------------------------------------------------

func (f *funcState) exAssign(n *ASTNode) SVal {
	if len(n.Children) != 2 {
		panic(unsupported("malformed assignment", n))
	}
	lhs, rhs := n.Children[0], n.Children[1]
	op := n.Operator
	if op != "=" {
		return f.exAssignOp(op[:len(op)-1], lhs, rhs, n)
	}
	// Plain store: evaluate RHS first (spilled), then resolve the address.
	// Single evaluation of each side, no register held across recursion.
	v := f.exSpill(f.genRVal(rhs))
	a := f.exNormAddr(f.addrOf(lhs))
	ty, ok := f.exLValType(lhs)
	if !ok {
		panic(unsupported("assignment to untyped lvalue", n))
	}
	return f.exAssignStore(a, v, ty, n)
}

// exAssignOp implements op= via load/compute/store. The RHS evaluates first
// (spilled); the address resolves after, is pinned to memory, and is then
// used immediately for load and (post-compute) store — never held in a
// register across recursion. Single evaluation of the LHS address.
func (f *funcState) exAssignOp(baseop string, lhs, rhs *ASTNode, n *ASTNode) SVal {
	r := f.exSpill(f.genRVal(rhs))
	ty, ok := f.exLValType(lhs)
	if !ok {
		panic(unsupported("compound assignment to untyped lvalue", n))
	}
	if exIsStructTy(ty, f.sp) || ty.Size > 8 {
		panic(unsupported("compound assignment on aggregate", n))
	}
	a := f.exPinAddr(f.exNormAddr(f.addrOf(lhs)))
	a.Size = ty.Size
	cur := f.load(a, ty)
	res := f.exArith(baseop, cur, r, n)
	cv := f.spStmtMaybeConvert(res, ty)
	return f.exAssignStore(a, cv, ty, n)
}

// exAssignStore converts, stores, and returns the stored value (reloaded for
// scalars so `a = b = v` chains work; KMem for aggregates).
func (f *funcState) exAssignStore(a Addr, v SVal, ty CType, n *ASTNode) SVal {
	_ = n
	cv := f.spStmtMaybeConvert(v, ty)
	dst := a
	f.spStmtStore(dst, cv, ty)
	if exIsStructTy(ty, f.sp) || ty.Size > 8 {
		dst.Size = ty.Size
		return SVal{Kind: KMem, Type: ty, Mem: dst}
	}
	dst.Size = ty.Size
	return f.load(dst, ty)
}

// ---------------------------------------------------------------------------
// Casts
// ---------------------------------------------------------------------------

func (f *funcState) exCast(n *ASTNode) SVal {
	if len(n.Children) < 1 {
		panic(unsupported("cast without operand", n))
	}
	to, ok := ctypeOf(n.DataType, f.sp)
	if !ok {
		panic(unsupported("cast to "+n.DataType, n))
	}
	v := f.genRVal(n.Children[0])
	if to.IsVoid {
		return SVal{Kind: KVoid}
	}
	return f.exConv(v, to)
}

// ---------------------------------------------------------------------------
// Ternary, comma
// ---------------------------------------------------------------------------

func (f *funcState) exTernary(n *ASTNode) SVal {
	if len(n.Children) != 3 {
		panic(unsupported("malformed ternary", n))
	}
	c := f.genRVal(n.Children[0])
	t1, ok1 := f.exRValType(n.Children[1])
	t2, ok2 := f.exRValType(n.Children[2])
	if (ok1 && t1.IsVoid) || (ok2 && t2.IsVoid) {
		// `c ? f() : g()` with void arms: evaluate for effect only.
		lv := f.localLabel("ex_tv_e")
		le := f.localLabel("ex_tv_n")
		f.branchOn(c, false, lv)
		f.exSpill(f.genRVal(n.Children[1]))
		f.encJmp(le)
		f.defineLabel(lv)
		f.exSpill(f.genRVal(n.Children[2]))
		f.defineLabel(le)
		return SVal{Kind: KVoid}
	}
	ct := exCommonOf(t1, t2, ok1, ok2)
	sz := ct.Size
	if sz < 8 {
		sz = 8
	}
	slot := f.tempSlot(sz)
	lElse := f.localLabel("ex_te")
	lEnd := f.localLabel("ex_tn")
	f.branchOn(c, false, lElse)
	v1 := f.spStmtMaybeConvert(f.genRVal(n.Children[1]), ct)
	f.spStmtStore(slot, v1, ct)
	f.encJmp(lEnd)
	f.defineLabel(lElse)
	v2 := f.spStmtMaybeConvert(f.genRVal(n.Children[2]), ct)
	f.spStmtStore(slot, v2, ct)
	f.defineLabel(lEnd)
	slot.Size = ct.Size
	if exIsStructTy(ct, f.sp) || ct.Size > 8 {
		slot.Size = ct.Size
		return SVal{Kind: KMem, Type: ct, Mem: slot}
	}
	return f.load(slot, ct)
}

// exCommonOf picks the ternary result type: float > double handling,
// pointer dominance (with null-constant tolerance), else usual conversions.
func exCommonOf(t1, t2 CType, ok1, ok2 bool) CType {
	if !ok1 && !ok2 {
		return exIntTy
	}
	if !ok1 {
		return t2
	}
	if !ok2 {
		return t1
	}
	if t1.IsFloat || t2.IsFloat {
		if (t1.IsFloat && t1.Size == 8) || (t2.IsFloat && t2.Size == 8) {
			return exDoubleTy
		}
		return exFloatTy
	}
	if t1.IsPtr || t2.IsPtr {
		if t1.IsPtr {
			return t1
		}
		return t2
	}
	return exArithType("+", t1, t2)
}

func (f *funcState) exComma(n *ASTNode) SVal {
	var last SVal = SVal{Kind: KVoid}
	for _, ch := range n.Children {
		last = f.exSpill(f.genRVal(ch))
	}
	return last
}

// ---------------------------------------------------------------------------
// Sizeof / offsetof (compile-time constants; operands never evaluated)
// ---------------------------------------------------------------------------

func (f *funcState) exSizeof(n *ASTNode) SVal {
	if len(n.Children) < 1 {
		panic(unsupported("sizeof without operand", n))
	}
	return SVal{Kind: KConst, Type: exULongTy, Imm: itoaSp(f.exSizeofStatic(n.Children[0]))}
}

func (f *funcState) exSizeofStatic(sub *ASTNode) int {
	if sub.Type == NodeIdentifier {
		name := sub.VarName
		if name == "" {
			name = sub.Name
		}
		if d, ok := memLookupArray(f, name); ok {
			total := d.elem.Size
			for _, dim := range d.dims {
				total *= dim
			}
			if total > 0 {
				return total
			}
		}
		// File-scope arrays (MEM's TU table; never in the per-function
		// note table): total = elem size * dims.
		if g, ok := f.sp.globals[name]; ok && g != nil && len(g.Dims) > 0 {
			if ety, ok := ctypeOf(g.Type, f.sp); ok && ety.Size > 0 {
				total := ety.Size
				for _, dim := range g.Dims {
					total *= dim
				}
				if total > 0 {
					return total
				}
			}
		}
		if lv, ok := f.lookup(name); ok {
			// Unnoted arrays fall through to the element size (STMT's
			// MemNoteArray hook fixes this; also see exIsArrayName).
			if sz := lv.Type.Size; sz > 0 {
				return sz
			}
		}
		if _, ok := f.sp.enums[name]; ok {
			return 4
		}
		if _, ok := f.sp.funcSigs[name]; ok {
			return 8 // function designator decays to pointer
		}
	}
	if sub.Type == NodeMemberAccess && len(sub.Children) > 0 {
		ty, ok := f.exMemberType(sub)
		if ok {
			// Array members decay in most contexts, but sizeof sees the
			// whole array: recover the total from the struct definition.
			if ty.IsPtr {
				if total := f.exMemberArrayTotal(sub); total > 0 {
					return total
				}
			}
			if ty.Size > 0 {
				return ty.Size
			}
		}
	}
	if sub.Type == NodeArrayAccess && len(sub.Children) >= 1 {
		// Partial indexing of a known multi-dim array: size is the
		// remaining dimensions' product times the element size.
		if dims := f.exArrayDims(sub); dims != nil {
			if elem, ok := f.exArrayElem(sub); ok && elem.Size > 0 {
				total := elem.Size
				for i := f.exArrayDepth(sub); i < len(dims); i++ {
					total *= dims[i]
				}
				if total > 0 {
					return total
				}
			}
		}
		if t, ok := f.exLValType(sub); ok && t.Size > 0 && !strings.Contains(t.Name, "[") {
			return t.Size
		}
	}
	if sub.Type == NodeUnaryOp && sub.Operator == "*" && len(sub.Children) > 0 {
		if t, ok := f.exRValType(sub.Children[0]); ok && t.IsPtr {
			if et, ok2 := ctypeOf(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t.Name), "*")), f.sp); ok2 && et.Size > 0 {
				return et.Size
			}
		}
	}
	if t, ok := f.exRValType(sub); ok && t.Size > 0 {
		return t.Size
	}
	if sub.DataType != "" {
		if ty, ok := ctypeOf(sub.DataType, f.sp); ok && ty.Size > 0 {
			return ty.Size
		}
	}
	return 4
}

// exMemberArrayTotal recovers a member array's total byte size.
func (f *funcState) exMemberArrayTotal(n *ASTNode) int {
	base := n.Children[0]
	var spelling string
	if n.IsPointer {
		t, ok := f.exRValType(base)
		if !ok || !t.IsPtr {
			return 0
		}
		spelling = strings.TrimSpace(t.Name)
		for len(spelling) > 0 && spelling[len(spelling)-1] == '*' {
			spelling = strings.TrimSpace(spelling[:len(spelling)-1])
		}
	} else {
		t, ok := f.exLValType(base)
		if !ok {
			t2, ok2 := f.exRValType(base)
			if !ok2 {
				return 0
			}
			t = t2
		}
		spelling = t.Name
	}
	_, size, _, _, ok := exStructMember(f.sp, spelling, n.MemberName)
	if !ok {
		return 0
	}
	return size
}

func (f *funcState) exOffsetof(n *ASTNode) SVal {
	structTy := n.DataType
	total := 0
	for _, field := range strings.Split(n.Value, ".") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		// Array subscripts on the path (m[3]) are compile-time constants;
		// the parser only emits dotted names, so a bracket here is fatal.
		if strings.Contains(field, "[") {
			panic(unsupported("offsetof with index", n))
		}
		off, _, _, _, ok := exStructMember(f.sp, structTy, field)
		if !ok {
			panic(unsupported("offsetof unknown member "+field, n))
		}
		total += off
		// Descend for nested paths.
		_, _, mty, _, _ := exStructMember(f.sp, structTy, field)
		structTy = mty.Name
	}
	return SVal{Kind: KConst, Type: exULongTy, Imm: itoaSp(total)}
}

// ---------------------------------------------------------------------------
// Calls
// ---------------------------------------------------------------------------

// exFnPtrRet extracts the return type from a function-pointer spelling
// like "Texture2D (*)(void*, char*)" or "int(*)(void)". Returns ok=false
// for unparseable spellings (callers fall back to long).
func exFnPtrRet(sp *SPass, spelling string) (CType, bool) {
	t := strings.TrimSpace(spelling)
	// Strip calling-convention/attribute noise is unnecessary here: our
	// parser never produces it. Find the outermost (*) marker.
	idx := strings.Index(t, "(*)")
	if idx < 0 {
		// Maybe "R(*)(...)" without space, or nested: try "(*" scan.
		idx = strings.Index(t, "(*")
		if idx < 0 {
			return CType{}, false
		}
	}
	ret := strings.TrimSpace(t[:idx])
	if ret == "" {
		return CType{}, false
	}
	return ctypeOf(ret, sp)
}

// exCallRetType resolves a call's return type without emitting code.
func (f *funcState) exCallRetType(n *ASTNode) (CType, bool) {
	if n.Target == nil && n.Name != "" {
		if sig, ok := f.sp.funcSigs[n.Name]; ok && sig != nil && sig.ReturnType != "" {
			if ty, ok := ctypeOf(sig.ReturnType, f.sp); ok {
				return ty, true
			}
		}
		if libcDoubleRet[n.Name] {
			return exDoubleTy, true
		}
		if libcFloatFuncs[n.Name] {
			return exFloatTy, true
		}
		return exLongTy, true
	}
	// Indirect call: resolve the pointed-to signature. The parser encodes
	// a function-pointer type `R (*)(...)` as the spelling `R*` (verified:
	// `((Texture2D(*)(void*,char*))fn)(...)` carries DataType
	// "struct Texture*"), so stripping one pointer level yields the
	// return type. A genuine struct pointer is uncallable in valid C, so
	// this reading is sound.
	if n.Target != nil {
		if sp, ok := exIndirectTargetSpelling(f, n.Target); ok {
			if ty, ok := exIndirectRetType(f.sp, sp); ok {
				return ty, true
			}
		}
		if n.Target.Type == NodeCast && n.Target.DataType != "" {
			if ty, ok := exFnPtrRet(f.sp, n.Target.DataType); ok {
				return ty, true
			}
		}
	}
	return exLongTy, true
}

// exIndirectTargetSpelling recovers the pointed-to type spelling of a
// call target: cast DataTypes, declared identifier types, or member types.
func exIndirectTargetSpelling(f *funcState, t *ASTNode) (string, bool) {
	switch t.Type {
	case NodeCast:
		if t.DataType != "" {
			return t.DataType, true
		}
	case NodeIdentifier:
		name := t.VarName
		if name == "" {
			name = t.Name
		}
		if lv, ok := f.lookup(name); ok {
			return lv.Type.Name, true
		}
		if _, gty, ok := f.sp.globalAddr(name); ok {
			return gty.Name, true
		}
		if sig, ok := f.sp.funcSigs[name]; ok && sig != nil {
			return sig.ReturnType, true
		}
	case NodeMemberAccess:
		if t.DataType != "" {
			return t.DataType, true
		}
	}
	return "", false
}

// exIndirectRetType maps a pointed-to spelling to the return type.
func exIndirectRetType(sp *SPass, spelling string) (CType, bool) {
	t := strings.TrimSpace(spelling)
	if t == "" {
		return CType{}, false
	}
	if strings.HasSuffix(t, "*") {
		if ty, ok := ctypeOf(strings.TrimSpace(strings.TrimSuffix(t, "*")), sp); ok {
			return ty, true
		}
	}
	return exFnPtrRet(sp, t)
}

func exNormParams(sig *FunctionSignature) (fixed []string, ellipsis bool) {
	if sig == nil {
		return nil, false
	}
	for _, p := range sig.ParamTypes {
		if p == "..." {
			ellipsis = true
			continue
		}
		fixed = append(fixed, p)
	}
	if len(fixed) == 1 && strings.TrimSpace(fixed[0]) == "void" {
		fixed = nil
	}
	return fixed, ellipsis
}

func (f *funcState) exCall(n *ASTNode) SVal {
	// Indirect through an explicit target expression.
	if n.Target != nil {
		return f.exCallIndirect(n.Target, n.Children, n)
	}
	name := n.Name
	if name == "" && n.Target == nil {
		panic(unsupported("nameless call", n))
	}
	// `fp()` where fp is a variable: indirect through the variable.
	if _, known := f.sp.funcSigs[name]; !known && name != "" {
		if _, ok := f.lookup(name); ok {
			return f.exCallIndirect(&ASTNode{Type: NodeIdentifier, VarName: name}, n.Children, n)
		}
	}
	var retTy CType
	var fixed []string
	var ellipsis bool
	sigKnown := false
	if sig, ok := f.sp.funcSigs[name]; ok && sig != nil {
		sigKnown = true
		if sig.ReturnType == "" {
			retTy = exLongTy
		} else if ty, ok := ctypeOf(sig.ReturnType, f.sp); ok {
			retTy = ty
		} else {
			retTy = exLongTy
		}
		fixed, ellipsis = exNormParams(sig)
	} else {
		// Unknown extern (libc with skipped headers): varargs-safe default.
		// Float math tables mirror the default backend exactly.
		switch {
		case libcDoubleRet[name]:
			retTy = exDoubleTy
		case libcFloatFuncs[name]:
			retTy = exFloatTy
		default:
			retTy = exLongTy
		}
	}
	isVarArg := !sigKnown || ellipsis || len(n.Children) > len(fixed)
	args := f.exCallArgs(n.Children, name, fixed, sigKnown, false)
	nFloat := 0
	for _, a := range args {
		if a.Type.IsFloat {
			if nFloat < 8 {
				nFloat++
			}
		}
	}
	target := SVal{Kind: KConst, Type: exPtrTy, Imm: name}
	var sretBuf *Addr
	if sigKnown && isLargeStructSp(retTy) {
		// Large struct return: allocate the caller's buffer; emitCall
		// passes its address in RDI itself (phase 0), so the buffer is
		// only referenced here for collecting the result value.
		slot := f.tempSlot(retTy.Size)
		slot.Size = retTy.Size
		sretBuf = &slot
	}
	ret := f.emitCall(target, isVarArg, nFloat, args, retTy, sretBuf)
	return f.exCollectCall(ret, retTy, n, sigKnown)
}

// exCallIndirect evaluates args, then the target (late, so its register is
// live for exactly the call sequence), then emits. The target travels
// spilled; emitCall loads it into fixed R13 for the call itself.
func (f *funcState) exCallIndirect(target *ASTNode, argNodes []*ASTNode, n *ASTNode) SVal {
	retTy := exLongTy
	if ty, ok := f.exCallRetType(n); ok {
		retTy = ty
	}
	args := f.exCallArgs(argNodes, "", nil, false, true)
	var sret *Addr
	if isLargeStructSp(retTy) {
		slot := f.tempSlot(retTy.Size)
		sret = &slot
	}
	nFloat := 0
	for _, a := range args {
		if a.Type.IsFloat && nFloat < 8 {
			nFloat++
		}
	}
	tv := f.exSpill(f.genRVal(target))
	return f.emitCall(tv, true, nFloat, args, retTy, sret)
}

// exCallArgs evaluates arguments left-to-right with conversions and the
// spill discipline: every KReg/KXMM arg is stored to a temp slot the moment
// it is produced, so later arg evaluation (and emitCall's own scratch use)
// can never clobber it. Structs are chunked into 8-byte integer pieces.
// exIsInternalCall reports whether a call uses the sea internal ABI
// (TU-defined target, or indirect through a function pointer — those
// target sea-compiled trampolines). External (libc etc.) calls use SysV.
// Mirrors the default backend's isExtern rule inverted.
func (f *funcState) exIsInternalCall(name string, indirect bool) bool {
	if indirect {
		return true
	}
	if name == "" {
		return false
	}
	return f.sp.definedFuncs[name]
}

func (f *funcState) exCallArgs(argNodes []*ASTNode, name string, fixed []string, sigKnown bool, indirect bool) []SVal {
	var args []SVal
	for i, an := range argNodes {
		v := f.genRVal(an)
		if sigKnown && i < len(fixed) {
			if pt, ok := ctypeOf(fixed[i], f.sp); ok {
				// Bare-array decay in pointer context (see exDecayIdent).
				// A bare non-pointer identifier where a pointer is
				// expected can only be an array in valid C.
				if pt.IsPtr && an.Type == NodeIdentifier && !v.Type.IsPtr {
					v = f.exDecayIdent(an)
				} else {
					v = f.spStmtMaybeConvert(v, pt)
				}
			}
		} else {
			// Varargs/unknown: float promotes to double (varargs-safe),
			// except libc float-only math which takes float bits.
			if v.Type.IsFloat && !(libcFloatFuncs[name] && v.Type.Size == 4) {
				v = f.spStmtMaybeConvert(v, exDoubleTy)
			} else if !v.Type.IsPtr && !v.Type.IsFloat && !v.Type.IsVoid && v.Type.Size < 4 && v.Kind != KVoid {
				v = f.spStmtMaybeConvert(v, exIntTy)
			}
		}
		if exIsStructVal(f.sp, v) {
			// Large structs (>16B) on internal calls travel by pointer
			// (sea ABI, mirroring the default backend); externals go
			// by value (chunked; full SysV stack copy is a noted gap).
			if v.Type.Size > 16 && f.exIsInternalCall(name, indirect) {
				args = append(args, f.exStructAddr(v))
				continue
			}
			args = append(args, f.exStructChunks(v)...)
			continue
		}
		args = append(args, f.exSpill(v))
	}
	return args
}

// exStructAddr materializes the address of a KMem struct value for
// by-pointer passing (sea internal ABI for >16-byte structs on internal
// calls). The address is spilled to a temp slot immediately (KMem, not
// KReg): a bare register would be clobbered by subsequent argument
// evaluation before emitCall's phase-1 spill runs (bell cam3_apply bug).
func (f *funcState) exStructAddr(v SVal) SVal {
	a := v.Mem
	a.Size = 8
	r := f.scratchReg()
	f.encLeaRegMem(r, a)
	return f.exSpill(SVal{Kind: KReg, Type: CType{Name: "void*", Size: 8, IsPtr: true}, Reg: r})
}

// exStructChunks splits a KMem struct value into 8-byte integer chunks for
// SysV-style by-value passing (int regs then stack, matching the frozen
// callee prologue for <=8-byte params; best effort beyond that).
func (f *funcState) exStructChunks(v SVal) []SVal {
	if v.Kind != KMem {
		return []SVal{f.exSpill(v)}
	}
	size := v.Type.Size
	if size <= 0 {
		panic(unsupported("zero-size struct argument", nil))
	}
	base := v.Mem
	if base.Sym != "" {
		// Encoder drops Disp for Sym bases: materialize per chunk below.
	}
	var out []SVal
	off := 0
	// SysV routing needs float chunks typed as float (else they ride int
	// regs); the bits are identical, only the tag changes. Classes mirror
	// the callee prologue's homeStructParam exactly.
	classes := spChunkClasses(f.sp, v.Type.Name)
	for off < size {
		chunk := 8
		for chunk > 1 && size-off < chunk {
			chunk /= 2
		}
		var a Addr
		if base.Sym != "" {
			rb := f.scratchReg()
			f.encLeaRegMem(rb, Addr{Sym: base.Sym, RegBase: -1, Index: -1, Scale: 1, Size: 8})
			a = Addr{RegBase: rb, Disp: base.Disp + off, Index: -1, Scale: 1, Size: chunk}
		} else {
			a = base
			a.Disp += off
			a.Size = chunk
		}
		r := f.load(a, spStmtChunkTy(chunk))
		sv := f.exSpill(r)
		if classes != nil {
			if ri := off / 8; ri < len(classes) && classes[ri] == "sse" {
				if chunk == 8 {
					sv.Type = exDoubleTy
				} else if chunk == 4 {
					sv.Type = exFloatTy
				}
			}
		}
		out = append(out, sv)
		off += chunk
	}
	return out
}

// exCollectCall canonicalizes call results: small structs to KMem slots,
// 9-16-byte TU structs to rax+rdx KMem slots, narrow ints in RAX.
func (f *funcState) exCollectCall(ret SVal, retTy CType, n *ASTNode, sigKnown bool) SVal {
	_ = n
	if retTy.IsVoid {
		return SVal{Kind: KVoid}
	}
	if isLargeStructSp(retTy) {
		// Value already in the sret buffer (KMem from emitCall).
		return ret
	}
	if sigKnown && exIsStructTy(retTy, f.sp) {
		if retTy.Size > 8 {
			// 9-16 bytes: high half still in RDX (callee put rax+rdx).
			slot := f.tempSlot(16)
			slot.Size = 8
			loTy := CType{Name: "long", Size: 8, Signed: true}
			f.store(slot, SVal{Kind: KReg, Type: loTy, Reg: REG_RAX})
			hi := slot
			hi.Disp += 8
			f.store(hi, SVal{Kind: KReg, Type: loTy, Reg: REG_RDX})
			slot.Size = 16
			return SVal{Kind: KMem, Type: retTy, Mem: slot}
		}
		slot := f.tempSlot(8)
		slot.Size = 8
		f.store(slot, ret)
		slot.Size = retTy.Size
		return SVal{Kind: KMem, Type: retTy, Mem: slot}
	}
	if ret.Kind == KReg && !retTy.IsFloat && retTy.Size < 8 {
		// Callees (libc especially) leave high garbage in RAX for narrow
		// returns; re-establish the invariant before anyone compares.
		f.exNarrow(ret.Reg, retTy.Signed)
	}
	return ret
}

// exIsStructVal reports whether an evaluated value is a struct/union value
// needing chunked (by-value) argument passing.
func exIsStructVal(sp *SPass, v SVal) bool {
	if v.Kind != KMem {
		return false
	}
	return exIsStructTy(v.Type, sp)
}

// ---------------------------------------------------------------------------
// Compound literals, statement expressions
// ---------------------------------------------------------------------------

func (f *funcState) exCompound(n *ASTNode) SVal {
	// Array compound literal `(int[]){...}`: the initializer defines the
	// size; element spelling is the bracket-free prefix.
	if strings.Contains(n.DataType, "[") {
		elemSpelling := n.DataType
		if idx := strings.Index(elemSpelling, "["); idx >= 0 {
			elemSpelling = strings.TrimSpace(elemSpelling[:idx])
		}
		ety, ok := ctypeOf(elemSpelling, f.sp)
		if !ok || ety.Size <= 0 {
			panic(unsupported("array compound element "+elemSpelling, n))
		}
		size := len(n.Children) * ety.Size
		if size <= 0 {
			size = ety.Size
		}
		slot := f.tempSlot(size)
		slot.Size = size
		f.genCompoundInit(n, slot)
		return SVal{Kind: KMem, Type: CType{Name: n.DataType, Size: size}, Mem: slot}
	}
	ty, ok := ctypeOf(n.DataType, f.sp)
	if !ok {
		panic(unsupported("compound literal of "+n.DataType, n))
	}
	size := ty.Size
	if size <= 0 {
		size = 8
	}
	slot := f.tempSlot(size)
	slot.Size = size
	// Filling is MEM-owned (zeroing, designated/positional, nested).
	f.genCompoundInit(n, slot)
	slot.Size = size
	return SVal{Kind: KMem, Type: ty, Mem: slot}
}

// exBlock evaluates a statement expression `({ ...; expr; })`: all children
// but the last run as statements; the trailing ExprStmt yields the value.
// Declared names are scoped: the locals map is restored afterwards.
func (f *funcState) exBlock(n *ASTNode) SVal {
	if len(n.Children) == 0 {
		return SVal{Kind: KVoid}
	}
	saved := make(map[string]*LocalVar, len(f.locals))
	for k, v := range f.locals {
		saved[k] = v
	}
	// Array dims table (MEM-owned) is function-scoped, not block-scoped;
	// block-local arrays leak names there, matching STMT's shadowing
	// limitation. Documented in the report.
	defer func() {
		f.locals = saved
	}()
	for _, st := range n.Children[:len(n.Children)-1] {
		if err := f.genStmt(st); err != nil {
			panic(exStmtErr(err, st))
		}
	}
	last := n.Children[len(n.Children)-1]
	if last.Type == NodeExprStmt && len(last.Children) > 0 {
		return f.genRVal(last.Children[0])
	}
	if err := f.genStmt(last); err != nil {
		panic(exStmtErr(err, last))
	}
	return SVal{Kind: KVoid}
}

// exStmtErr converts a genStmt error to a trappable spassError. genStmt
// returns *spassError values; anything else is wrapped as unsupported.
func exStmtErr(err error, n *ASTNode) *spassError {
	if se, ok := err.(*spassError); ok {
		return se
	}
	return unsupported(err.Error(), n)
}

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

func itoaSp(v int) string {
	if v == 0 {
		return "0"
	}
	neg := false
	if v < 0 {
		neg = true
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func isPow2Sp(v int) bool { return v > 0 && v&(v-1) == 0 }

func log2Sp(v int) int {
	r := 0
	for v > 1 {
		v >>= 1
		r++
	}
	return r
}
