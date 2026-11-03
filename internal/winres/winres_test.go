package winres

import (
	"bytes"
	"context"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var buildDir string

func TestMain(m *testing.M) {
	// Helper mode for the console probe's control run: a console-subsystem
	// process without the manifest that follows the probe's protocol.
	if os.Getenv("NM_WINRES_TEST_HELPER") == "ready" {
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "nm-winres-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create build dir: %v\n", err)
		os.Exit(1)
	}
	buildDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type resource struct {
	typ, id, lang uint32
	data          []byte
}

// readResources walks the three-level resource tree (type, name, language)
// that starts at rva inside the PE or COFF section sect and returns every
// leaf. Directory entry offsets are relative to the tree's start; a data
// entry's OffsetToData is an RVA, which in an unlinked object is the
// section-relative addend of its ADDR32NB relocation.
func readResources(t *testing.T, sect *pe.Section, rva uint32) []resource {
	t.Helper()
	raw, err := sect.Data()
	if err != nil {
		t.Fatalf("read %s: %v", sect.Name, err)
	}
	tree := raw[rva-sect.VirtualAddress:]
	u16 := func(off uint32) uint32 { return uint32(binary.LittleEndian.Uint16(tree[off:])) }
	u32 := func(off uint32) uint32 { return binary.LittleEndian.Uint32(tree[off:]) }

	var out []resource
	var walk func(off uint32, path []uint32)
	walk = func(off uint32, path []uint32) {
		if named := u16(off + 12); named != 0 {
			t.Fatalf("resource directory at %#x has %d named entries; want ID entries only", off, named)
		}
		for i := uint32(0); i < u16(off+14); i++ {
			entry := off + 16 + 8*i
			id, target := u32(entry), u32(entry+4)
			if target&0x80000000 != 0 {
				walk(target&^0x80000000, append(path, id))
				continue
			}
			if len(path) != 2 {
				t.Fatalf("resource leaf at depth %d; want type/name/language", len(path)+1)
			}
			dataRVA, size := u32(target), u32(target+4)
			start := dataRVA - sect.VirtualAddress
			out = append(out, resource{typ: path[0], id: path[1], lang: id, data: raw[start : start+size]})
		}
	}
	walk(0, nil)
	return out
}

func assertOnlyManifest(t *testing.T, resources []resource) {
	t.Helper()
	if len(resources) != 1 {
		t.Fatalf("resources = %d, want exactly the manifest", len(resources))
	}
	got := resources[0]
	if got.typ != TypeManifest || got.id != ManifestID || got.lang != ManifestLangID {
		t.Fatalf("resource = type %d, id %d, lang %#x; want RT_MANIFEST (%d), id %d, lang %#x",
			got.typ, got.id, got.lang, TypeManifest, ManifestID, ManifestLangID)
	}
	if !bytes.Equal(got.data, Manifest()) {
		t.Fatalf("resource bytes differ from the manifest:\n%s", got.data)
	}
}

func TestObjectCarriesOnlyTheManifest(t *testing.T) {
	for _, target := range Targets {
		t.Run(target.GOARCH, func(t *testing.T) {
			f, err := pe.NewFile(bytes.NewReader(target.Object()))
			if err != nil {
				t.Fatalf("parse object: %v", err)
			}
			if f.Machine != target.Machine {
				t.Fatalf("machine = %#x, want %#x", f.Machine, target.Machine)
			}
			if f.TimeDateStamp != 0 {
				t.Fatalf("timestamp = %d, want 0 for a reproducible object", f.TimeDateStamp)
			}
			if len(f.Sections) != 1 || f.Sections[0].Name != ".rsrc" {
				t.Fatalf("sections = %v, want exactly .rsrc", f.Sections)
			}
			sect := f.Sections[0]
			if sect.Characteristics != pe.IMAGE_SCN_CNT_INITIALIZED_DATA|pe.IMAGE_SCN_MEM_READ {
				t.Fatalf("section characteristics = %#x, want initialized read-only data", sect.Characteristics)
			}
			assertOnlyManifest(t, readResources(t, sect, 0))

			if len(sect.Relocs) != 1 {
				t.Fatalf("relocations = %d, want 1", len(sect.Relocs))
			}
			reloc := sect.Relocs[0]
			if reloc.VirtualAddress != DataEntryOffset || reloc.Type != target.RelocADDR32NB || reloc.SymbolTableIndex != 0 {
				t.Fatalf("relocation = %+v, want ADDR32NB (%#x) of the data entry at %#x against symbol 0",
					reloc, target.RelocADDR32NB, DataEntryOffset)
			}
			if len(f.COFFSymbols) != 1 {
				t.Fatalf("symbols = %d, want only the section symbol", len(f.COFFSymbols))
			}
			sym := f.COFFSymbols[0]
			name, err := sym.FullName(f.StringTable)
			if err != nil {
				t.Fatal(err)
			}
			if name != ".rsrc" || sym.SectionNumber != 1 || sym.StorageClass != symbolClassStatic || sym.Value != 0 {
				t.Fatalf("symbol = %s %+v, want the static .rsrc section symbol", name, sym)
			}
		})
	}
}

// The committed objects are binary blobs that every Windows build links, so a
// plain `go test` run must catch drift too, not only `make lint`. They were
// rendered in another process at another time, so this test (with
// `genwinres --check`) is also the determinism oracle for the generator.
func TestCommittedObjectsMatchGenerator(t *testing.T) {
	for _, target := range Targets {
		got, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(target.ObjectPath())))
		if err != nil {
			t.Fatalf("read committed object: %v", err)
		}
		if !bytes.Equal(got, target.Object()) {
			t.Fatalf("%s is stale; run `go run ./cmd/genwinres` and commit the result", target.ObjectPath())
		}
	}
}

func TestNoMistakesExecutableCarriesExactlyTheManifest(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-builds no-mistakes.exe for every Windows target")
	}
	for _, target := range Targets {
		t.Run(target.GOARCH, func(t *testing.T) {
			f, err := pe.Open(buildNoMistakes(t, target.GOARCH))
			if err != nil {
				t.Fatalf("open executable: %v", err)
			}
			defer f.Close()
			header, ok := f.OptionalHeader.(*pe.OptionalHeader64)
			if !ok {
				t.Fatalf("optional header = %T, want PE32+", f.OptionalHeader)
			}
			rva := header.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_RESOURCE].VirtualAddress
			if rva == 0 {
				t.Fatal("executable has no resource directory")
			}
			var rsrc *pe.Section
			for _, sect := range f.Sections {
				if rva >= sect.VirtualAddress && rva < sect.VirtualAddress+sect.VirtualSize {
					rsrc = sect
				}
			}
			if rsrc == nil {
				t.Fatalf("no section contains the resource directory at %#x", rva)
			}
			assertOnlyManifest(t, readResources(t, rsrc, rva))
		})
	}
}

var (
	buildMu    sync.Mutex
	buildCache = map[string]string{}
)

// buildNoMistakes builds cmd/no-mistakes for windows/goarch once per test
// process, exactly as `make dist` does apart from version stamping.
func buildNoMistakes(t *testing.T, goarch string) string {
	t.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	if exe, ok := buildCache[goarch]; ok {
		return exe
	}
	exe := filepath.Join(buildDir, "no-mistakes-"+goarch+".exe")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", exe, "github.com/kunchenguid/no-mistakes/cmd/no-mistakes")
	cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH="+goarch, "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build windows/%s: %v\n%s", goarch, err, out)
	}
	buildCache[goarch] = exe
	return exe
}
