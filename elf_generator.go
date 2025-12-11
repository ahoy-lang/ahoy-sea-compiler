package main

import (
	"bytes"
	"encoding/binary"
)

// ELF Generator - Creates ELF64 executable files
type ELFGenerator struct {
	header         ELF64Header
	programHeaders []ELF64ProgramHeader
	sections       []ELF64Section
	symbolTable    []ELF64Symbol
	stringTable    []byte
	shstrtab       []byte
	
	textData       []byte
	rodataData     []byte
	dataData       []byte
	bssSize        uint64
}

// ELF64 Header
type ELF64Header struct {
	Magic      [4]byte  // 0x7f, 'E', 'L', 'F'
	Class      byte     // 1=32-bit, 2=64-bit
	Data       byte     // 1=little endian, 2=big endian
	Version    byte     // 1
	OSABI      byte     // 0=System V
	ABIVersion byte     // 0
	Pad        [7]byte  // padding
	Type       uint16   // 2=executable, 3=shared
	Machine    uint16   // 0x3E=x86-64
	Version2   uint32   // 1
	Entry      uint64   // entry point address
	PhOff      uint64   // program header offset
	ShOff      uint64   // section header offset
	Flags      uint32   // 0
	EhSize     uint16   // size of this header (64)
	PhEntSize  uint16   // size of program header (56)
	PhNum      uint16   // number of program headers
	ShEntSize  uint16   // size of section header (64)
	ShNum      uint16   // number of section headers
	ShStrNdx   uint16   // section header string table index
}

// Program Header (for loadable segments)
type ELF64ProgramHeader struct {
	Type   uint32 // 1=PT_LOAD
	Flags  uint32 // 1=X, 2=W, 4=R
	Offset uint64 // file offset
	VAddr  uint64 // virtual address
	PAddr  uint64 // physical address
	FileSz uint64 // size in file
	MemSz  uint64 // size in memory
	Align  uint64 // alignment
}

// Section Header
type ELF64Section struct {
	Name      uint32 // offset in shstrtab
	Type      uint32 // 0=NULL, 1=PROGBITS, 8=NOBITS, 2=SYMTAB, 3=STRTAB
	Flags     uint64 // 1=WRITE, 2=ALLOC, 4=EXECINSTR
	Addr      uint64 // virtual address
	Offset    uint64 // file offset
	Size      uint64 // size in bytes
	Link      uint32 // link to another section
	Info      uint32 // additional info
	AddrAlign uint64 // alignment
	EntSize   uint64 // entry size for tables
}

// Symbol Table Entry
type ELF64Symbol struct {
	Name  uint32 // offset in strtab
	Info  byte   // type and binding
	Other byte   // visibility
	Shndx uint16 // section index
	Value uint64 // symbol value
	Size  uint64 // symbol size
}

// Section indices
const (
	SHN_UNDEF = 0
	SHN_ABS   = 0xFFF1
)

// Section types
const (
	SHT_NULL     = 0
	SHT_PROGBITS = 1
	SHT_SYMTAB   = 2
	SHT_STRTAB   = 3
	SHT_NOBITS   = 8
)

// Section flags
const (
	SHF_WRITE     = 0x1
	SHF_ALLOC     = 0x2
	SHF_EXECINSTR = 0x4
)

// Program header types
const (
	PT_NULL = 0
	PT_LOAD = 1
)

// Program header flags
const (
	PF_X = 0x1
	PF_W = 0x2
	PF_R = 0x4
)

func NewELFGenerator() *ELFGenerator {
	return &ELFGenerator{
		sections:    make([]ELF64Section, 0),
		symbolTable: make([]ELF64Symbol, 0),
		stringTable: []byte{0},
		shstrtab:    []byte{0},
	}
}

func (e *ELFGenerator) SetCode(textData, rodataData, dataData []byte, bssSize uint64) {
	e.textData = textData
	e.rodataData = rodataData
	e.dataData = dataData
	e.bssSize = bssSize
}

func (e *ELFGenerator) AddSymbol(name string, value uint64, size uint64, section uint16, binding byte, symType byte) {
	nameOffset := e.addString(name)
	
	info := (binding << 4) | (symType & 0x0F)
	
	sym := ELF64Symbol{
		Name:  nameOffset,
		Info:  info,
		Other: 0,
		Shndx: section,
		Value: value,
		Size:  size,
	}
	
	e.symbolTable = append(e.symbolTable, sym)
}

func (e *ELFGenerator) Generate(entryPoint uint64) ([]byte, error) {
	buf := new(bytes.Buffer)
	
	// Base address for executable
	baseAddr := uint64(0x400000)
	
	// Calculate number of program headers needed
	numPH := 2 // text + data always
	if len(e.rodataData) > 0 {
		numPH = 3
	}
	
	// Calculate offsets
	headerSize := uint64(64)                              // ELF header
	phOffset := headerSize                                 // program headers start after ELF header
	phSize := uint64(56 * numPH)                          // program headers
	
	textOffset := phOffset + phSize
	textAddr := baseAddr + textOffset
	textSize := uint64(len(e.textData))
	
	rodataOffset := textOffset + textSize
	rodataAddr := baseAddr + rodataOffset
	rodataSize := uint64(len(e.rodataData))
	
	dataOffset := rodataOffset + rodataSize
	dataAddr := baseAddr + dataOffset
	dataSize := uint64(len(e.dataData))
	
	bssAddr := dataAddr + dataSize
	
	// Build section headers first to know how many we have
	e.buildSections(textOffset, textSize, textAddr,
		rodataOffset, rodataSize, rodataAddr,
		dataOffset, dataSize, dataAddr,
		bssAddr, e.bssSize)
	
	// Calculate actual data end
	actualDataEnd := dataOffset + dataSize
	
	// Build string tables
	symtabData := e.buildSymbolTable()
	strtabData := e.stringTable
	
	symtabOffset := actualDataEnd
	strtabOffset := symtabOffset + uint64(len(symtabData))
	shstrtabOffset := strtabOffset + uint64(len(strtabData))
	
	// Add symbol table, string table, and section name string table sections
	e.addSymtabSection(symtabOffset, uint64(len(symtabData)))
	e.addStrtabSection(strtabOffset, uint64(len(strtabData)))
	e.addShstrtabSection(shstrtabOffset, uint64(len(e.shstrtab)))
	
	shHeadersOffset := shstrtabOffset + uint64(len(e.shstrtab))
	
	// Create ELF header
	e.header = ELF64Header{
		Magic:      [4]byte{0x7F, 'E', 'L', 'F'},
		Class:      2,
		Data:       1,
		Version:    1,
		OSABI:      0,
		ABIVersion: 0,
		Type:       2,
		Machine:    0x3E,
		Version2:   1,
		Entry:      textAddr + entryPoint,
		PhOff:      phOffset,
		ShOff:      shHeadersOffset,
		Flags:      0,
		EhSize:     64,
		PhEntSize:  56,
		PhNum:      uint16(numPH),
		ShEntSize:  64,
		ShNum:      uint16(len(e.sections)),
		ShStrNdx:   uint16(len(e.sections) - 1),
	}
	
	// Write ELF header
	binary.Write(buf, binary.LittleEndian, &e.header)
	
	// Write program headers
	e.writeProgramHeaders(buf, textOffset, textSize, textAddr,
		rodataOffset, rodataSize, rodataAddr,
		dataOffset, dataSize, dataAddr, bssAddr, e.bssSize)
	
	// Write .text section
	buf.Write(e.textData)
	
	// Write .rodata section
	if len(e.rodataData) > 0 {
		buf.Write(e.rodataData)
	}
	
	// Write .data section
	if len(e.dataData) > 0 {
		buf.Write(e.dataData)
	}
	
	// Write symbol table
	buf.Write(symtabData)
	
	// Write string table
	buf.Write(strtabData)
	
	// Write section header string table
	buf.Write(e.shstrtab)
	
	// Write section headers
	for _, section := range e.sections {
		binary.Write(buf, binary.LittleEndian, &section)
	}
	
	return buf.Bytes(), nil
}

func (e *ELFGenerator) buildSections(textOff, textSize, textAddr,
	rodataOff, rodataSize, rodataAddr,
	dataOff, dataSize, dataAddr,
	bssAddr, bssSize uint64) {
	
	e.sections = make([]ELF64Section, 0)
	
	// NULL section (required)
	e.sections = append(e.sections, ELF64Section{})
	
	// .text section
	e.sections = append(e.sections, ELF64Section{
		Name:      e.addShString(".text"),
		Type:      SHT_PROGBITS,
		Flags:     SHF_ALLOC | SHF_EXECINSTR,
		Addr:      textAddr,
		Offset:    textOff,
		Size:      textSize,
		Link:      0,
		Info:      0,
		AddrAlign: 16,
		EntSize:   0,
	})
	
	// .rodata section
	if rodataSize > 0 {
		e.sections = append(e.sections, ELF64Section{
			Name:      e.addShString(".rodata"),
			Type:      SHT_PROGBITS,
			Flags:     SHF_ALLOC,
			Addr:      rodataAddr,
			Offset:    rodataOff,
			Size:      rodataSize,
			Link:      0,
			Info:      0,
			AddrAlign: 8,
			EntSize:   0,
		})
	}
	
	// .data section
	if dataSize > 0 {
		e.sections = append(e.sections, ELF64Section{
			Name:      e.addShString(".data"),
			Type:      SHT_PROGBITS,
			Flags:     SHF_WRITE | SHF_ALLOC,
			Addr:      dataAddr,
			Offset:    dataOff,
			Size:      dataSize,
			Link:      0,
			Info:      0,
			AddrAlign: 8,
			EntSize:   0,
		})
	}
	
	// .bss section
	if bssSize > 0 {
		e.sections = append(e.sections, ELF64Section{
			Name:      e.addShString(".bss"),
			Type:      SHT_NOBITS,
			Flags:     SHF_WRITE | SHF_ALLOC,
			Addr:      bssAddr,
			Offset:    0,
			Size:      bssSize,
			Link:      0,
			Info:      0,
			AddrAlign: 8,
			EntSize:   0,
		})
	}
}

func (e *ELFGenerator) addSymtabSection(offset, size uint64) {
	e.sections = append(e.sections, ELF64Section{
		Name:      e.addShString(".symtab"),
		Type:      SHT_SYMTAB,
		Flags:     0,
		Addr:      0,
		Offset:    offset,
		Size:      size,
		Link:      uint32(len(e.sections) + 1),
		Info:      1,
		AddrAlign: 8,
		EntSize:   24,
	})
}

func (e *ELFGenerator) addStrtabSection(offset, size uint64) {
	e.sections = append(e.sections, ELF64Section{
		Name:      e.addShString(".strtab"),
		Type:      SHT_STRTAB,
		Flags:     0,
		Addr:      0,
		Offset:    offset,
		Size:      size,
		Link:      0,
		Info:      0,
		AddrAlign: 1,
		EntSize:   0,
	})
}

func (e *ELFGenerator) addShstrtabSection(offset, size uint64) {
	e.sections = append(e.sections, ELF64Section{
		Name:      e.addShString(".shstrtab"),
		Type:      SHT_STRTAB,
		Flags:     0,
		Addr:      0,
		Offset:    offset,
		Size:      size,
		Link:      0,
		Info:      0,
		AddrAlign: 1,
		EntSize:   0,
	})
}

func (e *ELFGenerator) writeProgramHeaders(buf *bytes.Buffer,
	textOff, textSize, textAddr,
	rodataOff, rodataSize, rodataAddr,
	dataOff, dataSize, dataAddr, bssAddr, bssSize uint64) {
	
	// Text segment (executable + readable)
	textPH := ELF64ProgramHeader{
		Type:   PT_LOAD,
		Flags:  PF_R | PF_X,
		Offset: 0,
		VAddr:  0x400000,
		PAddr:  0x400000,
		FileSz: textOff + textSize,
		MemSz:  textOff + textSize,
		Align:  0x1000,
	}
	binary.Write(buf, binary.LittleEndian, &textPH)
	
	// Rodata segment (readable) - only if we have rodata
	if rodataSize > 0 {
		rodataPH := ELF64ProgramHeader{
			Type:   PT_LOAD,
			Flags:  PF_R,
			Offset: rodataOff,
			VAddr:  rodataAddr,
			PAddr:  rodataAddr,
			FileSz: rodataSize,
			MemSz:  rodataSize,
			Align:  0x1000,
		}
		binary.Write(buf, binary.LittleEndian, &rodataPH)
	}
	
	// Data+BSS segment (readable + writable)
	dataPH := ELF64ProgramHeader{
		Type:   PT_LOAD,
		Flags:  PF_R | PF_W,
		Offset: dataOff,
		VAddr:  dataAddr,
		PAddr:  dataAddr,
		FileSz: dataSize,
		MemSz:  dataSize + bssSize,
		Align:  0x1000,
	}
	binary.Write(buf, binary.LittleEndian, &dataPH)
}

func (e *ELFGenerator) buildSymbolTable() []byte {
	buf := new(bytes.Buffer)
	
	// First symbol is always null
	nullSym := ELF64Symbol{}
	binary.Write(buf, binary.LittleEndian, &nullSym)
	
	// Write actual symbols
	for _, sym := range e.symbolTable {
		binary.Write(buf, binary.LittleEndian, &sym)
	}
	
	return buf.Bytes()
}

func (e *ELFGenerator) addString(s string) uint32 {
	offset := uint32(len(e.stringTable))
	e.stringTable = append(e.stringTable, []byte(s)...)
	e.stringTable = append(e.stringTable, 0)
	return offset
}

func (e *ELFGenerator) addShString(s string) uint32 {
	offset := uint32(len(e.shstrtab))
	e.shstrtab = append(e.shstrtab, []byte(s)...)
	e.shstrtab = append(e.shstrtab, 0)
	return offset
}
