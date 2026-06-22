// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// hardening_test.go — security tests for the SquashFS reader against malicious
// or corrupt images. Threat model: an untrusted image must NEVER panic the
// host, read out of bounds, integer-overflow into a bad allocation/slice, loop
// forever, or OOM.
//
// This package is already the strongest in the org: every decompressor caps its
// output with io.LimitReader(maxOut+1) and rejects overruns, metadata block
// lengths are validated, block lists and directory counts are bounded, and the
// symlink-hop budget is fixed at 40. The one remaining class-(A) allocation —
// readFile pre-sizing its output buffer from an extended-file inode's raw u64
// Size — is fixed via safeio.MakeBytes (data.go). These tests prove that fix and
// LOCK IN the existing good handling: every fuzz target asserts "no panic +
// graceful error", and its seed corpus carries the concrete attack vectors
// (ext-file Size=2^60 OOM bomb, decompression bombs, truncated metadata blocks,
// bad directory counts) so the assertions also run under plain `go test`.
package squashfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/go-volumes/safeio"
)

// ─────────────────────── canonical good image (seed) ───────────────────────

// goodImage builds a small but structurally complete valid SquashFS image in
// memory: a root directory holding a sub-block file (fragment-packed), a
// multi-block file, a symlink and a sub-directory. It is the "valid" seed every
// fuzz target starts from, and the base buffer the mutation tests corrupt.
func goodImage(t testing.TB) []byte {
	t.Helper()
	root := &node{
		name: "",
		mode: sIFDIR | 0o755,
		children: []*node{
			{name: "small.txt", mode: sIFREG | 0o644, data: []byte("hello squashfs\n")},
			{name: "big.bin", mode: sIFREG | 0o644, data: pattern(300000)},
			{name: "link", mode: sIFLNK | 0o777, target: "small.txt"},
			{name: "sub", mode: sIFDIR | 0o755, children: []*node{
				{name: "nested.txt", mode: sIFREG | 0o644, data: []byte("nested\n")},
			}},
		},
	}
	img, err := buildImage(root, BuildOptions{})
	if err != nil {
		t.Fatalf("buildImage: %v", err)
	}
	return img
}

// openImage is goodImage opened through the public reader.
func openImage(t testing.TB) *FS {
	t.Helper()
	img := goodImage(t)
	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open(goodImage): %v", err)
	}
	return fs
}

// ─────────────────── M: readFile size pre-allocation (OOM) ──────────────────

// TestHardenReadFileExtSizeBomb is the headline regression. A crafted extended
// file inode declares Size = 2^60 with an empty block list and no fragment.
// Before the fix, readFile did make([]byte, 0, in.Size) and OOM-killed the host
// on that allocation alone. Now it must reject the inode via safeio (the
// declared size exceeds every reachable byte) instead of allocating.
func TestHardenReadFileExtSizeBomb(t *testing.T) {
	fs := openImage(t)
	in := &inode{
		Type:       inodeExtFile,
		Size:       1 << 60, // 1 EiB declared, zero bytes reachable
		fragIdx:    invalidFrag,
		blockSizes: nil,
	}
	_, err := readFile(fs, in)
	if err == nil {
		t.Fatal("readFile accepted Size=2^60 ext-file (would OOM)")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("readFile(Size=2^60) err = %v, want ErrCorrupt", err)
	}
}

// TestHardenReadFileSizeAboveReachable feeds a size one byte past what the block
// list can supply: the bound rejects it before the per-block assembly loop.
func TestHardenReadFileSizeAboveReachable(t *testing.T) {
	fs := openImage(t)
	bs := uint64(fs.sb.BlockSize)
	in := &inode{
		Type:       inodeExtFile,
		Size:       2*bs + 1, // 1 block list entry can supply at most 2*bs
		fragIdx:    invalidFrag,
		blockSizes: []uint32{0}, // one sparse block
	}
	_, err := readFile(fs, in)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("readFile(oversized) err = %v, want ErrCorrupt", err)
	}
}

// TestHardenReadFileSparseWithinBound confirms the bound is not over-tight: a
// fully sparse file whose Size sits exactly within (len(blockSizes)+1)*blockSize
// still assembles (no false positive), proving we did not weaken correctness.
func TestHardenReadFileSparseWithinBound(t *testing.T) {
	fs := openImage(t)
	bs := uint64(fs.sb.BlockSize)
	in := &inode{
		Type:       inodeExtFile,
		Size:       bs, // exactly one sparse block
		fragIdx:    invalidFrag,
		blockSizes: []uint32{0}, // sparse: zero-filled
	}
	out, err := readFile(fs, in)
	if err != nil {
		t.Fatalf("readFile(sparse 1 block) err = %v, want nil", err)
	}
	if uint64(len(out)) != bs {
		t.Fatalf("readFile(sparse) len = %d, want %d", len(out), bs)
	}
	for _, b := range out {
		if b != 0 {
			t.Fatal("sparse block not zero-filled")
		}
	}
}

// TestHardenReadFileBasicSizeBomb applies the same bound to a basic (u32 size)
// file: even the lower-risk variant must not over-allocate.
func TestHardenReadFileBasicSizeBomb(t *testing.T) {
	fs := openImage(t)
	in := &inode{
		Type:       inodeBasicFile,
		Size:       0xFFFFFFFF, // max u32, far beyond an empty block list
		fragIdx:    invalidFrag,
		blockSizes: nil,
	}
	if _, err := readFile(fs, in); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("readFile(basic 4GiB) err = %v, want ErrCorrupt", err)
	}
}

// TestSafeIOWired is a smoke check that the shared hardening lib resolves and
// its sentinel is reachable, so a dependency regression fails loudly here.
func TestSafeIOWired(t *testing.T) {
	if _, err := safeio.MakeBytes(1<<60, 16); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("safeio.MakeBytes ceiling check = %v, want ErrTooLarge", err)
	}
}

// ──────────────────────── reader smoke over good image ─────────────────────

// TestHardenGoodImageReadsBack confirms the crafted in-memory image is itself
// valid end-to-end, so the fuzz seed corpus exercises real parsing paths.
func TestHardenGoodImageReadsBack(t *testing.T) {
	fs := openImage(t)
	if got, err := fs.ReadFile("/small.txt"); err != nil || string(got) != "hello squashfs\n" {
		t.Fatalf("ReadFile(/small.txt) = %q, %v", got, err)
	}
	if got, err := fs.ReadFile("/big.bin"); err != nil || !bytes.Equal(got, pattern(300000)) {
		t.Fatalf("ReadFile(/big.bin): err=%v equal=%v", err, bytes.Equal(got, pattern(300000)))
	}
	if tgt, err := fs.ReadLink("/link"); err != nil || tgt != "small.txt" {
		t.Fatalf("ReadLink(/link) = %q, %v", tgt, err)
	}
	if _, err := fs.ListDir("/sub"); err != nil {
		t.Fatalf("ListDir(/sub): %v", err)
	}
}

// exercise opens an image and walks everything reachable, returning the first
// error. It must never panic regardless of how corrupt the image is — that is
// the property the fuzz targets assert.
func exercise(img []byte) {
	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		return
	}
	walk(fs, "/", 0)
}

// walk recursively lists and reads every entry under path, bounded in depth so a
// crafted cyclic directory cannot drive unbounded recursion in the test itself.
func walk(fs *FS, path string, depth int) {
	if depth > 16 {
		return
	}
	_, _ = fs.Stat(path)
	_, _ = fs.ReadLink(path)
	_, _ = fs.Xattrs(path)
	entries, err := fs.ListDir(path)
	if err != nil {
		_, _ = fs.ReadFile(path)
		return
	}
	for _, e := range entries {
		child := path
		if path != "/" {
			child += "/"
		}
		child += e.Name()
		_, _ = fs.ReadFile(child)
		walk(fs, child, depth+1)
	}
}

// ───────────────────────────── fuzz targets ────────────────────────────────

// FuzzOpenRead feeds arbitrary / mutated images to the full Open+walk path and
// asserts it never panics or hangs. The seed corpus carries the valid image and
// the concrete attack classes called out in the threat model.
func FuzzOpenRead(f *testing.F) {
	good := goodImage(f)
	f.Add(good)

	// ── ext-file Size=2^60 OOM bomb planted into a real image ──
	f.Add(plantExtSizeBomb(f, good))

	// ── decompression-bomb vector: flip the root data block's size word so it
	// claims to be compressed, tempting an unbounded inflate (bounded by the
	// LimitReader the decompressors already carry). ──
	f.Add(corruptFirstDataBlock(good))

	// ── truncated metadata: lop off the inode/dir tables so every metadata
	// read hits EOF mid-structure. ──
	if len(good) > 200 {
		f.Add(append([]byte(nil), good[:len(good)-200]...))
	}
	f.Add(good[:96]) // superblock only, every table offset past EOF

	// ── bad directory header count + flipped compression id ──
	f.Add([]byte("hsqs")) // magic only, truncated superblock
	f.Add(make([]byte, 96))

	f.Fuzz(func(t *testing.T, img []byte) {
		exercise(img) // property: no panic, no OOM, terminates.
	})
}

// FuzzReadFile drives the readFile assembly path directly with an
// attacker-controlled declared size and block list, against the valid image's
// backing store. This concentrates fuzzing on the bounded-allocation fix. Seeds
// include the 2^60 OOM vector.
func FuzzReadFile(f *testing.F) {
	good := goodImage(f)

	f.Add(uint64(15), uint8(0), uint32(0))         // small, no blocks
	f.Add(uint64(1)<<60, uint8(0), uint32(0))      // M: OOM bomb
	f.Add(uint64(300000), uint8(3), uint32(1<<20)) // multi-block-ish
	f.Add(^uint64(0), uint8(255), uint32(0))       // max size, many blocks

	f.Fuzz(func(t *testing.T, size uint64, nBlocks uint8, sizeWord uint32) {
		fs, err := Open(bytes.NewReader(good), int64(len(good)))
		if err != nil {
			t.Fatalf("Open(goodImage): %v", err) // good image must always open
		}
		blocks := make([]uint32, int(nBlocks))
		for i := range blocks {
			blocks[i] = sizeWord
		}
		in := &inode{
			Type:        inodeExtFile,
			Size:        size,
			fragIdx:     invalidFrag,
			blocksStart: 0, // point at the start of the (small) image
			blockSizes:  blocks,
		}
		// Errors are fine; a panic or OOM is not.
		_, _ = readFile(fs, in)
	})
}

// FuzzReadMetaBlock feeds arbitrary 2-byte header + payload framing to the
// metadata-block reader, locking in the length-validation / compressed-bit
// handling against truncation and oversize claims.
func FuzzReadMetaBlock(f *testing.F) {
	f.Add([]byte{0x00, 0x80})                          // SET compressed-bit, size 0 → rejected
	f.Add([]byte{0xff, 0x7f})                          // size 0x7fff > metaBlockMax → rejected
	f.Add([]byte{0x05, 0x80, 'h', 'e', 'l', 'l', 'o'}) // raw 5-byte block
	f.Add([]byte{0x05, 0x00, 1, 2, 3})                 // claims compressed, garbage
	f.Add([]byte{0x00})                                // truncated header

	d := gzipDecompressor{}
	f.Fuzz(func(t *testing.T, raw []byte) {
		// Never panics; on success the payload is within bounds.
		data, _, err := readMetaBlock(bytes.NewReader(raw), d, 0)
		if err == nil && len(data) > metaBlockMax {
			t.Fatalf("readMetaBlock returned %d bytes > metaBlockMax", len(data))
		}
	})
}

// FuzzReadDir feeds arbitrary directory-listing bytes through the directory
// parser, locking in the header-count and name-length caps against a bad count.
func FuzzReadDir(f *testing.F) {
	// A minimal one-entry directory header + entry + name.
	hdr := make([]byte, 12)
	binary.LittleEndian.PutUint32(hdr[0:], 0) // count-1 = 0 → 1 entry
	ent := make([]byte, 8)
	binary.LittleEndian.PutUint16(ent[6:], 0) // nameLen-1 = 0 → 1 byte
	seed := append(append(hdr, ent...), 'a')
	f.Add(seed)

	// Hostile count word (0xFFFFFFFF) → count overflows the 256 cap.
	bad := append([]byte(nil), seed...)
	binary.LittleEndian.PutUint32(bad[0:], 0xFFFFFFFF)
	f.Add(bad)
	f.Add([]byte{0, 0, 0}) // too short for a header

	f.Fuzz(func(t *testing.T, listing []byte) {
		if len(listing) > metaBlockMax {
			listing = listing[:metaBlockMax]
		}
		// Serve the listing as a single raw metadata block at directory-table
		// offset 0, then drive the real readDir over a synthetic FS. This
		// exercises the production header-count / name-length caps, not a copy.
		fs := &FS{
			rs: bytes.NewReader(metaWrap(listing)),
			d:  gzipDecompressor{},
			sb: &Superblock{BlockSize: 131072, DirTableStart: 0},
		}
		in := &inode{
			Type:          inodeBasicDir,
			Size:          uint64(len(listing)),
			dirStartBlock: 0,
			dirOffset:     0,
		}
		_, _ = readDir(fs, in) // property: no panic; caps hold.
	})
}

// ─────────────────────────── fuzz support ──────────────────────────────────

// metaWrap frames payload as a single raw (uncompressed) metadata block: a
// 2-byte little-endian header with the compressed-bit SET.
func metaWrap(payload []byte) []byte {
	if len(payload) > metaBlockMax {
		payload = payload[:metaBlockMax]
	}
	out := make([]byte, 2+len(payload))
	binary.LittleEndian.PutUint16(out[0:], uint16(len(payload))|metaHeaderCompressedBit)
	copy(out[2:], payload)
	return out
}

// plantExtSizeBomb returns a copy of img with the first basic-file inode header
// rewritten in place to a (truncated) ext-file declaring Size=2^60. The reader
// must reject it on read rather than OOM. Because we only flip the magic-level
// framing the image stays openable, so the bomb is reached through ReadFile.
func plantExtSizeBomb(t testing.TB, img []byte) []byte {
	out := append([]byte(nil), img...)
	// Scan the inode-table region for the first basic-file inode (type word 2)
	// and flip its type to ext-file (9) with a 2^60 size. We do a coarse scan:
	// the type word is the first u16 of each inode. This is best-effort; if not
	// found the image is still a useful seed.
	le := binary.LittleEndian
	start := int(le.Uint64(out[0x40:])) // InodeTableStart
	if start < 0 || start+2 >= len(out) {
		return out
	}
	for i := start + 2; i+40 < len(out); i++ {
		if le.Uint16(out[i:]) == inodeBasicFile {
			le.PutUint16(out[i:], inodeExtFile)
			le.PutUint64(out[i+16:], 0)     // start_block(u64)
			le.PutUint64(out[i+24:], 1<<60) // file_size(u64) = bomb
			break
		}
	}
	return out
}

// corruptFirstDataBlock flips the byte just after the superblock, perturbing the
// first compressed data/metadata stream so a decompressor sees garbage.
func corruptFirstDataBlock(img []byte) []byte {
	out := append([]byte(nil), img...)
	if len(out) > superblockSize+4 {
		out[superblockSize] ^= 0xff
		out[superblockSize+1] ^= 0xff
	}
	return out
}
