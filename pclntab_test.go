package wzprof

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"testing"
)

func TestWasmFunctionStarts(t *testing.T) {
	bin, err := os.ReadFile("testdata/golang/simple.wasm")
	if err != nil {
		panic(err)
	}
	imports, code, _, name := wasmbinSections(bin)
	cm := buildCodemap(code, name, imports)

	// Format is multiple lines of:
	//   $offset func[$index] <$funcname>:
	// where offset is the offset in byte since the beginning of the wasm binary
	// in hexadecimal. Index may not start at 0.
	// For example:
	//   120886 func[1319] <fmt.__pp_.fmtInteger>:
	// Generated with:
	//   wasm-objdump -j Code  -d testdata/golang/simple.wasm |grep -E 'func\[' > testdata/golang/simple_addresses.txt
	type fnEntry struct {
		addr uint64
		idx  int
		name string
	}
	var expected []fnEntry
	{
		re := regexp.MustCompile(`^([0-9a-f]+) func\[([0-9]+)\] <(.+)>:`)

		f, err := os.Open("testdata/golang/simple_addresses.txt")
		if err != nil {
			panic(err)
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			match := re.FindStringSubmatch(scanner.Text())
			addr, err := strconv.ParseUint(match[1], 16, 64)
			if err != nil {
				panic(err)
			}
			idx, err := strconv.ParseUint(match[2], 10, 64)
			if err != nil {
				panic(err)
			}
			expected = append(expected, fnEntry{
				name: match[3],
				addr: addr,
				idx:  int(idx),
			})
		}
		if err := scanner.Err(); err != nil {
			panic(err)
		}
	}

	if len(cm.fnmaps) != len(expected) {
		t.Fatalf("code map has %d functions; expected %d", len(cm.fnmaps), len(expected))
	}

	for i, f := range cm.fnmaps {
		// fnmaps offsets are relative to the beginning of the Code section,
		// while the expected address is absolute in the binary.
		cs := f.Start + code.Offset
		if cs != expected[i].addr {
			t.Errorf("function %d: starts at %x; expected %x", i, cs, expected[i])
		}
		if f.Name != expected[i].name {
			t.Errorf("function %d: name is '%s'; expected '%s'", i, f.Name, expected[i].name)
		}
	}
}
