// Package winres owns the Windows application manifest embedded in
// no-mistakes.exe and renders the COFF resource objects that carry it.
//
// The manifest's consoleAllocationPolicy=detached setting is what keeps the
// managed daemon console-less when Task Scheduler starts it (issue #1653):
// Windows 11 24H2 / Server 2025 (build 26100) and later then skip allocating a
// console for the console-subsystem binary unless it inherits one from its
// parent, so a terminal, git hook, or agent still shares the binary's console.
// Older builds ignore the setting.
//
// Go's linker has no manifest support; it links any .syso object in a
// package directory and merges .rsrc sections into the executable's resource
// directory. cmd/genwinres writes Object's output for each Targets entry to
// cmd/no-mistakes, so `go build`, `go install`, `make dist`, and the release
// workflow embed the manifest without extra steps, and `make lint` fails if a
// committed object drifts from this package.
package winres

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"path"
)

//go:embed no-mistakes.exe.manifest
var manifest []byte

// Manifest returns a copy of the application manifest that every Windows
// build of no-mistakes.exe embeds.
func Manifest() []byte {
	return bytes.Clone(manifest)
}

// Resource identifiers of the one resource the object carries. Windows reads a
// process's manifest from RT_MANIFEST resource ID 1
// (CREATEPROCESS_MANIFEST_RESOURCE_ID).
const (
	TypeManifest   = 24     // RT_MANIFEST
	ManifestID     = 1      // CREATEPROCESS_MANIFEST_RESOURCE_ID
	ManifestLangID = 0x0409 // en-US
)

// Target is one Windows architecture no-mistakes ships for.
type Target struct {
	GOARCH string
	// Machine is the COFF IMAGE_FILE_MACHINE_* value.
	Machine uint16
	// RelocADDR32NB is the architecture's image-relative 32-bit relocation
	// type, which resolves the data entry's RVA at link time.
	RelocADDR32NB uint16
}

// Targets lists the architectures `make dist` builds for Windows.
var Targets = []Target{
	{GOARCH: "amd64", Machine: 0x8664, RelocADDR32NB: 0x0003}, // IMAGE_FILE_MACHINE_AMD64, IMAGE_REL_AMD64_ADDR32NB
	{GOARCH: "arm64", Machine: 0xaa64, RelocADDR32NB: 0x0002}, // IMAGE_FILE_MACHINE_ARM64, IMAGE_REL_ARM64_ADDR32NB
}

// ObjectDir is the package directory, relative to the repository root, whose
// build links the objects.
const ObjectDir = "cmd/no-mistakes"

// ObjectPath returns the object's slash-separated path relative to the
// repository root. The _windows_<arch> suffix is a Go build constraint, so
// each build links exactly its own architecture's object.
func (t Target) ObjectPath() string {
	return path.Join(ObjectDir, "rsrc_windows_"+t.GOARCH+".syso")
}

const (
	fileHeaderSize    = 20
	sectionHeaderSize = 40
	relocationSize    = 10
	directorySize     = 16
	directoryEntry    = 8
	dataEntrySize     = 16

	// The resource tree is three single-entry directories (type, name,
	// language) followed by the leaf's data entry and the manifest bytes.
	// The high bit of a directory entry's offset marks a subdirectory.
	subdirectory  = 0x80000000
	nameDirOffset = directorySize + directoryEntry
	langDirOffset = 2 * (directorySize + directoryEntry)
	// DataEntryOffset is the .rsrc offset of the data entry whose RVA field
	// the object's only relocation patches.
	DataEntryOffset = 3 * (directorySize + directoryEntry)
	manifestOffset  = DataEntryOffset + dataEntrySize

	sectionCharacteristics = 0x40000040 // IMAGE_SCN_CNT_INITIALIZED_DATA | IMAGE_SCN_MEM_READ
	symbolClassStatic      = 3          // IMAGE_SYM_CLASS_STATIC
)

// Object renders the COFF object that embeds the manifest for t: one .rsrc
// section holding a single RT_MANIFEST resource (ID 1, language 0x0409), an
// ADDR32NB relocation that turns the data entry's section offset into an RVA,
// and the .rsrc section symbol that relocation targets. The output depends
// only on the manifest and t: the timestamp is zero and nothing is read from
// the host, so a rerun reproduces the committed bytes exactly.
func (t Target) Object() []byte {
	le := binary.LittleEndian

	var rsrc []byte
	directory := func(id, offset uint32) {
		rsrc = le.AppendUint32(rsrc, 0) // Characteristics
		rsrc = le.AppendUint32(rsrc, 0) // TimeDateStamp
		rsrc = le.AppendUint16(rsrc, 0) // MajorVersion
		rsrc = le.AppendUint16(rsrc, 0) // MinorVersion
		rsrc = le.AppendUint16(rsrc, 0) // NumberOfNamedEntries
		rsrc = le.AppendUint16(rsrc, 1) // NumberOfIdEntries
		rsrc = le.AppendUint32(rsrc, id)
		rsrc = le.AppendUint32(rsrc, offset)
	}
	directory(TypeManifest, subdirectory|nameDirOffset)
	directory(ManifestID, subdirectory|langDirOffset)
	directory(ManifestLangID, DataEntryOffset)
	// The RVA field holds the manifest's section offset as the relocation's
	// addend; the linker adds the final .rsrc section's virtual address.
	rsrc = le.AppendUint32(rsrc, manifestOffset)
	rsrc = le.AppendUint32(rsrc, uint32(len(manifest)))
	rsrc = le.AppendUint32(rsrc, 0) // CodePage
	rsrc = le.AppendUint32(rsrc, 0) // Reserved
	rsrc = append(rsrc, manifest...)
	for len(rsrc)%8 != 0 {
		rsrc = append(rsrc, 0)
	}

	rawDataOffset := uint32(fileHeaderSize + sectionHeaderSize)
	relocationsOffset := rawDataOffset + uint32(len(rsrc))
	symbolTableOffset := relocationsOffset + relocationSize

	var out []byte
	// COFF file header.
	out = le.AppendUint16(out, t.Machine)
	out = le.AppendUint16(out, 1) // NumberOfSections
	out = le.AppendUint32(out, 0) // TimeDateStamp
	out = le.AppendUint32(out, symbolTableOffset)
	out = le.AppendUint32(out, 1) // NumberOfSymbols
	out = le.AppendUint16(out, 0) // SizeOfOptionalHeader
	out = le.AppendUint16(out, 0) // Characteristics
	// .rsrc section header.
	out = append(out, ".rsrc\x00\x00\x00"...)
	out = le.AppendUint32(out, 0) // VirtualSize
	out = le.AppendUint32(out, 0) // VirtualAddress
	out = le.AppendUint32(out, uint32(len(rsrc)))
	out = le.AppendUint32(out, rawDataOffset)
	out = le.AppendUint32(out, relocationsOffset)
	out = le.AppendUint32(out, 0) // PointerToLinenumbers
	out = le.AppendUint16(out, 1) // NumberOfRelocations
	out = le.AppendUint16(out, 0) // NumberOfLinenumbers
	out = le.AppendUint32(out, sectionCharacteristics)
	out = append(out, rsrc...)
	// The data entry's RVA, relative to symbol 0 (the .rsrc section).
	out = le.AppendUint32(out, DataEntryOffset)
	out = le.AppendUint32(out, 0) // SymbolTableIndex
	out = le.AppendUint16(out, t.RelocADDR32NB)
	// Symbol 0: the .rsrc section symbol.
	out = append(out, ".rsrc\x00\x00\x00"...)
	out = le.AppendUint32(out, 0) // Value
	out = le.AppendUint16(out, 1) // SectionNumber
	out = le.AppendUint16(out, 0) // Type
	out = append(out, symbolClassStatic, 0)
	// Empty string table: just its own 4-byte size.
	out = le.AppendUint32(out, 4)
	return out
}
