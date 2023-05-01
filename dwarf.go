package wzprof

import (
	"debug/dwarf"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/tetratelabs/wazero/api"
)

type pcrange = [2]uint64

type entryatrange struct {
	Range pcrange
	Entry *dwarf.Entry
}

// dwarfmapper is adapted from the wazero DWARFLines.Line
// implementation, with a more aggressive caching strategy.
type dwarfmapper struct {
	d *dwarf.Data
	// Potentially overlapping sequence of ranges sorted by start
	// pc that map to CompileUnit or inlined entries.
	compileUnits []entryatrange
	inlinedFuncs []entryatrange
}

func newDwarfmapper(sections []api.CustomSection) (mapper, error) {
	var info, line, ranges, str, abbrev []byte

	for _, section := range sections {
		switch section.Name() {
		case ".debug_info":
			info = section.Data()
		case ".debug_line":
			line = section.Data()
		case ".debug_str":
			str = section.Data()
		case ".debug_abbrev":
			abbrev = section.Data()
		case ".debug_ranges":
			ranges = section.Data()
		}
	}

	if info == nil {
		return nil, fmt.Errorf("dwarf: missing section: .debug_info")
	}
	if line == nil {
		return nil, fmt.Errorf("dwarf: missing section: .debug_line")
	}
	if str == nil {
		return nil, fmt.Errorf("dwarf: missing section: .debug_str")
	}
	if abbrev == nil {
		return nil, fmt.Errorf("dwarf: missing section: .debug_abbrev")
	}
	if ranges == nil {
		return nil, fmt.Errorf("dwarf: missing section: .debug_ranges")
	}

	d, _ := dwarf.New(abbrev, nil, nil, info, line, nil, ranges, str)

	r := d.Reader()

	compileUnits := []entryatrange{}
	inlinedFuncs := []entryatrange{}

	for {
		ent, err := r.Next()
		if err != nil || ent == nil {
			break
		}

		switch ent.Tag {
		case dwarf.TagCompileUnit:
		case dwarf.TagInlinedSubroutine:
		default:
			continue
		}

		ranges, err := d.Ranges(ent)
		if err != nil {
			fmt.Fprintf(os.Stderr, "profiler: dwarf: failed to read ranges: %s\n", err)
			continue
		}

		if ent.Tag == dwarf.TagCompileUnit {
			for _, pcr := range ranges {
				compileUnits = append(compileUnits, entryatrange{pcr, ent})
			}
		} else if ent.Tag == dwarf.TagInlinedSubroutine {
			for _, pcr := range ranges {
				inlinedFuncs = append(inlinedFuncs, entryatrange{pcr, ent})
			}
		} else {
			panic("bug")
		}
	}

	sort.Slice(compileUnits, func(i, j int) bool {
		return compileUnits[i].Range[0] < compileUnits[j].Range[0]
	})
	// Inlined functions are present in dwarf in order of
	// inlining. Functions inlined at a specific point will have
	// the same range, so it is important to preserve order, hence
	// the stable sort.
	sort.SliceStable(inlinedFuncs, func(i, j int) bool {
		return inlinedFuncs[i].Range[0] < inlinedFuncs[j].Range[0]
	})

	dm := &dwarfmapper{
		d: d,

		compileUnits: compileUnits,
		inlinedFuncs: inlinedFuncs,
	}

	return dm, nil
}

// Lookup returns a list of function locations for a given program
// counter, starting from current function followed by the inlined
// functions, in order of inlining. Result if empty if the pc cannot
// be resolved in the dwarf data.
func (d *dwarfmapper) Lookup(pc uint64) []location {
	// TODO: replace with binary search
	var cu *dwarf.Entry

	i := 0
	for i := range d.compileUnits {
		if d.compileUnits[i].Range[0] <= pc && pc <= d.compileUnits[i].Range[1] {
			cu = d.compileUnits[i].Entry
			break
		}
	}
	if cu == nil {
		// no compile unit contains this pc
		return nil
	}

	lr, err := d.d.LineReader(cu)
	if err != nil || lr == nil {
		fmt.Fprintf(os.Stderr, "profiler: dwarf: failed to read lines: %s\n", err)
		return nil
	}

	// TODO: cache this
	var lines []line
	var le dwarf.LineEntry
	for {
		pos := lr.Tell()
		err = lr.Next(&le)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "profiler: dwarf: failed to iterate on lines: %s\n", err)
			break
		}
		lines = append(lines, line{Pos: pos, Address: le.Address})

	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].Address < lines[j].Address })

	i = sort.Search(len(lines), func(i int) bool { return lines[i].Address >= pc })
	if i == len(lines) {
		// no line information for this pc.
		return nil
	}

	l := lines[i]
	if l.Address != pc {
		// https://github.com/stealthrocket/wazero/blob/867459d7d5ed988a55452d6317ff3cc8451b8ff0/internal/wasmdebug/dwarf.go#L141-L150
		// If the address doesn't match exactly, the previous
		// entry is the one that contains the instruction.
		// That can happen anytime as the DWARF spec allows
		// it, and other tools can handle it in this way
		// conventionally
		// https://github.com/gimli-rs/addr2line/blob/3a2dbaf84551a06a429f26e9c96071bb409b371f/src/lib.rs#L236-L242
		// https://github.com/kateinoigakukun/wasminspect/blob/f29f052f1b03104da9f702508ac0c1bbc3530ae4/crates/debugger/src/dwarf/mod.rs#L453-L459
		if i-1 < 0 {
			return nil
		}
		l = lines[i-1]
	}

	lr.Seek(l.Pos)
	err = lr.Next(&le)
	if err != nil {
		// l.Pos was created from parsing dwarf, should not
		// happen.
		panic("bug")
	}

	// now find inlined subprograms
	var inlinedEntries []entryatrange
	startIdx := sort.Search(len(d.inlinedFuncs), func(i int) bool {
		return d.inlinedFuncs[i].Range[0] <= pc && pc <= d.inlinedFuncs[i].Range[1]
	})
	if startIdx != len(d.inlinedFuncs) {
		endIdx := startIdx + 1
		for ; endIdx < len(d.inlinedFuncs); endIdx++ {
			if d.inlinedFuncs[endIdx].Range[0] <= pc && pc <= d.inlinedFuncs[endIdx].Range[1] {
				continue
			}
			break
		}
		inlinedEntries = d.inlinedFuncs[startIdx:endIdx]
	}

	locations := make([]location, 0, 1+len(inlinedEntries))

	locations = append(locations, location{
		File:    le.File.Name,
		Line:    int64(le.Line),
		Column:  int64(le.Column),
		Inlined: len(inlinedEntries) > 0,
		PC:      pc,
	})

	if len(inlinedEntries) > 0 {
		files := lr.Files()
		for i := len(inlinedEntries) - 1; i >= 0; i-- {
			f := inlinedEntries[i].Entry
			fileIdx, ok := f.Val(dwarf.AttrCallFile).(int64)
			if !ok || fileIdx >= int64(len(files)) {
				break
			}
			file := files[fileIdx]
			line, _ := f.Val(dwarf.AttrCallLine).(int64)
			col, _ := f.Val(dwarf.AttrCallLine).(int64)
			locations = append(locations, location{
				File:     file.Name,
				Line:     line,
				Column:   col,
				Inlined:  i != 0,
				PC:       pc,
				Function: d.stableNameForSubprogram(f),
			})
		}
	}

	return locations
}

// line is used to cache line entries for a given compilation unit.
type line struct {
	Pos     dwarf.LineReaderPos
	Address uint64
}

func (d *dwarfmapper) stableNameForSubprogram(e *dwarf.Entry) string {
	// If an inlined function, grab the name from the origin.
	var err error
	r := d.d.Reader()
	for {
		ao, ok := e.Val(dwarf.AttrAbstractOrigin).(dwarf.Offset)
		if !ok {
			break
		}
		r.Seek(ao)
		e, err = r.Next()
		if err != nil {
			// malformed dwarf
			break
		}
	}
	// Otherwise just return the name of the subprogram.
	name, _ := e.Val(dwarf.AttrLinkageName).(string)
	if name == "" {
		name, _ = e.Val(dwarf.AttrName).(string)
	}
	return name
}
