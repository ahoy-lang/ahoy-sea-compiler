package main

import (
	"fmt"
	"sync"
)

// Linker - Links object code and creates executable
type Linker struct {
	textSection   []byte
	rodataSection []byte
	dataSection   []byte
	bssSize       uint64
	
	symbols       map[string]LinkSymbol
	relocations   []Relocation
	
	entryPoint    string
	entryOffset   uint64
}

type LinkSymbol struct {
	Name    string
	Value   uint64
	Size    uint64
	Section string
	Binding byte
	Type    byte
}

const (
	STB_LOCAL  = 0
	STB_GLOBAL = 1
	STB_WEAK   = 2
)

const (
	STT_NOTYPE  = 0
	STT_OBJECT  = 1
	STT_FUNC    = 2
	STT_SECTION = 3
	STT_FILE    = 4
)

func NewLinker() *Linker {
	return &Linker{
		symbols:     make(map[string]LinkSymbol),
		relocations: make([]Relocation, 0),
		entryPoint:  "main",
	}
}

func (l *Linker) SetSections(text, rodata, data []byte, bssSize uint64) {
	l.textSection = text
	l.rodataSection = rodata
	l.dataSection = data
	l.bssSize = bssSize
}

func (l *Linker) AddSymbol(name string, value uint64, section string) {
	l.symbols[name] = LinkSymbol{
		Name:    name,
		Value:   value,
		Size:    0,
		Section: section,
		Binding: STB_GLOBAL,
		Type:    STT_FUNC,
	}
}

func (l *Linker) AddRelocation(rel Relocation) {
	l.relocations = append(l.relocations, rel)
}

func (l *Linker) Link() ([]byte, error) {
	// Resolve symbols
	err := l.resolveSymbols()
	if err != nil {
		return nil, err
	}
	
	// Debug: check text section size
	if false {  // Set to true to debug
		fmt.Printf("DEBUG: Text section size: %d bytes\n", len(l.textSection))
		fmt.Printf("DEBUG: First 32 bytes: % x\n", l.textSection[:min(32, len(l.textSection))])
	}
	
	// Apply relocations (in parallel for speed)
	err = l.applyRelocations()
	if err != nil {
		return nil, err
	}
	
	// Find entry point
	if entry, ok := l.symbols[l.entryPoint]; ok {
		l.entryOffset = entry.Value
	} else {
		return nil, fmt.Errorf("entry point '%s' not found", l.entryPoint)
	}
	
	// Generate ELF executable
	return l.generateExecutable()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (l *Linker) resolveSymbols() error {
	// Check for undefined symbols
	for _, rel := range l.relocations {
		if _, ok := l.symbols[rel.Symbol]; !ok {
			return fmt.Errorf("undefined symbol: %s", rel.Symbol)
		}
	}
	return nil
}

func (l *Linker) applyRelocations() error {
	// Use goroutines to process relocations in parallel
	// Split relocations into chunks
	numWorkers := 4
	chunkSize := (len(l.relocations) + numWorkers - 1) / numWorkers
	
	var wg sync.WaitGroup
	errChan := make(chan error, numWorkers)
	
	for i := 0; i < numWorkers; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(l.relocations) {
			end = len(l.relocations)
		}
		if start >= len(l.relocations) {
			break
		}
		
		wg.Add(1)
		go func(rels []Relocation) {
			defer wg.Done()
			
			for _, rel := range rels {
				err := l.applyRelocation(rel)
				if err != nil {
					errChan <- err
					return
				}
			}
		}(l.relocations[start:end])
	}
	
	// Wait for all workers
	wg.Wait()
	close(errChan)
	
	// Check for errors
	for err := range errChan {
		if err != nil {
			return err
		}
	}
	
	return nil
}

func (l *Linker) applyRelocation(rel Relocation) error {
	sym, ok := l.symbols[rel.Symbol]
	if !ok {
		return fmt.Errorf("undefined symbol in relocation: %s", rel.Symbol)
	}
	
	var target []byte
	switch rel.Type {
	case R_X86_64_PC32:
		// PC-relative 32-bit
		targetAddr := sym.Value
		pcAddr := rel.Offset + 4
		offset := int32(int64(targetAddr) - int64(pcAddr) + rel.Addend)
		
		target = l.textSection
		if int(rel.Offset)+4 > len(target) {
			return fmt.Errorf("relocation offset out of bounds")
		}
		
		// Write 32-bit offset (little-endian)
		target[rel.Offset] = byte(offset)
		target[rel.Offset+1] = byte(offset >> 8)
		target[rel.Offset+2] = byte(offset >> 16)
		target[rel.Offset+3] = byte(offset >> 24)
		
	case R_X86_64_64:
		// Absolute 64-bit
		targetAddr := sym.Value + uint64(rel.Addend)
		
		target = l.textSection
		if int(rel.Offset)+8 > len(target) {
			return fmt.Errorf("relocation offset out of bounds")
		}
		
		// Write 64-bit address (little-endian)
		for i := 0; i < 8; i++ {
			target[rel.Offset+uint64(i)] = byte(targetAddr >> (i * 8))
		}
		
	default:
		return fmt.Errorf("unsupported relocation type: %d", rel.Type)
	}
	
	return nil
}

func (l *Linker) generateExecutable() ([]byte, error) {
	// Create ELF generator
	elfGen := NewELFGenerator()
	
	// Set sections
	elfGen.SetCode(l.textSection, l.rodataSection, l.dataSection, l.bssSize)
	
	// Add symbols to ELF (in parallel)
	symbolSlice := make([]LinkSymbol, 0, len(l.symbols))
	for _, sym := range l.symbols {
		symbolSlice = append(symbolSlice, sym)
	}
	
	// Process symbols in parallel
	var wg sync.WaitGroup
	numWorkers := 4
	chunkSize := (len(symbolSlice) + numWorkers - 1) / numWorkers
	
	// Use a channel-free approach - just add to elfGen sequentially after parallel prep
	type symbolData struct {
		name    string
		value   uint64
		size    uint64
		section uint16
		binding byte
		symType byte
	}
	
	symbolDataChan := make(chan symbolData, len(symbolSlice))
	
	for i := 0; i < numWorkers; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(symbolSlice) {
			end = len(symbolSlice)
		}
		if start >= len(symbolSlice) {
			break
		}
		
		wg.Add(1)
		go func(syms []LinkSymbol) {
			defer wg.Done()
			
			for _, sym := range syms {
				var sectionIdx uint16 = 1
				switch sym.Section {
				case "text":
					sectionIdx = 1
				case "rodata":
					sectionIdx = 2
				case "data":
					sectionIdx = 3
				case "bss":
					sectionIdx = 4
				}
				
				symbolDataChan <- symbolData{
					name:    sym.Name,
					value:   sym.Value,
					size:    sym.Size,
					section: sectionIdx,
					binding: sym.Binding,
					symType: sym.Type,
				}
			}
		}(symbolSlice[start:end])
	}
	
	// Wait for workers
	go func() {
		wg.Wait()
		close(symbolDataChan)
	}()
	
	// Add symbols sequentially from channel
	for sd := range symbolDataChan {
		elfGen.AddSymbol(sd.name, sd.value, sd.size, sd.section, sd.binding, sd.symType)
	}
	
	// Generate ELF file
	return elfGen.Generate(l.entryOffset)
}

func (l *Linker) SetEntryPoint(name string) {
	l.entryPoint = name
}
