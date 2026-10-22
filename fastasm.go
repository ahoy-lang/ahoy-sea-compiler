// fastasm: in-process AT&T-syntax x86-64 assembler.
//
// AssembleObject parses the assembly text emitted by the compiler and
// produces an *ObjectFile (machine code + symbols + relocations) without
// shelling out to GNU as. elf_object.go writes the result as an ELF64
// relocatable object.
//
// Encoding choices mirror GNU as so that .text bytes match `as` output:
//   - ALU $imm with %rax and a 32-bit immediate uses the accumulator
//     short form (05/2D/3D/25/0D/35/A9); otherwise 83 for int8, 81 for int32.
//   - movq $imm,%reg uses C7 when the value fits int32 else B8 (movabs).
//   - jmp/jcc use the short form when the displacement fits int8.
//   - RIP-relative references to named symbols are emitted with a zero
//     displacement plus a relocation (PC32 against the symbol for globals,
//     undefined and COMMON symbols; PC32 against the section for static
//     locals), exactly like GNU as.
//   - call to a static (non-global) function is resolved locally; call to
//     a global/undefined/COMMON symbol emits R_X86_64_PLT32.
//   - Numeric literals follow GAS rules: 0x hex, 0b binary, leading-0
//     octal, otherwise decimal.
package main

import (
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Required API
// ---------------------------------------------------------------------------

// ObjSymbol describes one symbol in the object file.
type ObjSymbol struct {
	Name    string
	Section string // ".text", ".rodata", ".data", ".bss"
	Offset  uint64
	Global  bool
	Local   bool
	Size    uint64
}

// ObjReloc describes one relocation entry.
type ObjReloc struct {
	Section string // section the relocation applies to
	Offset  uint64 // offset within that section
	Symbol  string // symbol name (or section name for section-relative)
	Type    RelocationType
	Addend  int64
}

// ObjectFile is the result of assembling: raw section bytes plus symbol
// and relocation tables.
type ObjectFile struct {
	Text    []byte
	Rodata  []byte
	Data    []byte
	BSS     uint64
	Symbols []ObjSymbol
	Relocs  []ObjReloc
}

// AssembleObject parses AT&T x86-64 assembly text and produces an ObjectFile.
func AssembleObject(asmText string) (*ObjectFile, error) {
	st := newFaState()
	if err := st.parse(asmText); err != nil {
		return nil, err
	}
	if err := st.layout(); err != nil {
		return nil, err
	}
	return st.finish()
}

// ---------------------------------------------------------------------------
// COMMON-symbol side table.
//
// COMMON symbols carry an alignment that has no field in ObjSymbol, so
// AssembleObject records it here for WriteELF (see elf_object.go). The
// public API types are untouched.
//
// A COMMON symbol appears in ObjectFile.Symbols with Section "" and its
// size in Size; faCommonTable holds the alignment and marks it COMMON.
// ---------------------------------------------------------------------------

type faCommonInfo struct {
	size  uint64
	align uint64
}

var faCommonTable = map[*ObjectFile]map[string]faCommonInfo{}

// ---------------------------------------------------------------------------
// Registers
// ---------------------------------------------------------------------------

type faRegInfo struct {
	code    int
	size    int // 8, 4, 2, 1
	needRex bool
	high8   bool
}

var faRegs = map[string]faRegInfo{
	"rax": {0, 8, false, false}, "eax": {0, 4, false, false}, "ax": {0, 2, false, false}, "al": {0, 1, false, false},
	"rcx": {1, 8, false, false}, "ecx": {1, 4, false, false}, "cx": {1, 2, false, false}, "cl": {1, 1, false, false},
	"rdx": {2, 8, false, false}, "edx": {2, 4, false, false}, "dx": {2, 2, false, false}, "dl": {2, 1, false, false},
	"rbx": {3, 8, false, false}, "ebx": {3, 4, false, false}, "bx": {3, 2, false, false}, "bl": {3, 1, false, false},
	"rsp": {4, 8, false, false}, "esp": {4, 4, false, false}, "sp": {4, 2, false, false}, "spl": {4, 1, true, false},
	"rbp": {5, 8, false, false}, "ebp": {5, 4, false, false}, "bp": {5, 2, false, false}, "bpl": {5, 1, true, false},
	"rsi": {6, 8, false, false}, "esi": {6, 4, false, false}, "si": {6, 2, false, false}, "sil": {6, 1, true, false},
	"rdi": {7, 8, false, false}, "edi": {7, 4, false, false}, "di": {7, 2, false, false}, "dil": {7, 1, true, false},
	"r8": {8, 8, false, false}, "r8d": {8, 4, false, false}, "r8w": {8, 2, false, false}, "r8b": {8, 1, false, false},
	"r9": {9, 8, false, false}, "r9d": {9, 4, false, false}, "r9w": {9, 2, false, false}, "r9b": {9, 1, false, false},
	"r10": {10, 8, false, false}, "r10d": {10, 4, false, false}, "r10w": {10, 2, false, false}, "r10b": {10, 1, false, false},
	"r11": {11, 8, false, false}, "r11d": {11, 4, false, false}, "r11w": {11, 2, false, false}, "r11b": {11, 1, false, false},
	"r12": {12, 8, false, false}, "r12d": {12, 4, false, false}, "r12w": {12, 2, false, false}, "r12b": {12, 1, false, false},
	"r13": {13, 8, false, false}, "r13d": {13, 4, false, false}, "r13w": {13, 2, false, false}, "r13b": {13, 1, false, false},
	"r14": {14, 8, false, false}, "r14d": {14, 4, false, false}, "r14w": {14, 2, false, false}, "r14b": {14, 1, false, false},
	"r15": {15, 8, false, false}, "r15d": {15, 4, false, false}, "r15w": {15, 2, false, false}, "r15b": {15, 1, false, false},
	"ah": {4, 1, false, true}, "ch": {5, 1, false, true}, "dh": {6, 1, false, true}, "bh": {7, 1, false, true},
}

func faParseReg(s string) (faRegInfo, bool) {
	t := s
	if strings.HasPrefix(t, "%") {
		t = t[1:]
	}
	t = faLower(t)
	r, ok := faRegs[t]
	return r, ok
}

func faParseXMM(s string) (int, bool) {
	t := s
	if strings.HasPrefix(t, "%") {
		t = t[1:]
	}
	t = faLower(t)
	if !strings.HasPrefix(t, "xmm") {
		return 0, false
	}
	n, err := strconv.Atoi(t[3:])
	if err != nil || n < 0 || n > 15 {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------
// GAS-flavored integer parsing: 0x hex, 0b binary, leading-0 octal,
// otherwise decimal, with optional sign.
// ---------------------------------------------------------------------------

func faParseInt(s string) (uint64, error) {
	t := strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(t, "-") {
		neg = true
		t = t[1:]
	} else if strings.HasPrefix(t, "+") {
		t = t[1:]
	}
	var v uint64
	var err error
	switch {
	case strings.HasPrefix(t, "0x") || strings.HasPrefix(t, "0X"):
		v, err = strconv.ParseUint(t[2:], 16, 64)
	case strings.HasPrefix(t, "0b") || strings.HasPrefix(t, "0B"):
		v, err = strconv.ParseUint(t[2:], 2, 64)
	case len(t) > 1 && t[0] == '0' && t[1] >= '0' && t[1] <= '9':
		for i := 1; i < len(t); i++ {
			if t[i] < '0' || t[i] > '7' {
				return 0, fmt.Errorf("invalid octal number: %s", s)
			}
		}
		v, err = strconv.ParseUint(t[1:], 8, 64)
	default:
		v, err = strconv.ParseUint(t, 10, 64)
	}
	if err != nil {
		return 0, fmt.Errorf("invalid number %q: %v", s, err)
	}
	if neg {
		v = uint64(-int64(v))
	}
	return v, nil
}

func faFitsInt8(v uint64) bool {
	sv := int64(v)
	return sv >= -128 && sv <= 127
}

func faFitsInt32(v uint64) bool {
	sv := int64(v)
	return sv >= -2147483648 && sv <= 2147483647
}

// ---------------------------------------------------------------------------
// Memory operands
// ---------------------------------------------------------------------------

type faAddr struct {
	isReg  bool
	reg    int // full code when isReg
	regSz  int
	base   int // -1 if none
	index  int // -1 if none
	scale  int
	disp   int64
	rip    bool
	ripSym string
	ripAdd int64
}

func faParseAddr(s string) (faAddr, error) {
	t := strings.TrimSpace(s)
	if r, ok := faParseReg(t); ok {
		return faAddr{isReg: true, reg: r.code, regSz: r.size}, nil
	}
	if _, ok := faParseXMM(t); ok {
		return faAddr{}, fmt.Errorf("xmm register cannot be a memory address: %s", s)
	}
	if i := strings.Index(t, "(%rip)"); i >= 0 {
		symPart := strings.TrimSpace(t[:i])
		rest := strings.TrimSpace(t[i+len("(%rip)"):])
		if rest != "" {
			return faAddr{}, fmt.Errorf("trailing text after (%%rip): %s", s)
		}
		sym, add, err := faSplitSymAdd(symPart)
		if err != nil {
			return faAddr{}, err
		}
		return faAddr{rip: true, ripSym: sym, ripAdd: add}, nil
	}
	pi := strings.IndexByte(t, '(')
	if pi < 0 || !strings.HasSuffix(t, ")") {
		return faAddr{}, fmt.Errorf("invalid address: %s", s)
	}
	dispStr := strings.TrimSpace(t[:pi])
	inner := t[pi+1 : len(t)-1]
	parts := faSplitTop(inner, ',')
	var a faAddr
	a.base = -1
	a.index = -1
	a.scale = 1
	if dispStr != "" {
		v, err := faParseInt(dispStr)
		if err != nil {
			return faAddr{}, fmt.Errorf("bad displacement in %s: %v", s, err)
		}
		a.disp = int64(v)
	}
	if len(parts) < 1 || len(parts) > 3 {
		return faAddr{}, fmt.Errorf("invalid address: %s", s)
	}
	base, ok := faParseReg(strings.TrimSpace(parts[0]))
	if !ok {
		return faAddr{}, fmt.Errorf("bad base register in %s", s)
	}
	a.base = base.code
	if len(parts) >= 2 {
		idx, ok := faParseReg(strings.TrimSpace(parts[1]))
		if !ok {
			return faAddr{}, fmt.Errorf("bad index register in %s", s)
		}
		a.index = idx.code
	}
	if len(parts) == 3 {
		sc, err := faParseInt(strings.TrimSpace(parts[2]))
		if err != nil || (sc != 1 && sc != 2 && sc != 4 && sc != 8) {
			return faAddr{}, fmt.Errorf("bad scale in %s", s)
		}
		a.scale = int(sc)
	}
	return a, nil
}

// faSplitSymAdd splits "sym", "sym+N", "sym-N".
func faSplitSymAdd(s string) (string, int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return "", 0, fmt.Errorf("empty symbol in RIP-relative address")
	}
	idx := strings.LastIndexAny(t, "+-")
	if idx <= 0 {
		return t, 0, nil
	}
	sym := strings.TrimSpace(t[:idx])
	add, err := faParseInt(strings.TrimSpace(t[idx:]))
	if err != nil {
		return "", 0, fmt.Errorf("bad addend in %q", s)
	}
	return sym, int64(add), nil
}

// faSplitTop splits on sep ignoring separators inside parentheses.
func faSplitTop(s string, sep byte) []string {
	return faSplitTopInto(s, sep, nil)
}

// faSplitTopInto splits s on sep at paren depth 0, writing parts into buf
// (reused to avoid per-line slice allocation on the hot path) and growing
// it only if more parts appear than fit.
func faSplitTopInto(s string, sep byte, buf []string) []string {
	parts := buf[:0]
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case sep:
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(s[start:]))
	return parts
}

// faSplitMn splits an instruction line into its mnemonic and the trimmed
// operand text with a single manual byte scan (strings.IndexAny + TrimSpace
// showed up in profiles at ~70ms per editor-TU assemble).
func faSplitMn(line string) (mn, rest string) {
	i := 0
	for i < len(line) && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	if i == len(line) {
		return line, ""
	}
	j := i + 1
	for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
		j++
	}
	k := len(line)
	for k > j && (line[k-1] == ' ' || line[k-1] == '\t') {
		k--
	}
	return line[:i], line[j:k]
}

// faMnOf extracts and lowercases the mnemonic of an instruction line.
func faMnOf(line string) string {
	mn := line
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		mn = line[:i]
	}
	return faLower(mn)
}

// faLower avoids a strings.ToLower copy per line: our emitter only emits
// lowercase mnemonics, so the input is returned as-is when it has no
// uppercase bytes.
func faLower(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			return strings.ToLower(s)
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// Assembler state
// ---------------------------------------------------------------------------

type faSym struct {
	name        string
	order       int
	defined     bool
	isCommon    bool
	commonSize  uint64
	commonAlign uint64
	section     string
	offset      uint64
	global      bool
	localMarked bool
	typ         string // "", "func", "object"
	size        uint64
	isTemp      bool // .L prefix: no symtab entry
}

type faSec uint8

const (
	faSecText faSec = iota
	faSecRodata
	faSecData
	faSecBss
)

func (s faSec) name() string {
	switch s {
	case faSecText:
		return ".text"
	case faSecRodata:
		return ".rodata"
	case faSecData:
		return ".data"
	case faSecBss:
		return ".bss"
	}
	return ".text"
}

func faSecOf(name string) faSec {
	switch name {
	case ".text":
		return faSecText
	case ".rodata":
		return faSecRodata
	case ".data":
		return faSecData
	case ".bss":
		return faSecBss
	}
	return faSecText
}

type faInstr struct {
	line     string
	target   string // jump target ("" for non-jumps; substring, no alloc)
	enc      []byte // layout-filled encoding for cacheable lines; nil otherwise
	lineno   int32
	cachedSz int32
	sec      faSec
	isJump   bool
	jcc      bool // true for conditional, false for jmp
	near     bool
	cacheable bool   // set at parse time via faCacheable
}

type faSizeMark struct {
	sym string
	sec string
	pos int // instruction index (for .text) or data offset
}

type faState struct {
	cur       string
	curID     faSec
	text      []faInstr
	rodata    []byte
	data      []byte
	bssSize   uint64
	syms      map[string]*faSym
	order     []string
	labels    map[string]int // .text label -> instr index
	sizeMarks []faSizeMark
	relocs    []ObjReloc
	textOffs  []uint64
	textSize  uint64 // total .text bytes (set by layout)
	// unknown, when non-nil, collects names of referenced-but-undefined
	// symbols instead of inserting placeholders into the shared table.
	// The parallel layout sizing pass sets it on worker states: workers
	// must not mutate shared maps. The sequential path leaves it nil and
	// inserts placeholders inline as before. A shared immutable dummy
	// stands in for the missing entry during sizing (sizes don't depend
	// on resolution).
	unknown   *[]string
}

func newFaState() *faState {
	return &faState{
		cur:    ".text",
		syms:   make(map[string]*faSym),
		labels: make(map[string]int),
	}
}

// faPlaceholder stands in for undefined symbols during parallel sizing
// (sizes never depend on resolution). It is never mutated.
var faPlaceholder = &faSym{}

// lookupSym resolves name for encoding: placeholders for missing entries
// go to the shared table sequentially, or to the worker's unknown list
// when set (parallel sizing must not mutate shared maps).
func (st *faState) lookupSym(name string) *faSym {
	if s, ok := st.syms[name]; ok {
		return s
	}
	if st.unknown != nil {
		*st.unknown = append(*st.unknown, name)
		return faPlaceholder
	}
	return st.sym(name)
}

// faCacheable reports whether an instruction line encodes identically at
// every position, so its bytes can be carried on the instruction.
// Excludes anything with a RIP-relative reference, calls, and jumps
// (target/reloc dependent).
func faCacheable(sec, mn, line string) bool {
	if sec != ".text" {
		return false
	}
	if mn == "call" || mn == "jmp" || faJccCode(mn) != -1 {
		return false
	}
	return !strings.Contains(line, "(%rip)")
}

func (st *faState) sym(name string) *faSym {
	s, ok := st.syms[name]
	if !ok {
		s = &faSym{name: name, order: len(st.order), isTemp: strings.HasPrefix(name, ".L")}
		st.syms[name] = s
		st.order = append(st.order, name)
	}
	return s
}

func (st *faState) curLen() uint64 {
	switch st.cur {
	case ".text":
		return 0 // assigned during layout
	case ".rodata":
		return uint64(len(st.rodata))
	case ".data":
		return uint64(len(st.data))
	case ".bss":
		return st.bssSize
	}
	return 0
}

func (st *faState) curBuf() *[]byte {
	switch st.cur {
	case ".rodata":
		return &st.rodata
	case ".data":
		return &st.data
	}
	return nil
}

func (st *faState) alignCur(n uint64, lineno int) error {
	if n == 0 {
		return fmt.Errorf("line %d: .align 0", lineno)
	}
	switch st.cur {
	case ".rodata", ".data":
		buf := st.curBuf()
		pad := (n - uint64(len(*buf))%n) % n
		*buf = append(*buf, make([]byte, pad)...)
	case ".bss":
		st.bssSize = (st.bssSize + n - 1) / n * n
	case ".text":
		// No .align occurs in .text input; record as pseudo-op.
		st.text = append(st.text, faInstr{line: ".align", lineno: int32(lineno), sec: faSecText, cachedSz: -int32(n)})
	default:
		return fmt.Errorf("line %d: .align in unknown section %s", lineno, st.cur)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

// faLineRec is one scanned source line: kind plus byte spans into the
// input (no allocations during the parallel scan; strings are cut in the
// sequential fold).
type faLineRec struct {
	kind     uint8 // 1=label, 2=directive, 3=instruction (0 unused/blank)
	start    int   // trimmed line start
	end      int   // trimmed line end
	mnEnd    int   // instruction only: mnemonic end (mn = text[start:mnEnd])
}

// faLineKind classifies one trimmed line span.
func faLineKind(text string, start, end int) (uint8, int) {
	if end-start <= 0 {
		return 0, start
	}
	if text[end-1] == ':' {
		return 1, start
	}
	if text[start] == '#' {
		return 0, start
	}
	if text[start] == '.' {
		return 2, start
	}
	// Instruction: mnemonic runs to the first blank.
	m := start
	for m < end && text[m] != ' ' && text[m] != '\t' {
		m++
	}
	return 3, m
}

// faTrimSpan trims trailing/leading blanks (space/tab/CR; the input never
// contains other TrimSpace whitespace inside a line).
func faTrimSpan(text string, a, b int) (int, int) {
	for a < b {
		c := text[a]
		if c != ' ' && c != '\t' && c != '\r' {
			break
		}
		a++
	}
	for b > a {
		c := text[b-1]
		if c != ' ' && c != '\t' && c != '\r' {
			break
		}
		b--
	}
	return a, b
}

// faScanLines splits the input into line records. Large inputs scan chunks
// on a worker pool (newline boundaries; lines are independent until the
// stateful fold). Small inputs scan inline with no goroutines.
func faScanLines(asmText string) (recs []faLineRec, nInstr int) {
	n := len(asmText)
	workers := 1
	if n >= 1<<20 && runtime.NumCPU() > 1 {
		workers = runtime.NumCPU()
	}
	// Chunk boundaries at newlines; a chunk owns lines starting in it.
	bounds := make([]int, workers+1)
	for w := 1; w < workers; w++ {
		p := w * n / workers
		for p < n && asmText[p] != '\n' {
			p++
		}
		if p < n {
			p++ // start after the newline
		}
		bounds[w] = p
	}
	bounds[workers] = n
	type faScanResult struct {
		recs   []faLineRec
		nInstr int
	}
	results := make([]faScanResult, workers)
	runChunk := func(w int) {
		a, b := bounds[w], bounds[w+1]
		// Lines per chunk unknown; cap ~1 record per 16 bytes (31MB
		// editor asm averages ~15 bytes/line; under-sizing regrows with
		// memmove copies that showed up in profiles).
		r := make([]faLineRec, 0, (b-a)/16+64)
		ni := 0
		pos := a
		for pos < b {
			e := pos
			for e < n && asmText[e] != '\n' {
				e++
			}
			s, f := faTrimSpan(asmText, pos, e)
			if f > s {
				kind, mnEnd := faLineKind(asmText, s, f)
				if kind != 0 {
					r = append(r, faLineRec{kind: kind, start: s, end: f, mnEnd: mnEnd})
					if kind == 3 {
						ni++
					}
				}
			}
			pos = e + 1
		}
		results[w] = faScanResult{recs: r, nInstr: ni}
	}
	if workers == 1 {
		runChunk(0)
	} else {
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				runChunk(w)
			}(w)
		}
		wg.Wait()
	}
	totalRecs, totalInstr := 0, 0
	for w := range results {
		totalRecs += len(results[w].recs)
		totalInstr += results[w].nInstr
	}
	recs = make([]faLineRec, 0, totalRecs)
	for w := range results {
		recs = append(recs, results[w].recs...)
	}
	return recs, totalInstr
}

func (st *faState) parse(asmText string) error {
	recs, nInstr := faScanLines(asmText)
	lineno := 0
	// Exact instruction presize from the scan (no regrowth copies).
	st.text = make([]faInstr, 0, nInstr)
	st.relocs = make([]ObjReloc, 0, len(asmText)/240)
	// lineno counts every source line including blanks: recover it from
	// record positions by counting newlines skipped between records.
	prevEnd := 0
	for _, rec := range recs {
		for i := prevEnd; i < rec.start; i++ {
			if asmText[i] == '\n' {
				lineno++
			}
		}
		lineno++
		prevEnd = rec.end
		line := asmText[rec.start:rec.end]
		if rec.kind == 1 {
			name := strings.TrimSpace(strings.TrimSuffix(line, ":"))
			if name == "" || strings.ContainsAny(name, " \t") {
				return fmt.Errorf("line %d: bad label %q", lineno, line)
			}
			s := st.sym(name)
			if s.defined {
				return fmt.Errorf("line %d: duplicate label %s", lineno, name)
			}
			s.defined = true
			if st.cur == ".text" {
				s.section = ".text"
				st.labels[name] = len(st.text)
			} else {
				s.section = st.cur
				s.offset = st.curLen()
			}
			continue
		}
		if rec.kind == 2 {
			if err := st.parseDirective(line, lineno); err != nil {
				return err
			}
			continue
		}
		// Instruction (kind 3): mnemonic span came from the scan; the
		// operand rest is trimmed here (same as faSplitMn).
		mn := asmText[rec.start:rec.mnEnd]
		rs, re := faTrimSpan(asmText, rec.mnEnd, rec.end)
		rest := asmText[rs:re]
		lower := faLower(mn)
		if lower == "jmp" || faJccCode(lower) != -1 {
			var opsBuf [2]string
			ops := faSplitTopInto(rest, ',', opsBuf[:0])
			if len(ops) != 1 {
				return fmt.Errorf("line %d: bad jump operands %q", lineno, line)
			}
			st.text = append(st.text, faInstr{line: line, lineno: int32(lineno), sec: st.curID,
				isJump: true, jcc: lower != "jmp", target: strings.TrimSpace(ops[0])})
			continue
		}
		st.text = append(st.text, faInstr{line: line, lineno: int32(lineno), sec: st.curID,
			cacheable: faCacheable(st.cur, lower, line)})
	}
	return nil
}

func (st *faState) parseDirective(line string, lineno int) error {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil
	}
	dir := fields[0]
	rest := strings.TrimSpace(line[len(dir):])
	switch dir {
	case ".section":
		switch rest {
		case ".rodata", ".text", ".data", ".bss":
			st.cur = rest
			st.curID = faSecOf(rest)
		default:
			return fmt.Errorf("line %d: unsupported section %q", lineno, rest)
		}
	case ".text", ".data", ".bss":
		st.cur = dir
		st.curID = faSecOf(dir)
	case ".globl":
		name := strings.Fields(rest)
		if len(name) != 1 {
			return fmt.Errorf("line %d: bad .globl %q", lineno, line)
		}
		st.sym(name[0]).global = true
	case ".local":
		name := strings.Fields(rest)
		if len(name) != 1 {
			return fmt.Errorf("line %d: bad .local %q", lineno, line)
		}
		s := st.sym(name[0])
		s.global = false
		s.localMarked = true
	case ".type":
		parts := faSplitTop(rest, ',')
		if len(parts) != 2 {
			return fmt.Errorf("line %d: bad .type %q", lineno, line)
		}
		name := strings.TrimSpace(parts[0])
		kind := strings.TrimSpace(parts[1])
		s := st.sym(name)
		switch kind {
		case "@function":
			s.typ = "func"
		case "@object":
			s.typ = "object"
		default:
			return fmt.Errorf("line %d: unsupported .type kind %q", lineno, kind)
		}
	case ".size":
		parts := faSplitTop(rest, ',')
		if len(parts) != 2 {
			return fmt.Errorf("line %d: bad .size %q", lineno, line)
		}
		name := strings.TrimSpace(parts[0])
		expr := strings.TrimSpace(parts[1])
		dot, symRef, ok := faParseSizeExpr(expr)
		if !ok || symRef != name || !dot {
			return fmt.Errorf("line %d: unsupported .size expr %q", lineno, expr)
		}
		m := faSizeMark{sym: name, sec: st.cur}
		if st.cur == ".text" {
			m.pos = len(st.text)
		} else {
			m.pos = int(st.curLen())
		}
		st.sizeMarks = append(st.sizeMarks, m)
	case ".align":
		n, err := faParseInt(strings.Fields(rest)[0])
		if err != nil {
			return fmt.Errorf("line %d: bad .align %q: %v", lineno, line, err)
		}
		if err := st.alignCur(n, lineno); err != nil {
			return err
		}
	case ".string":
		b, err := faDecodeString(rest)
		if err != nil {
			return fmt.Errorf("line %d: bad .string: %v", lineno, err)
		}
		buf := st.curBuf()
		if buf == nil {
			return fmt.Errorf("line %d: .string outside data section", lineno)
		}
		*buf = append(*buf, b...)
		*buf = append(*buf, 0)
	case ".double":
		f, err := strconv.ParseFloat(strings.Fields(rest)[0], 64)
		if err != nil {
			return fmt.Errorf("line %d: bad .double %q", lineno, line)
		}
		buf := st.curBuf()
		if buf == nil {
			return fmt.Errorf("line %d: .double outside data section", lineno)
		}
		var tmp [8]byte
		bits := math.Float64bits(f)
		tmp[0] = byte(bits)
		tmp[1] = byte(bits >> 8)
		tmp[2] = byte(bits >> 16)
		tmp[3] = byte(bits >> 24)
		tmp[4] = byte(bits >> 32)
		tmp[5] = byte(bits >> 40)
		tmp[6] = byte(bits >> 48)
		tmp[7] = byte(bits >> 56)
		*buf = append(*buf, tmp[:]...)
	case ".float":
		f, err := strconv.ParseFloat(strings.Fields(rest)[0], 64)
		if err != nil {
			return fmt.Errorf("line %d: bad .float %q", lineno, line)
		}
		buf := st.curBuf()
		if buf == nil {
			return fmt.Errorf("line %d: .float outside data section", lineno)
		}
		var tmp [4]byte
		bits := math.Float32bits(float32(f))
		tmp[0] = byte(bits)
		tmp[1] = byte(bits >> 8)
		tmp[2] = byte(bits >> 16)
		tmp[3] = byte(bits >> 24)
		*buf = append(*buf, tmp[:]...)
	case ".long":
		v, err := faParseInt(strings.Fields(rest)[0])
		if err != nil {
			return fmt.Errorf("line %d: bad .long %q: %v", lineno, line, err)
		}
		buf := st.curBuf()
		if buf == nil {
			return fmt.Errorf("line %d: .long outside data section", lineno)
		}
		*buf = append(*buf, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	case ".quad":
		op := strings.TrimSpace(rest)
		buf := st.curBuf()
		if buf == nil {
			return fmt.Errorf("line %d: .quad outside data section", lineno)
		}
		if strings.HasPrefix(op, ".") {
			// ".", ". + N", ".-N": absolute reference to current location.
			extra := int64(0)
			t := strings.ReplaceAll(strings.TrimSpace(op[1:]), " ", "")
			if t != "" {
				v, err := faParseInt(t)
				if err != nil {
					return fmt.Errorf("line %d: bad .quad expr %q", lineno, line)
				}
				extra = int64(v)
			}
			off := uint64(len(*buf))
			*buf = append(*buf, 0, 0, 0, 0, 0, 0, 0, 0)
			st.relocs = append(st.relocs, ObjReloc{
				Section: st.cur, Offset: off, Symbol: st.cur,
				Type: R_X86_64_64, Addend: int64(off) + extra,
			})
		} else {
			v, err := faParseInt(strings.Fields(op)[0])
			if err != nil {
				return fmt.Errorf("line %d: bad .quad %q: %v", lineno, line, err)
			}
			for i := 0; i < 8; i++ {
				*buf = append(*buf, byte(v>>(8*i)))
			}
		}
	case ".zero":
		n, err := faParseInt(strings.Fields(rest)[0])
		if err != nil {
			return fmt.Errorf("line %d: bad .zero %q: %v", lineno, line, err)
		}
		switch st.cur {
		case ".rodata", ".data":
			buf := st.curBuf()
			*buf = append(*buf, make([]byte, n)...)
		case ".bss":
			st.bssSize += n
		default:
			return fmt.Errorf("line %d: .zero in %s", lineno, st.cur)
		}
	case ".comm":
		parts := faSplitTop(rest, ',')
		if len(parts) != 3 {
			return fmt.Errorf("line %d: bad .comm %q", lineno, line)
		}
		name := strings.TrimSpace(parts[0])
		size, err := faParseInt(strings.TrimSpace(parts[1]))
		if err != nil {
			return fmt.Errorf("line %d: bad .comm size: %v", lineno, err)
		}
		align, err := faParseInt(strings.TrimSpace(parts[2]))
		if err != nil {
			return fmt.Errorf("line %d: bad .comm align: %v", lineno, err)
		}
		s := st.sym(name)
		if s.defined || s.isCommon {
			return fmt.Errorf("line %d: duplicate symbol %s", lineno, name)
		}
		s.isCommon = true
		s.commonSize = size
		s.commonAlign = align
	default:
		return fmt.Errorf("line %d: unsupported directive %q", lineno, dir)
	}
	return nil
}

func faParseSizeExpr(expr string) (dot bool, sym string, ok bool) {
	// forms: ".-name"
	t := strings.TrimSpace(expr)
	if !strings.HasPrefix(t, ".-") {
		return false, "", false
	}
	return true, strings.TrimSpace(t[2:]), true
}

func faDecodeString(s string) ([]byte, error) {
	t := strings.TrimSpace(s)
	if len(t) < 2 || t[0] != '"' || t[len(t)-1] != '"' {
		return nil, fmt.Errorf("not a quoted string: %s", s)
	}
	body := t[1 : len(t)-1]
	var out []byte
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		i++
		if i >= len(body) {
			return nil, fmt.Errorf("trailing backslash")
		}
		e := body[i]
		switch e {
		case 'n':
			out = append(out, '\n')
		case 't':
			out = append(out, '\t')
		case 'r':
			out = append(out, '\r')
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'v':
			out = append(out, '\v')
		case '\\':
			out = append(out, '\\')
		case '"':
			out = append(out, '"')
		case '\'':
			out = append(out, '\'')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			v := e - '0'
			for k := 0; k < 2 && i+1 < len(body) && body[i+1] >= '0' && body[i+1] <= '7'; k++ {
				i++
				v = v*8 + (body[i] - '0')
			}
			out = append(out, v)
		default:
			return nil, fmt.Errorf("unknown escape \\%c", e)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Layout with branch relaxation
// ---------------------------------------------------------------------------

func faJumpShortLen(ins *faInstr) int { return 2 }

func faJumpNearLen(ins *faInstr) int {
	if ins.jcc {
		return 6
	}
	return 5
}

// sizeInstructions runs the sizing pre-pass: every non-jump instruction
// is encoded once (lengths are label-independent) and cacheable lines
// carry their bytes for finish. Large inputs split across a worker pool;
// each worker encodes into scratch and copies cacheable bytes onto its
// instructions (no shared memo: total work is the same, wall time isn't).
// Workers never mutate shared state: symbol misses go to worker-local
// unknown lists (materialized sequentially afterwards), and relocations
// are suppressed (emitReloc=false).
func (st *faState) sizeInstructions() error {
	n := len(st.text)
	workers := 1
	if n >= 32768 && runtime.NumCPU() > 1 {
		workers = runtime.NumCPU()
		if workers > n {
			workers = n
		}
	}
	type faSizeResult struct {
		unknown []string
		err     error
	}
	results := make([]faSizeResult, workers)
	chunk := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			a := w * chunk
			b := a + chunk
			if b > n {
				b = n
			}
			// Worker-local state shares read-only tables; misses go to
			// the local unknown list, relocations are suppressed.
			var unknown []string
			wst := *st
			wst.unknown = &unknown
			sizeScratch := make([]byte, 0, 32)
			for i := a; i < b; i++ {
				ins := &st.text[i]
				if ins.isJump || (ins.sec == faSecText && strings.HasPrefix(ins.line, ".align")) {
					continue
				}
				benc, err := wst.encodeLine(ins.line, ins.sec.name(), 0, sizeScratch[:0], false, false)
				if err != nil {
					results[w].err = fmt.Errorf("line %d: %v", ins.lineno, err)
					return
				}
				if len(benc) > cap(sizeScratch) {
					sizeScratch = benc
				}
				ins.cachedSz = int32(len(benc))
				if ins.cacheable {
					ins.enc = append([]byte(nil), benc...)
				}
			}
			results[w].unknown = unknown
		}(w)
	}
	wg.Wait()
	for w := range results {
		if results[w].err != nil {
			return results[w].err
		}
	}
	// Materialize placeholders for worker-observed undefined symbols
	// (chunk order: deterministic).
	seen := make(map[string]bool)
	for w := range results {
		for _, name := range results[w].unknown {
			if !seen[name] {
				seen[name] = true
				st.sym(name)
			}
		}
	}
	return nil
}

func (st *faState) layout() error {
	if err := st.sizeInstructions(); err != nil {
		return err
	}
	insSize := func(ins *faInstr, off uint64) int {
		if ins.sec == faSecText && strings.HasPrefix(ins.line, ".align") {
			n := uint64(-ins.cachedSz)
			if n <= 1 {
				n = 1
			}
			return int((n - off%n) % n)
		}
		if ins.isJump {
			if ins.near {
				return faJumpNearLen(ins)
			}
			return faJumpShortLen(ins)
		}
		return int(ins.cachedSz)
	}
	// Mark jumps to undefined symbols as near up front.
	for i := range st.text {
		ins := &st.text[i]
		if !ins.isJump {
			continue
		}
		if s, ok := st.syms[ins.target]; !ok || (!s.defined && !s.isCommon) {
			ins.near = true
		}
	}
	st.textOffs = make([]uint64, len(st.text))
	for iter := 0; iter < 30; iter++ {
		off := uint64(0)
		for i := range st.text {
			st.textOffs[i] = off
			off += uint64(insSize(&st.text[i], off))
		}
		labelOff := func(name string) (uint64, bool) {
			if idx, ok := st.labels[name]; ok {
				if idx < len(st.text) {
					return st.textOffs[idx], true
				}
				return off, true
			}
			return 0, false
		}
		changed := false
		for i := range st.text {
			ins := &st.text[i]
			if !ins.isJump || ins.near {
				continue
			}
			t, ok := labelOff(ins.target)
			if !ok {
				ins.near = true
				changed = true
				continue
			}
			d := int64(t) - (int64(st.textOffs[i]) + int64(faJumpShortLen(ins)))
			if d < -128 || d > 127 {
				ins.near = true
				changed = true
			}
		}
		if !changed {
			break
		}
		if iter == 29 {
			return fmt.Errorf("branch relaxation did not converge")
		}
	}
	// Final offsets.
	off := uint64(0)
	st.textOffs = make([]uint64, len(st.text))
	for i := range st.text {
		st.textOffs[i] = off
		off += uint64(insSize(&st.text[i], off))
	}
	st.textSize = off
	// Assign .text label offsets.
	for name, idx := range st.labels {
		var o uint64
		if idx < len(st.text) {
			o = st.textOffs[idx]
		} else {
			o = off
		}
		st.syms[name].offset = o
	}
	// Resolve .size marks.
	for _, m := range st.sizeMarks {
		s := st.syms[m.sym]
		if s == nil || (!s.defined && !s.isCommon) {
			return fmt.Errorf(".size for unknown symbol %s", m.sym)
		}
		var end uint64
		if m.sec == ".text" {
			if m.pos < len(st.text) {
				end = st.textOffs[m.pos]
			} else {
				end = off
			}
		} else {
			end = uint64(m.pos)
		}
		if end < s.offset {
			return fmt.Errorf(".size for %s is negative", m.sym)
		}
		s.size = end - s.offset
	}
	return nil
}

// ---------------------------------------------------------------------------
// Final encoding
// ---------------------------------------------------------------------------

func (st *faState) finish() (*ObjectFile, error) {
	// Large inputs encode in parallel (per-instruction encodings are
	// independent once layout fixed every offset); small inputs keep the
	// sequential loop.
	if len(st.text) >= 32768 && runtime.NumCPU() > 1 {
		return st.finishParallel()
	}
	text := make([]byte, 0, int(st.textSize))
	// Reused encode scratch: encodeLine and friends compute the RIP/call
	// displacement position as len(out), which is only valid when each
	// instruction encodes into an empty buffer. Keep that invariant and
	// copy the finished bytes into text (same as before, minus the alloc).
	var scratch []byte
	for i := range st.text {
		ins := &st.text[i]
		if ins.sec == faSecText && strings.HasPrefix(ins.line, ".align") {
			n := uint64(-ins.cachedSz)
			if n <= 1 {
				n = 1
			}
			pad := (n - uint64(len(text))%n) % n
			for j := uint64(0); j < pad; j++ {
				text = append(text, 0x90)
			}
			continue
		}
		if err := st.finishOne(st, ins, uint64(len(text)), &scratch, &text); err != nil {
			return nil, err
		}
	}
	return st.finishObj(text), nil
}

// finishOne encodes a single non-align instruction at absolute offset off
// into text: memo-carried bytes are copied, otherwise encodeLine runs into
// scratch. Encoding happens on wst (== st sequentially; a worker-local
// shallow copy with its own reloc list in the parallel path, since
// encodeLine appends relocations to its receiver).
func (st *faState) finishOne(wst *faState, ins *faInstr, off uint64, scratch *[]byte, text *[]byte) error {
	// Cacheable lines were encoded once in layout and carry their
	// bytes on the instruction (no map re-hash, no re-encode).
	if ins.enc != nil {
		if len(ins.enc) != int(ins.cachedSz) && !ins.isJump {
			return fmt.Errorf("line %d: internal size mismatch", ins.lineno)
		}
		*text = append(*text, ins.enc...)
		return nil
	}
	var err error
	*scratch, err = wst.encodeLine(ins.line, ins.sec.name(), off, (*scratch)[:0], true, ins.near)
	if err != nil {
		return fmt.Errorf("line %d: %v", ins.lineno, err)
	}
	got := len(*scratch)
	if !strings.HasPrefix(ins.line, ".align") && got != int(ins.cachedSz) && !ins.isJump {
		return fmt.Errorf("line %d: internal size mismatch", ins.lineno)
	}
	if ins.isJump {
		want := faJumpShortLen(ins)
		if ins.near {
			want = faJumpNearLen(ins)
		}
		if got != want {
			return fmt.Errorf("line %d: internal jump size mismatch", ins.lineno)
		}
	}
	*text = append(*text, (*scratch)...)
	return nil
}

// finishParallel encodes index-contiguous chunks of st.text on a worker
// pool, each writing its absolute range of the pre-sized output and
// collecting its own relocations (merged in order afterwards).
//
// Safety: post-layout, encodeLine only reads shared state (syms, labels,
// textOffs) — except st.sym() placeholder inserts for symbols never seen
// before, which layout's sizing pass already triggered for every non-jump
// line. Jump targets are pre-seeded below, so workers never write shared
// maps; relocations are the only writes and they go to worker-local slices.
func (st *faState) finishParallel() (*ObjectFile, error) {
	for i := range st.text {
		if ins := &st.text[i]; ins.isJump {
			st.sym(ins.target)
		}
	}
	text := make([]byte, st.textSize)
	workers := runtime.NumCPU()
	if workers > len(st.text) {
		workers = len(st.text)
	}
	type faChunkResult struct {
		relocs []ObjReloc
		err    error
	}
	results := make([]faChunkResult, workers)
	chunk := (len(st.text) + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			a := w * chunk
			b := a + chunk
			if b > len(st.text) {
				b = len(st.text)
			}
			// Worker-local faState shares all read-only tables and owns
			// only its relocation list. wtext aliases this chunk's exact
			// range of the pre-sized output (append-bounded by full slice
			// tricks would clobber neighbors; cap it at the chunk end).
			wst := *st
			wst.relocs = make([]ObjReloc, 0, (b-a)/12)
			end := st.textSize
			if b < len(st.text) {
				end = st.textOffs[b]
			}
			// Zero-length slice capped at the chunk end: appends fill
			// exactly this worker's range of the shared output.
			wtext := text[st.textOffs[a]:st.textOffs[a]:end]
			var scratch []byte
			for i := a; i < b; i++ {
				ins := &st.text[i]
				pos := st.textOffs[a] + uint64(len(wtext))
				if pos != st.textOffs[i] {
					results[w].err = fmt.Errorf("line %d: internal offset mismatch w=%d a=%d b=%d i=%d pos=%d want=%d isJump=%v near=%v enc=%d cachedSz=%d line=%.80q", ins.lineno, w, a, b, i, pos, st.textOffs[i], ins.isJump, ins.near, len(ins.enc), ins.cachedSz, ins.line)
					return
				}
				if ins.sec == faSecText && strings.HasPrefix(ins.line, ".align") {
					n := uint64(-ins.cachedSz)
					if n <= 1 {
						n = 1
					}
					pad := (n - pos%n) % n
					for j := uint64(0); j < pad; j++ {
						wtext = append(wtext, 0x90)
					}
					continue
				}
				if err := st.finishOne(&wst, ins, pos, &scratch, &wtext); err != nil {
					results[w].err = err
					return
				}
			}
			results[w].relocs = wst.relocs
		}(w)
	}
	wg.Wait()
	for w := range results {
		if results[w].err != nil {
			return nil, results[w].err
		}
	}
	for w := range results {
		st.relocs = append(st.relocs, results[w].relocs...)
	}
	return st.finishObj(text), nil
}

// finishObj builds the ObjectFile from finished section bytes (shared by
// the sequential and parallel finish paths).
func (st *faState) finishObj(text []byte) *ObjectFile {
	obj := &ObjectFile{
		Text:   text,
		Rodata: st.rodata,
		Data:   st.data,
		BSS:    st.bssSize,
	}
	for _, name := range st.order {
		s := st.syms[name]
		if s.isTemp {
			continue
		}
		if s.isCommon {
			obj.Symbols = append(obj.Symbols, ObjSymbol{
				Name: name, Section: "", Offset: 0,
				Global: true, Local: false, Size: s.commonSize,
			})
			continue
		}
		if !s.defined {
			obj.Symbols = append(obj.Symbols, ObjSymbol{
				Name: name, Section: "", Offset: 0,
				Global: true, Local: false, Size: 0,
			})
			continue
		}
		obj.Symbols = append(obj.Symbols, ObjSymbol{
			Name: name, Section: s.section, Offset: s.offset,
			Global: s.global, Local: s.localMarked, Size: s.size,
		})
	}
	obj.Relocs = append([]ObjReloc(nil), st.relocs...)
	// Register COMMON info and symbol types for the ELF writer.
	m := make(map[string]faCommonInfo)
	em := make(map[string]faSymExtra)
	for _, s := range st.syms {
		if s.isCommon {
			m[s.name] = faCommonInfo{size: s.commonSize, align: s.commonAlign}
			em[s.name] = faSymExtra{isCommon: true, align: s.commonAlign, typ: s.typ}
		} else if s.defined || s.order >= 0 {
			em[s.name] = faSymExtra{typ: s.typ}
		}
	}
	faCommonTable[obj] = m
	faExtraTable[obj] = em
	return obj
}

// ---------------------------------------------------------------------------
// Jump condition codes
// ---------------------------------------------------------------------------

func faJccCode(mn string) int {
	switch mn {
	case "jo":
		return 0x70
	case "jno":
		return 0x71
	case "jb", "jnae":
		return 0x72
	case "jae", "jnb":
		return 0x73
	case "jz", "je":
		return 0x74
	case "jnz", "jne":
		return 0x75
	case "jbe", "jna":
		return 0x76
	case "ja", "jnbe":
		return 0x77
	case "js":
		return 0x78
	case "jns":
		return 0x79
	case "jp", "jpe":
		return 0x7A
	case "jnp", "jpo":
		return 0x7B
	case "jl", "jnge":
		return 0x7C
	case "jge", "jnl":
		return 0x7D
	case "jle", "jng":
		return 0x7E
	case "jg", "jnle":
		return 0x7F
	}
	return -1
}

func faSetccCode(mn string) int {
	switch mn {
	case "seto":
		return 0x90
	case "setno":
		return 0x91
	case "setb", "setnae":
		return 0x92
	case "setae", "setnb":
		return 0x93
	case "sete", "setz":
		return 0x94
	case "setne", "setnz":
		return 0x95
	case "setbe", "setna":
		return 0x96
	case "seta", "setnbe":
		return 0x97
	case "sets":
		return 0x98
	case "setns":
		return 0x99
	case "setp", "setpe":
		return 0x9A
	case "setnp", "setpo":
		return 0x9B
	case "setl", "setnge":
		return 0x9C
	case "setge", "setnl":
		return 0x9D
	case "setle", "setng":
		return 0x9E
	case "setg", "setnle":
		return 0x9F
	}
	return -1
}

// ---------------------------------------------------------------------------
// Encoding helpers
// ---------------------------------------------------------------------------

// faEmitPrefix appends legacy prefix + REX as needed.
func faEmitPrefix(out []byte, legacy byte, w bool, regField int, a *faAddr, forceRex bool) []byte {
	if legacy != 0 {
		out = append(out, legacy)
	}
	rex := byte(0x40)
	if w {
		rex |= 0x08
	}
	if regField >= 8 {
		rex |= 0x04
	}
	if a != nil {
		if a.isReg {
			if a.reg >= 8 {
				rex |= 0x01
			}
		} else if !a.rip {
			if a.index >= 8 {
				rex |= 0x02
			}
			if a.base >= 8 {
				rex |= 0x01
			}
		}
	}
	if rex != 0x40 || forceRex {
		out = append(out, rex)
	}
	return out
}

// faEmitRM appends ModRM (+SIB+disp). RIP refs get 4 zero bytes (patched or
// relocated by the caller). regField is a full register code; only its low
// 3 bits go into ModRM (the REX.R bit is handled by faEmitPrefix).
func faEmitRM(out []byte, regField int, a faAddr) []byte {
	rf := byte(regField & 7)
	if a.isReg {
		return append(out, 0xC0|rf<<3|byte(a.reg&7))
	}
	if a.rip {
		out = append(out, rf<<3|0x05)
		return append(out, 0, 0, 0, 0)
	}
	needSIB := a.index >= 0 || (a.base&7) == 4
	var mod byte
	switch {
	case a.base < 0:
		mod = 0x00 // disp32 follows
	case (a.base&7) == 5 && a.disp == 0:
		mod = 0x40 // disp8 0 (rbp/r13 cannot use mod 00)
	case a.disp == 0:
		mod = 0x00
	case a.disp >= -128 && a.disp <= 127:
		mod = 0x40
	default:
		mod = 0x80
	}
	rm := byte(4)
	if !needSIB {
		rm = byte(a.base & 7)
	}
	out = append(out, mod|rf<<3|rm)
	if needSIB {
		si := byte(4)
		if a.index >= 0 {
			si = byte(a.index & 7)
		}
		bi := byte(5)
		if a.base >= 0 {
			bi = byte(a.base & 7)
		}
		var sc byte
		switch a.scale {
		case 1:
			sc = 0
		case 2:
			sc = 1
		case 4:
			sc = 2
		case 8:
			sc = 3
		}
		out = append(out, sc<<6|si<<3|bi)
	}
	switch mod {
	case 0x40:
		if a.base >= 0 && (a.base&7) == 5 && a.disp == 0 {
			out = append(out, 0)
		} else {
			out = append(out, byte(int8(a.disp)))
		}
	case 0x80:
		d := int32(a.disp)
		out = append(out, byte(d), byte(d>>8), byte(d>>16), byte(d>>24))
	default:
		if a.base < 0 {
			d := int32(a.disp)
			out = append(out, byte(d), byte(d>>8), byte(d>>16), byte(d>>24))
		}
	}
	return out
}

func faAppendU32(out []byte, v uint64) []byte {
	return append(out, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func faAppendU64(out []byte, v uint64) []byte {
	for i := 0; i < 8; i++ {
		out = append(out, byte(v>>(8*uint(i))))
	}
	return out
}

// faRipRef emits a 4-byte RIP displacement (zeros) plus the appropriate
// relocation, or patches a locally-resolved displacement. dispPos is the
// index in out where the 4 displacement bytes start.
func (st *faState) faRipRef(out []byte, sec string, off uint64, dispPos int, sym string, extra int64, emitReloc bool) ([]byte, error) {
	s := st.lookupSym(sym)
	resolveLocal := false
	if s.isTemp {
		resolveLocal = true
	} else if s.defined && !s.isCommon && s.section == sec {
		resolveLocal = true
	}
	if resolveLocal {
		d := int64(s.offset) + extra - (int64(off) + int64(len(out)))
		if d < -2147483648 || d > 2147483647 {
			return nil, fmt.Errorf("RIP displacement out of range for %s", sym)
		}
		v := uint32(int32(d))
		out[dispPos] = byte(v)
		out[dispPos+1] = byte(v >> 8)
		out[dispPos+2] = byte(v >> 16)
		out[dispPos+3] = byte(v >> 24)
		return out, nil
	}
	if !emitReloc {
		return out, nil
	}
	if s.defined && !s.isCommon && !s.global {
		// Static local defined in another section: relocate vs section.
		st.relocs = append(st.relocs, ObjReloc{
			Section: sec, Offset: off + uint64(dispPos), Symbol: s.section,
			Type: R_X86_64_PC32, Addend: int64(s.offset) + extra - 4,
		})
	} else {
		// Global defined, COMMON or undefined: relocate vs the symbol.
		st.relocs = append(st.relocs, ObjReloc{
			Section: sec, Offset: off + uint64(dispPos), Symbol: sym,
			Type: R_X86_64_PC32, Addend: extra - 4,
		})
	}
	return out, nil
}

// faCallTarget encodes a direct call.
func (st *faState) faCallTarget(out []byte, sec string, off uint64, target string, emitReloc bool) ([]byte, error) {
	s := st.lookupSym(target)
	if s.defined && !s.global && !s.isCommon && s.section == sec {
		d := int64(s.offset) - (int64(off) + 5)
		if d < -2147483648 || d > 2147483647 {
			return nil, fmt.Errorf("call displacement out of range for %s", target)
		}
		out = append(out, 0xE8)
		return faAppendU32(out, uint64(uint32(int32(d)))), nil
	}
	out = append(out, 0xE8, 0, 0, 0, 0)
	if emitReloc {
		st.relocs = append(st.relocs, ObjReloc{
			Section: sec, Offset: off + 1, Symbol: target,
			Type: R_X86_64_PLT32, Addend: -4,
		})
	}
	return out, nil
}

// faJumpTarget encodes jmp/jcc to target; near flag comes from relaxation.
func (st *faState) faJumpTarget(out []byte, sec string, off uint64, shortOp byte, target string, isJcc bool, near bool, emitReloc bool) ([]byte, error) {
	s := st.lookupSym(target)
	if !s.defined && !s.isCommon {
		// Undefined: near form + PLT32.
		if isJcc {
			out = append(out, 0x0F, shortOp+0x10)
		} else {
			out = append(out, 0xE9)
		}
		out = append(out, 0, 0, 0, 0)
		if emitReloc {
			st.relocs = append(st.relocs, ObjReloc{
				Section: sec, Offset: off + uint64(len(out)) - 4, Symbol: target,
				Type: R_X86_64_PLT32, Addend: -4,
			})
		}
		return out, nil
	}
	if near {
		var nlen int
		if isJcc {
			out = append(out, 0x0F, shortOp+0x10)
			nlen = 6
		} else {
			out = append(out, 0xE9)
			nlen = 5
		}
		d := int64(s.offset) - (int64(off) + int64(nlen))
		if d < -2147483648 || d > 2147483647 {
			return nil, fmt.Errorf("jump displacement out of range for %s", target)
		}
		return faAppendU32(out, uint64(uint32(int32(d)))), nil
	}
	d := int64(s.offset) - (int64(off) + 2)
	if d < -128 || d > 127 {
		return nil, fmt.Errorf("internal error: short jump to %s out of range", target)
	}
	out = append(out, shortOp, byte(int8(d)))
	return out, nil
}

type faALUDesc struct {
	regOp byte
	immOp byte
	ext   byte
	accOp byte
	w64   bool
}

func faALUDescFor(mn string) (faALUDesc, bool) {
	switch mn {
	case "addq":
		return faALUDesc{0x01, 0x81, 0, 0x05, true}, true
	case "subq":
		return faALUDesc{0x29, 0x81, 5, 0x2D, true}, true
	case "andq":
		return faALUDesc{0x21, 0x81, 4, 0x25, true}, true
	case "orq":
		return faALUDesc{0x09, 0x81, 1, 0x0D, true}, true
	case "xorq":
		return faALUDesc{0x31, 0x81, 6, 0x35, true}, true
	case "cmpq":
		return faALUDesc{0x39, 0x81, 7, 0x3D, true}, true
	case "cmpl", "cmp":
		return faALUDesc{0x39, 0x81, 7, 0x3D, false}, true
	}
	return faALUDesc{}, false
}

func (st *faState) faALU(out []byte, sec string, off uint64, d faALUDesc, src, dst string, emitReloc bool) ([]byte, error) {
	_ = emitReloc
	if strings.HasPrefix(src, "$") {
		imm, err := faParseInt(src[1:])
		if err != nil {
			return nil, err
		}
		dr, ok := faParseReg(dst)
		if !ok {
			return nil, fmt.Errorf("bad ALU destination %s", dst)
		}
		if d.w64 && dr.size != 8 {
			return nil, fmt.Errorf("bad ALU destination %s", dst)
		}
		if !d.w64 && dr.size != 4 {
			return nil, fmt.Errorf("bad ALU destination %s", dst)
		}
		var wbit bool
		var accReg int
		if d.w64 {
			wbit = true
			accReg = REG_RAX
		} else {
			accReg = REG_RAX // same code; W=0 selects eax
		}
		if faFitsInt8(imm) {
			out = faEmitPrefix(out, 0, wbit, int(d.ext), &faAddr{isReg: true, reg: dr.code}, false)
			out = append(out, 0x83, 0xC0|d.ext<<3|byte(dr.code&7), byte(int8(int64(imm))))
			return out, nil
		}
		if dr.code == accReg {
			out = faEmitPrefix(out, 0, wbit, 0, &faAddr{isReg: true, reg: 0}, false)
			out = append(out, d.accOp)
			return faAppendU32(out, imm), nil
		}
		out = faEmitPrefix(out, 0, wbit, int(d.ext), &faAddr{isReg: true, reg: dr.code}, false)
		out = append(out, d.immOp, 0xC0|d.ext<<3|byte(dr.code&7))
		return faAppendU32(out, imm), nil
	}
	sr, ok := faParseReg(src)
	if !ok {
		return nil, fmt.Errorf("bad ALU source %s", src)
	}
	dr, ok := faParseReg(dst)
	if !ok {
		return nil, fmt.Errorf("bad ALU destination %s", dst)
	}
	if d.w64 && (sr.size != 8 || dr.size != 8) {
		return nil, fmt.Errorf("bad ALU operands %s, %s", src, dst)
	}
	if !d.w64 && (sr.size != 4 || dr.size != 4) {
		return nil, fmt.Errorf("bad ALU operands %s, %s", src, dst)
	}
	out = faEmitPrefix(out, 0, d.w64, sr.code, &faAddr{isReg: true, reg: dr.code}, false)
	out = append(out, d.regOp, 0xC0|byte(sr.code&7)<<3|byte(dr.code&7))
	return out, nil
}

// ---------------------------------------------------------------------------
// Instruction encoding dispatcher
// ---------------------------------------------------------------------------

func (st *faState) encodeLine(line, sec string, off uint64, out []byte, emitReloc, jumpNear bool) ([]byte, error) {
	mn, rest := faSplitMn(line)
	mn = faLower(mn)
	var opsBuf [3]string
	var ops []string
	if rest != "" {
		ops = faSplitTopInto(rest, ',', opsBuf[:0])
	}
	var err error
	switch mn {
	case "movq":
		out, err = st.faMov(out, sec, off, ops, true, false, emitReloc)
	case "movl", "mov":
		out, err = st.faMov(out, sec, off, ops, false, false, emitReloc)
	case "movabs":
		out, err = st.faMovAbs(out, ops)
	case "movb":
		out, err = st.faMovB(out, sec, off, ops, emitReloc)
	case "movw":
		out, err = st.faMovW(out, sec, off, ops, emitReloc)
	case "movzbq":
		out, err = st.faMovzx(out, sec, off, ops, true, 1, 8, false, emitReloc)
	case "movzbl":
		out, err = st.faMovzx(out, sec, off, ops, false, 1, 4, false, emitReloc)
	case "movzwl":
		out, err = st.faMovzx(out, sec, off, ops, false, 2, 4, false, emitReloc)
	case "movzwq":
		out, err = st.faMovzx(out, sec, off, ops, true, 2, 8, false, emitReloc)
	case "movsbl":
		out, err = st.faMovzx(out, sec, off, ops, false, 1, 4, true, emitReloc)
	case "movswl":
		out, err = st.faMovzx(out, sec, off, ops, false, 2, 4, true, emitReloc)
	case "movswq":
		out, err = st.faMovzx(out, sec, off, ops, true, 2, 8, true, emitReloc)
	case "movslq":
		out, err = st.faMovslq(out, ops)
	case "movd":
		out, err = st.faMovd(out, ops)
	case "movsd":
		out, err = st.faMovsd(out, sec, off, ops, emitReloc)
	case "addq", "subq", "andq", "orq", "xorq", "cmpq", "cmpl", "cmp":
		if len(ops) != 2 {
			err = fmt.Errorf("bad %s operands", mn)
			break
		}
		d, _ := faALUDescFor(mn)
		out, err = st.faALU(out, sec, off, d, ops[0], ops[1], emitReloc)
		_ = sec
		_ = off
	case "testq":
		out, err = st.faTestq(out, ops)
	case "xorl":
		out, err = st.faXorl(out, ops)
	case "andb":
		out, err = st.faALU8(out, ops, 0x20)
	case "orb":
		out, err = st.faALU8(out, ops, 0x08)
	case "negq":
		out, err = st.faNegq(out, ops)
	case "notq":
		out, err = st.faNotq(out, ops)
	case "imulq":
		out, err = st.faImul(out, ops)
	case "idivl":
		out, err = st.faDiv(out, ops, false, true)
	case "idivq":
		out, err = st.faDiv(out, ops, true, true)
	case "divl":
		out, err = st.faDiv(out, ops, false, false)
	case "divq":
		out, err = st.faDiv(out, ops, true, false)
	case "cdq":
		out = append(out, 0x99)
	case "cqto":
		out = append(out, 0x48, 0x99)
	case "leaq":
		out, err = st.faLea(out, sec, off, ops, emitReloc)
	case "call":
		out, err = st.faCall(out, sec, off, ops, emitReloc)
	case "jmp":
		if len(ops) != 1 {
			err = fmt.Errorf("bad jmp operands")
			break
		}
		out, err = st.faJumpTarget(out, sec, off, 0xEB, ops[0], false, jumpNear, emitReloc)
	case "pushq":
		out, err = faPushPop(out, ops, true)
	case "popq":
		out, err = faPushPop(out, ops, false)
	case "ret":
		out = append(out, 0xC3)
	case "nop":
		out = append(out, 0x90)
	case "salq", "shlq", "sarq", "shrq":
		out, err = st.faShift(out, mn, ops)
	case "addsd":
		out, err = faSSE(out, ops, 0xF2, 0x58)
	case "subsd":
		out, err = faSSE(out, ops, 0xF2, 0x5C)
	case "mulsd":
		out, err = faSSE(out, ops, 0xF2, 0x59)
	case "divsd":
		out, err = faSSE(out, ops, 0xF2, 0x5E)
	case "ucomisd":
		out, err = faSSE(out, ops, 0x66, 0x2E)
	case "cvtss2sd":
		out, err = st.faCvtSS2SD(out, sec, off, ops, emitReloc)
	case "cvtsd2ss":
		out, err = faSSE(out, ops, 0xF2, 0x5A)
	case "cvtsi2sd":
		out, err = st.faCvtsi2sd(out, ops, false)
	case "cvtsi2sdq":
		out, err = st.faCvtsi2sd(out, ops, true)
	case "cvttsd2si":
		out, err = faCvttsd2si(out, ops)
	default:
		if code := faJccCode(mn); code != -1 {
			if len(ops) != 1 {
				err = fmt.Errorf("bad %s operands", mn)
				break
			}
			out, err = st.faJumpTarget(out, sec, off, byte(code), ops[0], true, jumpNear, emitReloc)
			break
		}
		if code := faSetccCode(mn); code != -1 {
			out, err = faSetcc(out, ops, byte(code))
			break
		}
		err = fmt.Errorf("unsupported mnemonic %q", mn)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// faMov encodes movq/movl.
func (st *faState) faMov(out []byte, sec string, off uint64, ops []string, w64, _ bool, emitReloc bool) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("mov needs 2 operands")
	}
	src, dst := ops[0], ops[1]
	if strings.HasPrefix(src, "$") {
		imm, err := faParseInt(src[1:])
		if err != nil {
			return nil, err
		}
		if dr, ok := faParseReg(dst); ok {
			if w64 && dr.size != 8 {
				return nil, fmt.Errorf("bad mov destination %s", dst)
			}
			if !w64 && dr.size != 4 {
				return nil, fmt.Errorf("bad mov destination %s", dst)
			}
			if w64 && faFitsInt32(imm) {
				out = faEmitPrefix(out, 0, true, 0, &faAddr{isReg: true, reg: dr.code}, false)
				out = append(out, 0xC7, 0xC0|byte(dr.code&7))
				return faAppendU32(out, imm), nil
			}
			if w64 {
				out = faEmitPrefix(out, 0, true, 0, &faAddr{isReg: true, reg: dr.code}, false)
				out = append(out, 0xB8+byte(dr.code&7))
				return faAppendU64(out, imm), nil
			}
			out = faEmitPrefix(out, 0, false, 0, &faAddr{isReg: true, reg: dr.code}, false)
			out = append(out, 0xB8+byte(dr.code&7))
			return faAppendU32(out, imm), nil
		}
		// immediate to memory
		a, err := faParseAddr(dst)
		if err != nil {
			return nil, err
		}
		if a.isReg || a.rip {
			return nil, fmt.Errorf("bad mov destination %s", dst)
		}
		if w64 {
			if !faFitsInt32(imm) {
				return nil, fmt.Errorf("mov immediate out of range: %s", src)
			}
			out = faEmitPrefix(out, 0, true, 0, &a, false)
			out = append(out, 0xC7)
			out = faEmitRM(out, 0, a)
			return faAppendU32(out, imm), nil
		}
		out = faEmitPrefix(out, 0, false, 0, &a, false)
		out = append(out, 0xC7)
		out = faEmitRM(out, 0, a)
		return faAppendU32(out, imm), nil
	}
	// xmm involved?
	if _, ok := faParseXMM(src); ok {
		if dr, ok := faParseReg(dst); ok {
			// xmm -> gp64
			if dr.size != 8 {
				return nil, fmt.Errorf("bad mov operands %s, %s", src, dst)
			}
			x, _ := faParseXMM(src)
			out = faEmitPrefix(out, 0x66, true, x, &faAddr{isReg: true, reg: dr.code}, false)
			out = append(out, 0x0F, 0x7E, 0xC0|byte(x&7)<<3|byte(dr.code&7))
			return out, nil
		}
		a, err := faParseAddr(dst)
		if err != nil {
			return nil, err
		}
		if a.isReg {
			return nil, fmt.Errorf("bad mov operands %s, %s", src, dst)
		}
		// xmm -> mem64 (MOVQ store)
		x, _ := faParseXMM(src)
		out = faEmitPrefix(out, 0x66, false, x, &a, false)
		out = append(out, 0x0F, 0xD6)
		return faEmitRM(out, x, a), nil
	}
	if _, ok := faParseXMM(dst); ok {
		x, _ := faParseXMM(dst)
		if sr, ok := faParseReg(src); ok {
			// gp64 -> xmm
			if sr.size != 8 {
				return nil, fmt.Errorf("bad mov operands %s, %s", src, dst)
			}
			out = faEmitPrefix(out, 0x66, true, x, &faAddr{isReg: true, reg: sr.code}, false)
			out = append(out, 0x0F, 0x6E, 0xC0|byte(x&7)<<3|byte(sr.code&7))
			return out, nil
		}
		a, err := faParseAddr(src)
		if err != nil {
			return nil, err
		}
		if a.isReg {
			return nil, fmt.Errorf("bad mov operands %s, %s", src, dst)
		}
		// m64 -> xmm (MOVQ load)
		out = faEmitPrefix(out, 0xF3, false, x, &a, false)
		out = append(out, 0x0F, 0x7E)
		dispPos := len(out)
		out = faEmitRM(out, x, a)
		if a.rip {
			return st.faRipRef(out, sec, off, dispPos+1, a.ripSym, a.ripAdd, emitReloc)
		}
		return out, nil
	}
	sr, srcIsReg := faParseReg(src)
	dr, dstIsReg := faParseReg(dst)
	if srcIsReg && dstIsReg {
		if w64 && (sr.size != 8 || dr.size != 8) {
			return nil, fmt.Errorf("bad mov operands %s, %s", src, dst)
		}
		if !w64 && (sr.size != 4 || dr.size != 4) {
			return nil, fmt.Errorf("bad mov operands %s, %s", src, dst)
		}
		out = faEmitPrefix(out, 0, w64, sr.code, &faAddr{isReg: true, reg: dr.code}, false)
		out = append(out, 0x89, 0xC0|byte(sr.code&7)<<3|byte(dr.code&7))
		return out, nil
	}
	if dstIsReg && !srcIsReg {
		a, err := faParseAddr(src)
		if err != nil {
			return nil, err
		}
		if w64 && dr.size != 8 {
			return nil, fmt.Errorf("bad mov operands %s, %s", src, dst)
		}
		if !w64 && dr.size != 4 {
			return nil, fmt.Errorf("bad mov operands %s, %s", src, dst)
		}
		out = faEmitPrefix(out, 0, w64, dr.code, &a, false)
		out = append(out, 0x8B)
		dispPos := len(out)
		out = faEmitRM(out, dr.code, a)
		if a.rip {
			return st.faRipRef(out, sec, off, dispPos+1, a.ripSym, a.ripAdd, emitReloc)
		}
		return out, nil
	}
	if srcIsReg && !dstIsReg {
		a, err := faParseAddr(dst)
		if err != nil {
			return nil, err
		}
		if w64 && sr.size != 8 {
			return nil, fmt.Errorf("bad mov operands %s, %s", src, dst)
		}
		if !w64 && sr.size != 4 {
			return nil, fmt.Errorf("bad mov operands %s, %s", src, dst)
		}
		out = faEmitPrefix(out, 0, w64, sr.code, &a, false)
		out = append(out, 0x89)
		dispPos := len(out)
		out = faEmitRM(out, sr.code, a)
		if a.rip {
			return st.faRipRef(out, sec, off, dispPos+1, a.ripSym, a.ripAdd, emitReloc)
		}
		return out, nil
	}
	return nil, fmt.Errorf("bad mov operands %s, %s", src, dst)
}

func (st *faState) faMovAbs(out []byte, ops []string) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("movabs needs 2 operands")
	}
	imm, err := faParseInt(strings.TrimPrefix(ops[0], "$"))
	if err != nil {
		return nil, err
	}
	dr, ok := faParseReg(ops[1])
	if !ok || dr.size != 8 {
		return nil, fmt.Errorf("bad movabs destination %s", ops[1])
	}
	out = faEmitPrefix(out, 0, true, 0, &faAddr{isReg: true, reg: dr.code}, false)
	out = append(out, 0xB8+byte(dr.code&7))
	return faAppendU64(out, imm), nil
}

func (st *faState) faMovB(out []byte, sec string, off uint64, ops []string, emitReloc bool) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("movb needs 2 operands")
	}
	src, dst := ops[0], ops[1]
	if sr, ok := faParseReg(src); ok {
		if sr.size != 1 {
			return nil, fmt.Errorf("bad movb operands %s, %s", src, dst)
		}
		a, err := faParseAddr(dst)
		if err != nil {
			return nil, err
		}
		if a.isReg || a.rip {
			return nil, fmt.Errorf("bad movb operands %s, %s", src, dst)
		}
		if sr.high8 && (a.base >= 8 || a.index >= 8) {
			return nil, fmt.Errorf("bad movb operands %s, %s", src, dst)
		}
		out = faEmitPrefix(out, 0, false, sr.code, &a, sr.needRex)
		out = append(out, 0x88)
		return faEmitRM(out, sr.code, a), nil
	}
	if dr, ok := faParseReg(dst); ok {
		if dr.size != 1 {
			return nil, fmt.Errorf("bad movb operands %s, %s", src, dst)
		}
		a, err := faParseAddr(src)
		if err != nil {
			return nil, err
		}
		if a.isReg {
			return nil, fmt.Errorf("bad movb operands %s, %s", src, dst)
		}
		out = faEmitPrefix(out, 0, false, dr.code, &a, dr.needRex)
		out = append(out, 0x8A)
		dispPos := len(out)
		out = faEmitRM(out, dr.code, a)
		if a.rip {
			return st.faRipRef(out, sec, off, dispPos+1, a.ripSym, a.ripAdd, emitReloc)
		}
		return out, nil
	}
	return nil, fmt.Errorf("bad movb operands %s, %s", src, dst)
}

func (st *faState) faMovW(out []byte, sec string, off uint64, ops []string, emitReloc bool) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("movw needs 2 operands")
	}
	src, dst := ops[0], ops[1]
	if sr, ok := faParseReg(src); ok {
		if sr.size != 2 {
			return nil, fmt.Errorf("bad movw operands %s, %s", src, dst)
		}
		a, err := faParseAddr(dst)
		if err != nil {
			return nil, err
		}
		if a.isReg || a.rip {
			return nil, fmt.Errorf("bad movw operands %s, %s", src, dst)
		}
		out = faEmitPrefix(out, 0x66, false, sr.code, &a, sr.needRex)
		out = append(out, 0x89)
		return faEmitRM(out, sr.code, a), nil
	}
	if dr, ok := faParseReg(dst); ok {
		if dr.size != 2 {
			return nil, fmt.Errorf("bad movw operands %s, %s", src, dst)
		}
		a, err := faParseAddr(src)
		if err != nil {
			return nil, err
		}
		if a.isReg {
			return nil, fmt.Errorf("bad movw operands %s, %s", src, dst)
		}
		out = faEmitPrefix(out, 0x66, false, dr.code, &a, dr.needRex)
		out = append(out, 0x8B)
		dispPos := len(out)
		out = faEmitRM(out, dr.code, a)
		if a.rip {
			return st.faRipRef(out, sec, off, dispPos+1, a.ripSym, a.ripAdd, emitReloc)
		}
		return out, nil
	}
	return nil, fmt.Errorf("bad movw operands %s, %s", src, dst)
}

// faMovzx encodes movzbq/movzbl/movzwl/movzwq/movsbl/movswl/movswq.
// dst64 selects REX.W; srcSize 1 or 2; dstSize 4 or 8; sext selects
// 0xBE/0xBF (sign-extend) vs 0xB6/0xB7 (zero-extend).
func (st *faState) faMovzx(out []byte, sec string, off uint64, ops []string, dst64 bool, srcSize, dstSize int, sext bool, emitReloc bool) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("movzx needs 2 operands")
	}
	src, dst := ops[0], ops[1]
	dr, ok := faParseReg(dst)
	if !ok || dr.size != dstSize {
		return nil, fmt.Errorf("bad movzx destination %s", dst)
	}
	op := byte(0xB6)
	if srcSize == 2 {
		op = 0xB7
	}
	if sext {
		op += 0x08 // 0xBE / 0xBF
	}
	var a faAddr
	if sr, ok := faParseReg(src); ok {
		if sr.size != srcSize {
			return nil, fmt.Errorf("bad movzx source %s", src)
		}
		a = faAddr{isReg: true, reg: sr.code, regSz: sr.size}
		out = faEmitPrefix(out, 0, dst64, dr.code, &a, sr.needRex)
		out = append(out, 0x0F, op)
		return faEmitRM(out, dr.code, a), nil
	}
	var err error
	a, err = faParseAddr(src)
	if err != nil {
		return nil, err
	}
	if a.isReg {
		return nil, fmt.Errorf("bad movzx source %s", src)
	}
	out = faEmitPrefix(out, 0, dst64, dr.code, &a, false)
	out = append(out, 0x0F, op)
	dispPos := len(out)
	out = faEmitRM(out, dr.code, a)
	if a.rip {
		return st.faRipRef(out, sec, off, dispPos+1, a.ripSym, a.ripAdd, emitReloc)
	}
	return out, nil
}

func (st *faState) faMovslq(out []byte, ops []string) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("movslq needs 2 operands")
	}
	sr, ok := faParseReg(ops[0])
	if !ok || sr.size != 4 {
		return nil, fmt.Errorf("bad movslq source %s", ops[0])
	}
	dr, ok := faParseReg(ops[1])
	if !ok || dr.size != 8 {
		return nil, fmt.Errorf("bad movslq destination %s", ops[1])
	}
	out = faEmitPrefix(out, 0, true, dr.code, &faAddr{isReg: true, reg: sr.code}, false)
	out = append(out, 0x63, 0xC0|byte(dr.code&7)<<3|byte(sr.code&7))
	return out, nil
}

func (st *faState) faMovd(out []byte, ops []string) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("movd needs 2 operands")
	}
	if x, ok := faParseXMM(ops[0]); ok {
		// xmm -> r32
		dr, ok := faParseReg(ops[1])
		if !ok || dr.size != 4 {
			return nil, fmt.Errorf("bad movd operands %s, %s", ops[0], ops[1])
		}
		out = faEmitPrefix(out, 0x66, false, x, &faAddr{isReg: true, reg: dr.code}, false)
		out = append(out, 0x0F, 0x7E, 0xC0|byte(x&7)<<3|byte(dr.code&7))
		return out, nil
	}
	if x, ok := faParseXMM(ops[1]); ok {
		// r32 -> xmm
		sr, ok := faParseReg(ops[0])
		if !ok || sr.size != 4 {
			return nil, fmt.Errorf("bad movd operands %s, %s", ops[0], ops[1])
		}
		out = faEmitPrefix(out, 0x66, false, x, &faAddr{isReg: true, reg: sr.code}, false)
		out = append(out, 0x0F, 0x6E, 0xC0|byte(x&7)<<3|byte(sr.code&7))
		return out, nil
	}
	return nil, fmt.Errorf("bad movd operands %s, %s", ops[0], ops[1])
}

func (st *faState) faMovsd(out []byte, sec string, off uint64, ops []string, emitReloc bool) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("movsd needs 2 operands")
	}
	src, dst := ops[0], ops[1]
	if x, ok := faParseXMM(src); ok {
		// store: xmm -> mem
		a, err := faParseAddr(dst)
		if err != nil {
			return nil, err
		}
		if a.isReg || a.rip {
			return nil, fmt.Errorf("bad movsd operands %s, %s", src, dst)
		}
		out = faEmitPrefix(out, 0xF2, false, x, &a, false)
		out = append(out, 0x0F, 0x11)
		return faEmitRM(out, x, a), nil
	}
	if x, ok := faParseXMM(dst); ok {
		// load: mem -> xmm
		a, err := faParseAddr(src)
		if err != nil {
			return nil, err
		}
		if a.isReg {
			return nil, fmt.Errorf("bad movsd operands %s, %s", src, dst)
		}
		out = faEmitPrefix(out, 0xF2, false, x, &a, false)
		out = append(out, 0x0F, 0x10)
		dispPos := len(out)
		out = faEmitRM(out, x, a)
		if a.rip {
			return st.faRipRef(out, sec, off, dispPos+1, a.ripSym, a.ripAdd, emitReloc)
		}
		return out, nil
	}
	return nil, fmt.Errorf("bad movsd operands %s, %s", src, dst)
}

func (st *faState) faTestq(out []byte, ops []string) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("testq needs 2 operands")
	}
	sr, ok := faParseReg(ops[0])
	if !ok || sr.size != 8 {
		return nil, fmt.Errorf("bad testq operands %s, %s", ops[0], ops[1])
	}
	dr, ok := faParseReg(ops[1])
	if !ok || dr.size != 8 {
		return nil, fmt.Errorf("bad testq operands %s, %s", ops[0], ops[1])
	}
	out = faEmitPrefix(out, 0, true, sr.code, &faAddr{isReg: true, reg: dr.code}, false)
	out = append(out, 0x85, 0xC0|byte(sr.code&7)<<3|byte(dr.code&7))
	return out, nil
}

func (st *faState) faXorl(out []byte, ops []string) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("xorl needs 2 operands")
	}
	sr, ok := faParseReg(ops[0])
	if !ok || sr.size != 4 {
		return nil, fmt.Errorf("bad xorl operands %s, %s", ops[0], ops[1])
	}
	dr, ok := faParseReg(ops[1])
	if !ok || dr.size != 4 {
		return nil, fmt.Errorf("bad xorl operands %s, %s", ops[0], ops[1])
	}
	out = faEmitPrefix(out, 0, false, sr.code, &faAddr{isReg: true, reg: dr.code}, false)
	out = append(out, 0x31, 0xC0|byte(sr.code&7)<<3|byte(dr.code&7))
	return out, nil
}

// faALU8 encodes 8-bit ALU reg,reg (andb/orb).
func (st *faState) faALU8(out []byte, ops []string, op byte) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("alu8 needs 2 operands")
	}
	sr, ok := faParseReg(ops[0])
	if !ok || sr.size != 1 {
		return nil, fmt.Errorf("bad alu8 operands %s, %s", ops[0], ops[1])
	}
	dr, ok := faParseReg(ops[1])
	if !ok || dr.size != 1 {
		return nil, fmt.Errorf("bad alu8 operands %s, %s", ops[0], ops[1])
	}
	if (sr.high8 || dr.high8) && (sr.code >= 8 || dr.code >= 8 || sr.needRex || dr.needRex) {
		return nil, fmt.Errorf("bad alu8 operands %s, %s", ops[0], ops[1])
	}
	out = faEmitPrefix(out, 0, false, sr.code, &faAddr{isReg: true, reg: dr.code}, sr.needRex || dr.needRex)
	out = append(out, op, 0xC0|byte(sr.code&7)<<3|byte(dr.code&7))
	return out, nil
}

func (st *faState) faNegq(out []byte, ops []string) ([]byte, error) {
	if len(ops) != 1 {
		return nil, fmt.Errorf("negq needs 1 operand")
	}
	dr, ok := faParseReg(ops[0])
	if !ok || dr.size != 8 {
		return nil, fmt.Errorf("bad negq operand %s", ops[0])
	}
	out = faEmitPrefix(out, 0, true, 0, &faAddr{isReg: true, reg: dr.code}, false)
	out = append(out, 0xF7, 0xD8|byte(dr.code&7))
	return out, nil
}

func (st *faState) faNotq(out []byte, ops []string) ([]byte, error) {
	if len(ops) != 1 {
		return nil, fmt.Errorf("notq needs 1 operand")
	}
	dr, ok := faParseReg(ops[0])
	if !ok || dr.size != 8 {
		return nil, fmt.Errorf("bad notq operand %s", ops[0])
	}
	out = faEmitPrefix(out, 0, true, 0, &faAddr{isReg: true, reg: dr.code}, false)
	out = append(out, 0xF7, 0xD0|byte(dr.code&7))
	return out, nil
}

func (st *faState) faImul(out []byte, ops []string) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("imulq needs 2 operands")
	}
	src, dst := ops[0], ops[1]
	dr, ok := faParseReg(dst)
	if !ok || dr.size != 8 {
		return nil, fmt.Errorf("bad imulq destination %s", dst)
	}
	if strings.HasPrefix(src, "$") {
		imm, err := faParseInt(src[1:])
		if err != nil {
			return nil, err
		}
		// REX.W + REX.R + REX.B (dst is both reg and rm).
		rex := byte(0x48)
		if dr.code >= 8 {
			rex |= 0x05
		}
		out = append(out, rex)
		if faFitsInt8(imm) {
			out = append(out, 0x6B, 0xC0|byte(dr.code&7)<<3|byte(dr.code&7), byte(int8(int64(imm))))
			return out, nil
		}
		out = append(out, 0x69, 0xC0|byte(dr.code&7)<<3|byte(dr.code&7))
		return faAppendU32(out, imm), nil
	}
	sr, ok := faParseReg(src)
	if !ok || sr.size != 8 {
		return nil, fmt.Errorf("bad imulq source %s", src)
	}
	out = faEmitPrefix(out, 0, true, dr.code, &faAddr{isReg: true, reg: sr.code}, false)
	out = append(out, 0x0F, 0xAF, 0xC0|byte(dr.code&7)<<3|byte(sr.code&7))
	return out, nil
}

func (st *faState) faDiv(out []byte, ops []string, w64, isIdiv bool) ([]byte, error) {
	if len(ops) != 1 {
		return nil, fmt.Errorf("div needs 1 operand")
	}
	r, ok := faParseReg(ops[0])
	if !ok {
		return nil, fmt.Errorf("bad div operand %s", ops[0])
	}
	if w64 && r.size != 8 {
		return nil, fmt.Errorf("bad div operand %s", ops[0])
	}
	if !w64 && r.size != 4 {
		return nil, fmt.Errorf("bad div operand %s", ops[0])
	}
	ext := byte(0xF0) // /6 div
	if isIdiv {
		ext = 0xF8 // /7 idiv
	}
	out = faEmitPrefix(out, 0, w64, 0, &faAddr{isReg: true, reg: r.code}, false)
	out = append(out, 0xF7, ext|byte(r.code&7))
	return out, nil
}

func (st *faState) faLea(out []byte, sec string, off uint64, ops []string, emitReloc bool) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("leaq needs 2 operands")
	}
	dr, ok := faParseReg(ops[1])
	if !ok || dr.size != 8 {
		return nil, fmt.Errorf("bad leaq destination %s", ops[1])
	}
	a, err := faParseAddr(ops[0])
	if err != nil {
		return nil, err
	}
	if a.isReg {
		return nil, fmt.Errorf("bad leaq source %s", ops[0])
	}
	out = faEmitPrefix(out, 0, true, dr.code, &a, false)
	out = append(out, 0x8D)
	dispPos := len(out)
	out = faEmitRM(out, dr.code, a)
	if a.rip {
		return st.faRipRef(out, sec, off, dispPos+1, a.ripSym, a.ripAdd, emitReloc)
	}
	return out, nil
}

func (st *faState) faCall(out []byte, sec string, off uint64, ops []string, emitReloc bool) ([]byte, error) {
	if len(ops) != 1 {
		return nil, fmt.Errorf("call needs 1 operand")
	}
	t := strings.TrimSpace(ops[0])
	if strings.HasPrefix(t, "*") {
		r, ok := faParseReg(t[1:])
		if !ok {
			return nil, fmt.Errorf("bad call target %s", t)
		}
		out = faEmitPrefix(out, 0, false, 0, &faAddr{isReg: true, reg: r.code}, false)
		out = append(out, 0xFF, 0xD0|byte(r.code&7))
		return out, nil
	}
	return st.faCallTarget(out, sec, off, t, emitReloc)
}

func faPushPop(out []byte, ops []string, push bool) ([]byte, error) {
	if len(ops) != 1 {
		return nil, fmt.Errorf("push/pop needs 1 operand")
	}
	r, ok := faParseReg(ops[0])
	if !ok || r.size != 8 {
		return nil, fmt.Errorf("bad push/pop operand %s", ops[0])
	}
	op := byte(0x50)
	if !push {
		op = 0x58
	}
	if r.code >= 8 {
		out = append(out, 0x41)
	}
	out = append(out, op+byte(r.code&7))
	return out, nil
}

func (st *faState) faShift(out []byte, mn string, ops []string) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("%s needs 2 operands", mn)
	}
	var ext byte
	switch mn {
	case "salq", "shlq":
		ext = 0xE0 // /4
	case "shrq":
		ext = 0xE8 // /5
	case "sarq":
		ext = 0xF8 // /7
	}
	dr, ok := faParseReg(ops[1])
	if !ok || dr.size != 8 {
		return nil, fmt.Errorf("bad %s destination %s", mn, ops[1])
	}
	src := strings.TrimSpace(ops[0])
	if strings.HasPrefix(src, "$") {
		imm, err := faParseInt(src[1:])
		if err != nil {
			return nil, err
		}
		out = faEmitPrefix(out, 0, true, 0, &faAddr{isReg: true, reg: dr.code}, false)
		if imm == 1 {
			out = append(out, 0xD1, ext|byte(dr.code&7))
			return out, nil
		}
		out = append(out, 0xC1, ext|byte(dr.code&7), byte(imm))
		return out, nil
	}
	if src == "%cl" {
		out = faEmitPrefix(out, 0, true, 0, &faAddr{isReg: true, reg: dr.code}, false)
		out = append(out, 0xD3, ext|byte(dr.code&7))
		return out, nil
	}
	return nil, fmt.Errorf("bad %s source %s", mn, ops[0])
}

// faSSE encodes xmm,xmm scalar-double/single ops with the given prefix.
func faSSE(out []byte, ops []string, legacy, op byte) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("sse op needs 2 operands")
	}
	s, ok := faParseXMM(ops[0])
	if !ok {
		return nil, fmt.Errorf("bad sse source %s", ops[0])
	}
	d, ok := faParseXMM(ops[1])
	if !ok {
		return nil, fmt.Errorf("bad sse destination %s", ops[1])
	}
	out = faEmitPrefix(out, legacy, false, d, &faAddr{isReg: true, reg: s}, false)
	out = append(out, 0x0F, op, 0xC0|byte(d&7)<<3|byte(s&7))
	return out, nil
}

func (st *faState) faCvtSS2SD(out []byte, sec string, off uint64, ops []string, emitReloc bool) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("cvtss2sd needs 2 operands")
	}
	d, ok := faParseXMM(ops[1])
	if !ok {
		return nil, fmt.Errorf("bad cvtss2sd destination %s", ops[1])
	}
	if s, ok := faParseXMM(ops[0]); ok {
		out = faEmitPrefix(out, 0xF3, false, d, &faAddr{isReg: true, reg: s}, false)
		out = append(out, 0x0F, 0x5A, 0xC0|byte(d&7)<<3|byte(s&7))
		return out, nil
	}
	a, err := faParseAddr(ops[0])
	if err != nil {
		return nil, err
	}
	if a.isReg {
		return nil, fmt.Errorf("bad cvtss2sd source %s", ops[0])
	}
	out = faEmitPrefix(out, 0xF3, false, d, &a, false)
	out = append(out, 0x0F, 0x5A)
	dispPos := len(out)
	out = faEmitRM(out, d, a)
	if a.rip {
		return st.faRipRef(out, sec, off, dispPos+1, a.ripSym, a.ripAdd, emitReloc)
	}
	return out, nil
}

func (st *faState) faCvtsi2sd(out []byte, ops []string, w64 bool) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("cvtsi2sd needs 2 operands")
	}
	sr, ok := faParseReg(ops[0])
	if !ok {
		return nil, fmt.Errorf("bad cvtsi2sd source %s", ops[0])
	}
	if w64 && sr.size != 8 {
		return nil, fmt.Errorf("bad cvtsi2sd source %s", ops[0])
	}
	if !w64 && sr.size != 4 {
		return nil, fmt.Errorf("bad cvtsi2sd source %s", ops[0])
	}
	d, ok := faParseXMM(ops[1])
	if !ok {
		return nil, fmt.Errorf("bad cvtsi2sd destination %s", ops[1])
	}
	out = faEmitPrefix(out, 0xF2, w64, d, &faAddr{isReg: true, reg: sr.code}, false)
	out = append(out, 0x0F, 0x2A, 0xC0|byte(d&7)<<3|byte(sr.code&7))
	return out, nil
}

func faCvttsd2si(out []byte, ops []string) ([]byte, error) {
	if len(ops) != 2 {
		return nil, fmt.Errorf("cvttsd2si needs 2 operands")
	}
	s, ok := faParseXMM(ops[0])
	if !ok {
		return nil, fmt.Errorf("bad cvttsd2si source %s", ops[0])
	}
	dr, ok := faParseReg(ops[1])
	if !ok || (dr.size != 4 && dr.size != 8) {
		return nil, fmt.Errorf("bad cvttsd2si destination %s", ops[1])
	}
	out = faEmitPrefix(out, 0xF2, dr.size == 8, s, &faAddr{isReg: true, reg: dr.code}, false)
	out = append(out, 0x0F, 0x2C, 0xC0|byte(s&7)<<3|byte(dr.code&7))
	return out, nil
}

func faSetcc(out []byte, ops []string, code byte) ([]byte, error) {
	if len(ops) != 1 {
		return nil, fmt.Errorf("setcc needs 1 operand")
	}
	r, ok := faParseReg(ops[0])
	if !ok || r.size != 1 {
		return nil, fmt.Errorf("bad setcc operand %s", ops[0])
	}
	if r.high8 && (r.code >= 8 || r.needRex) {
		return nil, fmt.Errorf("setcc cannot use %s with REX", ops[0])
	}
	if r.code >= 8 || r.needRex {
		out = append(out, 0x41)
	}
	out = append(out, 0x0F, code, 0xC0|byte(r.code&7))
	return out, nil
}
