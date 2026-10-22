// elf_object: writes an *ObjectFile as an ELF64 relocatable (.o) object.
//
// Layout mirrors GNU as output (section order, flags, symbol ordering) so
// the result links cleanly with GNU ld. Relocations that reference a
// section (static-local RIP references and `.quad . + 8`) use the section
// symbol; all others reference the target symbol directly.
package main

import (
	"fmt"
	"os"
)

// faSymExtra carries symbol attributes that have no field in ObjSymbol.
type faSymExtra struct {
	isCommon bool
	align    uint64
	typ      string // "func", "object", ""
}

var faExtraTable = map[*ObjectFile]map[string]faSymExtra{}

func faExtra(o *ObjectFile, name string) faSymExtra {
	if m := faExtraTable[o]; m != nil {
		return m[name]
	}
	return faSymExtra{}
}

// faElfRelocType translates the package's internal relocation enum
// (assembler.go) to true ELF64 relocation numbers. The internal values
// coincide with ELF for NONE/64/PC32, but internal PLT32 is 3 (ELF GOT32)
// and internal GOTPCREL is 4 (ELF GOTPCREL is 9).
func faElfRelocType(t RelocationType) (uint32, error) {
	switch t {
	case R_X86_64_NONE:
		return 0, nil
	case R_X86_64_64:
		return 1, nil
	case R_X86_64_PC32:
		return 2, nil
	case R_X86_64_PLT32:
		return 4, nil
	case R_X86_64_GOTPCREL:
		return 9, nil
	}
	return 0, fmt.Errorf("unsupported relocation type %d", int(t))
}

const (
	faSHTNull     = 0
	faSHTProgbits = 1
	faSHTSymtab   = 2
	faSHTStrtab   = 3
	faSHTRela     = 4
	faSHTNobits   = 8

	faSHFWrite    = 0x1
	faSHFAlloc    = 0x2
	faSHFExec     = 0x4
	faSHFInfoLink = 0x40

	faSTBLocal   = 0
	faSTBGlobal  = 1
	faSTTNotype  = 0
	faSTTObject  = 1
	faSTTFunc    = 2
	faSTTSection = 3

	faSHNUndef  = 0
	faSHNCommon = 0xFFF2
)

type faSection struct {
	name   string
	typ    uint32
	flags  uint64
	data   []byte
	nobits uint64 // size when NOBITS
	align  uint64
	link   uint32
	info   uint32
	entsz  uint64
	off    uint64
}

// WriteELF writes the object file as an ELF64 relocatable object.
func (o *ObjectFile) WriteELF(path string) error {
	// Partition relocs by section.
	relBySec := map[string][]ObjReloc{}
	for _, r := range o.Relocs {
		relBySec[r.Section] = append(relBySec[r.Section], r)
	}

	// Section symbols needed for section-relative relocations.
	needSecSym := map[string]bool{}
	isSecName := func(n string) bool {
		return n == ".text" || n == ".rodata" || n == ".data" || n == ".bss"
	}
	for _, r := range o.Relocs {
		if isSecName(r.Symbol) {
			needSecSym[r.Symbol] = true
		}
	}

	// Symbol table entries.
	type faSymOut struct {
		name  string
		bind  int
		typ   int
		shndx int // -1 => resolve later; faSHNUndef/faSHNCommon special
		value uint64
		size  uint64
		sec   string
	}
	var syms []faSymOut
	symIdx := map[string]int{"": 0}
	addSym := func(s faSymOut) int {
		idx := len(syms) + 1
		syms = append(syms, s)
		return idx
	}

	// Section index placeholder: assigned below; section syms reference
	// section names, resolved after layout.
	secOrder := []string{".text", ".rodata", ".data", ".bss"}
	for _, sn := range secOrder {
		if needSecSym[sn] {
			idx := addSym(faSymOut{name: sn, bind: faSTBLocal, typ: faSTTSection, sec: sn})
			symIdx[sn] = idx
		}
	}
	nLocals := 0
	for _, s := range o.Symbols {
		ex := faExtra(o, s.Name)
		if ex.isCommon {
			continue
		}
		if s.Section == "" {
			continue // undefined; handled with globals
		}
		if s.Global {
			continue
		}
		t := faSTTNotype
		if ex.typ == "func" {
			t = faSTTFunc
		} else if ex.typ == "object" {
			t = faSTTObject
		}
		idx := addSym(faSymOut{name: s.Name, bind: faSTBLocal, typ: t, sec: s.Section, value: s.Offset, size: s.Size})
		symIdx[s.Name] = idx
		nLocals++
	}
	firstGlobal := len(syms) + 1
	for _, s := range o.Symbols {
		ex := faExtra(o, s.Name)
		if ex.isCommon {
			al := ex.align
			if al == 0 {
				al = 1
			}
			idx := addSym(faSymOut{name: s.Name, bind: faSTBGlobal, typ: faSTTObject, shndx: faSHNCommon, value: al, size: s.Size})
			symIdx[s.Name] = idx
			continue
		}
		if s.Section != "" && s.Global {
			t := faSTTNotype
			if ex.typ == "func" {
				t = faSTTFunc
			} else if ex.typ == "object" {
				t = faSTTObject
			}
			idx := addSym(faSymOut{name: s.Name, bind: faSTBGlobal, typ: t, sec: s.Section, value: s.Offset, size: s.Size})
			symIdx[s.Name] = idx
			continue
		}
		if s.Section != "" {
			continue // local, already emitted
		}
		idx := addSym(faSymOut{name: s.Name, bind: faSTBGlobal, typ: faSTTNotype, shndx: faSHNUndef})
		symIdx[s.Name] = idx
	}

	// Build sections.
	var secs []faSection
	secs = append(secs, faSection{name: ".text", typ: faSHTProgbits, flags: faSHFAlloc | faSHFExec, data: o.Text, align: 16})
	if len(relBySec[".text"]) > 0 {
		secs = append(secs, faSection{name: ".rela.text", typ: faSHTRela, flags: faSHFInfoLink, align: 8, entsz: 24})
	}
	secs = append(secs, faSection{name: ".data", typ: faSHTProgbits, flags: faSHFAlloc | faSHFWrite, data: o.Data, align: 8})
	secs = append(secs, faSection{name: ".bss", typ: faSHTNobits, flags: faSHFAlloc | faSHFWrite, nobits: o.BSS, align: 8})
	secs = append(secs, faSection{name: ".rodata", typ: faSHTProgbits, flags: faSHFAlloc, data: o.Rodata, align: 8})
	if len(relBySec[".rodata"]) > 0 {
		secs = append(secs, faSection{name: ".rela.rodata", typ: faSHTRela, flags: faSHFInfoLink, align: 8, entsz: 24})
	}
	if len(relBySec[".data"]) > 0 {
		secs = append(secs, faSection{name: ".rela.data", typ: faSHTRela, flags: faSHFInfoLink, align: 8, entsz: 24})
	}
	secs = append(secs, faSection{name: ".symtab", typ: faSHTSymtab, align: 8, entsz: 24})
	secs = append(secs, faSection{name: ".strtab", typ: faSHTStrtab, align: 1})
	secs = append(secs, faSection{name: ".shstrtab", typ: faSHTStrtab, align: 1})

	secIdx := map[string]int{}
	for i, s := range secs {
		secIdx[s.name] = i + 1
	}

	// Resolve section references in symbols.
	for i := range syms {
		if syms[i].sec != "" {
			syms[i].shndx = secIdx[syms[i].sec]
			syms[i].sec = ""
		} else if syms[i].shndx == 0 && syms[i].name != "" && syms[i].typ == faSTTSection {
			syms[i].shndx = secIdx[syms[i].name]
		}
	}

	// .strtab contents.
	strtab := []byte{0}
	strOff := map[string]uint32{}
	for _, s := range syms {
		if _, ok := strOff[s.name]; !ok {
			strOff[s.name] = uint32(len(strtab))
			strtab = append(strtab, s.name...)
			strtab = append(strtab, 0)
		}
	}

	// .symtab contents.
	symtab := make([]byte, 0, (len(syms)+1)*24)
	symtab = append(symtab, make([]byte, 24)...)
	for _, s := range syms {
		var e [24]byte
		off, _ := strOff[s.name]
		e[0] = byte(off)
		e[1] = byte(off >> 8)
		e[2] = byte(off >> 16)
		e[3] = byte(off >> 24)
		e[4] = byte(s.bind<<4 | s.typ)
		e[5] = 0
		e[6] = byte(s.shndx)
		e[7] = byte(s.shndx >> 8)
		putU64(e[8:], s.value)
		putU64(e[16:], s.size)
		symtab = append(symtab, e[:]...)
	}

	// .rela contents.
	relaData := map[string][]byte{}
	for _, sc := range secs {
		if len(sc.name) > 6 && sc.name[:6] == ".rela." {
			target := sc.name[5:]
			var buf []byte
			for _, r := range relBySec[target] {
				si, ok := symIdx[r.Symbol]
				if !ok {
					return fmt.Errorf("relocation against unknown symbol %q", r.Symbol)
				}
				var e [24]byte
				et, err := faElfRelocType(r.Type)
				if err != nil {
					return err
				}
				putU64(e[0:], r.Offset)
				putU64(e[8:], uint64(si)<<32|uint64(et))
				putU64(e[16:], uint64(int64(r.Addend)))
				buf = append(buf, e[:]...)
			}
			relaData[sc.name] = buf
		}
	}

	// .shstrtab contents.
	shstrtab := []byte{0}
	shOff := map[string]uint32{}
	for _, sc := range secs {
		shOff[sc.name] = uint32(len(shstrtab))
		shstrtab = append(shstrtab, sc.name...)
		shstrtab = append(shstrtab, 0)
	}

	// Attach data and link/info.
	symtabIdx := secIdx[".symtab"]
	strtabIdx := secIdx[".strtab"]
	for i := range secs {
		switch secs[i].name {
		case ".symtab":
			secs[i].data = symtab
			secs[i].link = uint32(strtabIdx)
			secs[i].info = uint32(firstGlobal)
		case ".strtab":
			secs[i].data = strtab
		case ".shstrtab":
			secs[i].data = shstrtab
		default:
			if d, ok := relaData[secs[i].name]; ok {
				secs[i].data = d
				secs[i].link = uint32(symtabIdx)
				secs[i].info = uint32(secIdx[secs[i].name[5:]])
			}
		}
	}

	// Layout file offsets.
	off := uint64(64)
	for i := range secs {
		al := secs[i].align
		if al == 0 {
			al = 1
		}
		off = (off + al - 1) / al * al
		secs[i].off = off
		if secs[i].typ != faSHTNobits {
			off += uint64(len(secs[i].data))
		}
	}
	shoff := (off + 7) / 8 * 8

	// Emit.
	out := make([]byte, 0, shoff+uint64((len(secs)+1)*64))
	var hdr [64]byte
	hdr[0] = 0x7F
	hdr[1] = 'E'
	hdr[2] = 'L'
	hdr[3] = 'F'
	hdr[4] = 2
	hdr[5] = 1
	hdr[6] = 1
	hdr[16] = 1  // ET_REL
	hdr[18] = 62 // EM_X86_64
	hdr[20] = 1  // version
	putU64(hdr[40:], shoff)
	hdr[52] = 64
	hdr[58] = 64
	putU16(hdr[60:], uint16(len(secs)+1))
	putU16(hdr[62:], uint16(secIdx[".shstrtab"]))
	out = append(out, hdr[:]...)
	for _, sc := range secs {
		for uint64(len(out)) < sc.off {
			out = append(out, 0)
		}
		if sc.typ != faSHTNobits {
			out = append(out, sc.data...)
		}
	}
	for uint64(len(out)) < shoff {
		out = append(out, 0)
	}
	out = append(out, make([]byte, 64)...) // NULL section
	for _, sc := range secs {
		var sh [64]byte
		putU32(sh[0:], shOff[sc.name])
		putU32(sh[4:], sc.typ)
		putU64(sh[8:], sc.flags)
		putU64(sh[24:], sc.off)
		if sc.typ == faSHTNobits {
			putU64(sh[32:], sc.nobits)
		} else {
			putU64(sh[32:], uint64(len(sc.data)))
		}
		putU32(sh[40:], sc.link)
		putU32(sh[44:], sc.info)
		putU64(sh[48:], sc.align)
		putU64(sh[56:], sc.entsz)
		out = append(out, sh[:]...)
	}
	return os.WriteFile(path, out, 0644)
}

func putU16(b []byte, v uint16) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func putU64(b []byte, v uint64) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
	b[4] = byte(v >> 32)
	b[5] = byte(v >> 40)
	b[6] = byte(v >> 48)
	b[7] = byte(v >> 56)
}
