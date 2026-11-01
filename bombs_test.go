// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// bombs_test.go — memory-bomb regression tests. Every input-driven allocation
// is bounded BEFORE it happens: each road below is a few bytes of hostile image
// that used to reserve megabytes to gigabytes, and each test asserts the road is
// refused with runtime.MemStats.TotalAlloc growing by less than a small budget.
// The "before" figures quoted in the comments were measured on v0.4.0.
package squashfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// allocated returns the bytes the runtime allocated while f ran.
func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// refused asserts that f ran with a TotalAlloc growth under budget bytes.
func refused(t *testing.T, what string, budget uint64, f func()) {
	t.Helper()
	if n := allocated(f); n >= budget {
		t.Errorf("%s allocated %d bytes, want < %d", what, n, budget)
	}
}

const miB = 1 << 20

// metaStream frames payload as consecutive raw (stored) metadata blocks of at
// most metaBlockMax bytes, the way an inode or directory table is laid out.
func metaStream(payload []byte) []byte {
	var out []byte
	for len(payload) > 0 {
		n := min(len(payload), metaBlockMax)
		out = append(out, metaWrap(payload[:n])...)
		payload = payload[n:]
	}
	return out
}

// metaFS is a synthetic FS whose inode table, directory table and xattr kv
// region all start at offset 0 of the given metadata stream.
func metaFS(payload []byte) *FS {
	img := metaStream(payload)
	return &FS{
		rs:    bytes.NewReader(img),
		size:  int64(len(img)),
		d:     gzipDecompressor{},
		sb:    &Superblock{BlockSize: 131072},
		cache: newBlockCache(),
	}
}

// ───────────────────────────── superblock ──────────────────────────────

// block_size sizes every decompression buffer (lz4, lzo and zstd allocate
// maxOut up front) and every data-cache slot. v0.4.0 accepted any power of two
// up to 2^31: lz4Decompressor{}.decompress(1 byte, 1<<30) allocated 1 GiB.
// linux fs/squashfs/super.c refuses block_size > 1 MiB and block_log > 20.
func TestBombSuperblockBlockSize(t *testing.T) {
	good := goodImage(t)
	patch := func(log uint16) []byte {
		img := append([]byte(nil), good...)
		binary.LittleEndian.PutUint32(img[0x0C:], uint32(1)<<(log&31))
		binary.LittleEndian.PutUint16(img[0x16:], log)
		return img
	}
	for _, log := range []uint16{12, 17, 20} {
		if _, err := readSuperblock(bytes.NewReader(patch(log))); err != nil {
			t.Errorf("block_log %d refused: %v", log, err)
		}
	}
	for _, log := range []uint16{21, 24, 30, 31} {
		if _, err := readSuperblock(bytes.NewReader(patch(log))); !errors.Is(err, ErrCorrupt) {
			t.Errorf("block_log %d accepted (err = %v), want ErrCorrupt", log, err)
		}
	}
	// block_log beyond the width of the shift must not be evaluated at all.
	img := patch(17)
	binary.LittleEndian.PutUint16(img[0x16:], 0xFFFF)
	if _, err := readSuperblock(bytes.NewReader(img)); !errors.Is(err, ErrCorrupt) {
		t.Errorf("block_log 0xFFFF: err = %v, want ErrCorrupt", err)
	}
}

// ───────────────────────────── data blocks ─────────────────────────────

// A 24-bit on-disk length can name 16 MiB. v0.4.0 only compared it with the
// block size for STORED blocks, so a compressed block word of 0xFFFFFF cost
// 16 MiB (measured 16 778 840 bytes) before the read failed.
func TestBombReadBlockLengthChecked(t *testing.T) {
	img := make([]byte, 1<<18)
	fs := &FS{rs: bytes.NewReader(img), size: int64(len(img)), d: gzipDecompressor{},
		sb: &Superblock{BlockSize: 1 << 20}, cache: newBlockCache()}

	refused(t, "compressed word 0xFFFFFF", 64<<10, func() {
		if _, err := fs.readBlock(0, 0xFFFFFF); !errors.Is(err, ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
		}
	})
	refused(t, "length one over block size", 64<<10, func() {
		if _, err := fs.readBlock(0, 1<<20+1); !errors.Is(err, ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
		}
	})
	// A length inside the block size but running past the end of the image is
	// refused before the buffer is made.
	refused(t, "extent past the image", 64<<10, func() {
		if _, err := fs.readBlock(int64(len(img))-10, 100000|dataUncompressedBit); !errors.Is(err, ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
		}
	})
	// Exactness: a stored block ending exactly at the end of the image reads.
	fs.sb.BlockSize = 1 << 18
	if b, err := fs.readBlock(0, 1<<18|dataUncompressedBit); err != nil || len(b) != 1<<18 {
		t.Errorf("stored block of exactly block_size at offset 0: len %d, err %v", len(b), err)
	}
	if _, err := fs.readBlock(1, 1<<18|dataUncompressedBit); !errors.Is(err, ErrCorrupt) {
		t.Errorf("stored block one byte past the end: err = %v, want ErrCorrupt", err)
	}
}

// ───────────────────────────── block list ──────────────────────────────

func extFileInode(size uint64, extra int) []byte {
	p := make([]byte, 16+40+extra)
	binary.LittleEndian.PutUint16(p[0:], inodeExtFile)
	binary.LittleEndian.PutUint64(p[16+8:], size)
	binary.LittleEndian.PutUint32(p[16+28:], invalidFrag)
	return p
}

// An extended file inode declaring 2^41 bytes at 128 KiB blocks claims 2^24
// block-list entries; v0.4.0 allocated the 64 MiB list (measured 67 110 600
// bytes) before reading the first one, from an inode with no list at all.
func TestBombBlockListFollowsMetadata(t *testing.T) {
	fs := metaFS(extFileInode(1<<41, 0))
	refused(t, "2^24-entry block list with no entries", 1<<20, func() {
		if _, err := readInode(fs, 0); err == nil {
			t.Error("readInode accepted a block list the metadata cannot supply")
		}
	})

	// A truthful list is read whole and in order, across a chunk boundary and a
	// metadata block boundary.
	const n = 3000
	p := extFileInode(uint64(n)*131072, 4*n)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(p[56+4*i:], uint32(i+1))
	}
	in, err := readInode(metaFS(p), 0)
	if err != nil {
		t.Fatalf("readInode(%d blocks): %v", n, err)
	}
	if len(in.blockSizes) != n {
		t.Fatalf("len(blockSizes) = %d, want %d", len(in.blockSizes), n)
	}
	for i, w := range in.blockSizes {
		if w != uint32(i+1) {
			t.Fatalf("blockSizes[%d] = %d, want %d", i, w, i+1)
		}
	}
}

// ───────────────────────────── ReadFile ceiling ────────────────────────

// A sparse block costs 4 bytes of (compressed) block list and yields a whole
// block of zeros, so the image size says nothing about the file size: the
// ceiling is the only bound. Exact: at the limit passes, one over is refused
// before anything is allocated.
func TestBombReadFileCeiling(t *testing.T) {
	fs := openImage(t)
	const blocks = 64
	bs := uint64(fs.sb.BlockSize)
	in := &inode{Type: inodeExtFile, Size: blocks * bs, fragIdx: invalidFrag, blockSizes: make([]uint32, blocks)}

	if out, err := readFileLimit(fs, in, int64(blocks*bs)); err != nil || uint64(len(out)) != blocks*bs {
		t.Fatalf("at the ceiling: len %d, err %v", len(out), err)
	}
	refused(t, "one byte over the ceiling", 64<<10, func() {
		if _, err := readFileLimit(fs, in, int64(blocks*bs)-1); !errors.Is(err, ErrTooLarge) {
			t.Errorf("err = %v, want ErrTooLarge", err)
		}
	})

	// The built-in default: 1 GiB + 1 of sparse blocks (8193 four-byte entries).
	big := &inode{Type: inodeExtFile, Size: uint64(DefaultMaxReadFile) + 1, fragIdx: invalidFrag,
		blockSizes: make([]uint32, uint64(DefaultMaxReadFile)/bs+1)}
	refused(t, "default ceiling", 64<<10, func() {
		if _, err := readFile(fs, big); !errors.Is(err, ErrTooLarge) {
			t.Errorf("err = %v, want ErrTooLarge", err)
		}
	})
	// A negative ceiling removes it (checked on a small file: the point is the
	// branch, not a gigabyte).
	if _, err := readFileLimit(fs, in, -1); err != nil {
		t.Errorf("no ceiling: %v", err)
	}
}

// A block list whose stored blocks lie outside the image is refused before the
// output buffer is made. v0.4.0 allocated the declared size first (64 MiB here).
func TestBombReadFileExtentsChecked(t *testing.T) {
	fs := openImage(t)
	bs := uint64(fs.sb.BlockSize)
	const blocks = 512 // 64 MiB at 128 KiB
	words := make([]uint32, blocks)
	for i := range words {
		words[i] = 100 | dataUncompressedBit
	}
	in := &inode{Type: inodeExtFile, Size: blocks * bs, fragIdx: invalidFrag, blockSizes: words,
		blocksStart: uint64(fs.size) - 50} // the first block already runs past the end
	refused(t, "stored blocks outside the image", 1<<20, func() {
		if _, err := readFile(fs, in); !errors.Is(err, ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
		}
	})
}

func TestReadFileLimitPublic(t *testing.T) {
	fs := openImage(t)
	if b, err := fs.ReadFileLimit("/big.bin", 300000); err != nil || len(b) != 300000 {
		t.Fatalf("limit == size: len %d, err %v", len(b), err)
	}
	if _, err := fs.ReadFileLimit("/big.bin", 299999); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("limit == size-1: err = %v, want ErrTooLarge", err)
	}
	if b, err := fs.ReadFileLimit("/big.bin", -1); err != nil || len(b) != 300000 {
		t.Fatalf("no limit: len %d, err %v", len(b), err)
	}
	if b, err := fs.ReadFile("/big.bin"); err != nil || len(b) != 300000 {
		t.Fatalf("ReadFile: len %d, err %v", len(b), err)
	}
}

// ───────────────────────────── directories ─────────────────────────────

func dirListing(n int) []byte {
	var p []byte
	for done := 0; done < n; {
		k := min(n-done, 256)
		h := make([]byte, 12)
		binary.LittleEndian.PutUint32(h, uint32(k-1))
		p = append(p, h...)
		for i := 0; i < k; i++ {
			e := make([]byte, 8) // offset, delta, type, name_size-1 = 0
			p = append(p, e...)
			p = append(p, 'a')
		}
		done += k
	}
	return p
}

// A directory's size is a u32 and its listing is compressed metadata, so a few
// KiB of image can list millions of entries, each parsed into a dirEntry
// (string header, reference, number, type: ~50 bytes) plus a name allocation.
// Exact: maxDirEntries entries list, one more is refused.
func TestBombDirEntriesCeiling(t *testing.T) {
	old := maxDirEntries
	maxDirEntries = 300 // spans two directory headers (256 + 44)
	t.Cleanup(func() { maxDirEntries = old })

	for _, tc := range []struct {
		n    int
		want error
	}{{300, nil}, {301, ErrTooLarge}} {
		p := dirListing(tc.n)
		fs := metaFS(p)
		in := &inode{Type: inodeBasicDir, Size: uint64(len(p))}
		entries, err := readDir(fs, in)
		if !errors.Is(err, tc.want) {
			t.Errorf("%d entries: err = %v, want %v", tc.n, err, tc.want)
		}
		if tc.want == nil && len(entries) != tc.n {
			t.Errorf("%d entries: got %d", tc.n, len(entries))
		}
	}
}

// The size is read as int64 so that a u32 directory size cannot go negative on
// a 32-bit target and make a huge directory look empty. On any target a size
// near 2^32 with a short listing must end with an error, not loop or panic.
func TestDirHugeSizeTerminates(t *testing.T) {
	p := dirListing(3)
	fs := metaFS(p)
	in := &inode{Type: inodeExtDir, Size: 0xFFFFFFF0}
	if _, err := readDir(fs, in); err == nil {
		t.Error("readDir returned no error for a 4 GiB directory over a 3-entry listing")
	}
}

// ───────────────────────────── xattrs ──────────────────────────────────

// The id table header declares idCount ids; v0.4.0 read and converted the whole
// pointer index (idCount/512 u64s, twice) up front: idCount = 2^31 measured
// 33 555 640 bytes, and 0xFFFFFE00 is 128 MiB. The pointers are now read one at
// a time, and the index must fit in the image (linux fs/squashfs/xattr_id.c).
func TestBombXattrIndexLazy(t *testing.T) {
	hdr := make([]byte, 16)
	binary.LittleEndian.PutUint32(hdr[8:], 0xFFFFFE00)
	fs := &FS{rs: bytes.NewReader(hdr), size: int64(len(hdr)), d: gzipDecompressor{},
		sb: &Superblock{BlockSize: 131072}, cache: newBlockCache()}
	refused(t, "index larger than the image", 64<<10, func() {
		if _, err := fs.readXattrTable(); !errors.Is(err, ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
		}
	})
	// With the image size unknown the table is accepted (nothing is read yet),
	// and a lookup fails cleanly on the missing pointer.
	fs.size = -1
	refused(t, "unknown image size", 64<<10, func() {
		tbl, err := fs.readXattrTable()
		if err != nil {
			t.Fatalf("readXattrTable: %v", err)
		}
		if _, err := fs.readXattrIDEntry(tbl, 0x7FFFFFFF); err == nil {
			t.Error("lookup of an id whose pointer lies outside the image succeeded")
		}
	})
}

func xattrPair(name string, vlen int) []byte {
	p := []byte{0, 0, byte(len(name)), 0}
	p = append(p, name...)
	p = binary.LittleEndian.AppendUint32(p, uint32(vlen))
	return append(p, bytes.Repeat([]byte{'v'}, vlen)...)
}

// e.count sized make(map, count): count = 2^26 allocated 6 447 735 400 bytes
// (6.4 GB) and a count near 2^32 aborts the process. The map now grows with
// the pairs that arrive, and the list is charged name + value + 8 bytes per
// pair against maxXattrBytes. Exact: the pair that makes the sum exceed it is
// the one refused.
func TestBombXattrPairs(t *testing.T) {
	fs := metaFS([]byte{0, 0, 0, 0})
	refused(t, "count = 2^26 over a truncated list", 1<<20, func() {
		if _, err := fs.readXattrPairs(&xattrTable{}, xattrIDEntry{count: 1 << 26}); err == nil {
			t.Error("accepted a list the metadata cannot supply")
		}
	})

	old := maxXattrBytes
	t.Cleanup(func() { maxXattrBytes = old })
	var list []byte
	for _, name := range []string{"a", "b", "c", "d"} {
		list = append(list, xattrPair(name, 10)...) // 8 + len("user.a") + 10 = 24 each
	}
	fs = metaFS(list)
	maxXattrBytes = 24 * 4
	got, err := fs.readXattrPairs(&xattrTable{}, xattrIDEntry{count: 4})
	if err != nil || len(got) != 4 {
		t.Fatalf("at the ceiling: %d pairs, err %v", len(got), err)
	}
	maxXattrBytes = 24*4 - 1
	if _, err := fs.readXattrPairs(&xattrTable{}, xattrIDEntry{count: 4}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("one byte under: err = %v, want ErrTooLarge", err)
	}
	// 2^32 - 1 empty pairs must stop at the ceiling too, not iterate 4 billion
	// times: every pair is charged its 8 bytes.
	var empties []byte
	for i := 0; i < 64; i++ {
		empties = append(empties, xattrPair("", 0)...)
	}
	maxXattrBytes = 8 * 32
	if _, err := metaFS(empties).readXattrPairs(&xattrTable{}, xattrIDEntry{count: 0xFFFFFFFF}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("2^32 empty pairs: err = %v, want ErrTooLarge", err)
	}
}

// ───────────────────────────── decompressors ───────────────────────────

// A zstd block of zero runs inflates ~10 000:1 (an RLE block is 3 bytes per
// 128 KiB). The shared decoder's default cap is 64 GiB and the maxOut check ran
// only after DecodeAll returned: a 7 172-byte block of 64 MiB of zeros
// allocated 67 554 288 bytes. Now the decoder stops at the format maximum.
func TestBombZstdStopsAtBlockSize(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	bomb := enc.EncodeAll(make([]byte, 64*miB), nil)
	refused(t, "64 MiB zstd block under a 128 KiB limit", 1<<20, func() {
		if _, err := (zstdDecompressor{}).decompress(bomb, 131072); err == nil {
			t.Error("zstd bomb accepted")
		}
	})
	// Exactness at the limit and one over, on a block that is allowed to be big.
	for _, tc := range []struct {
		n       int
		maxOut  int
		wantErr bool
	}{{131072, 131072, false}, {131073, 131072, true}, {miB, miB, false}} {
		z := enc.EncodeAll(make([]byte, tc.n), nil)
		out, err := zstdDecompressor{}.decompress(z, tc.maxOut)
		if (err != nil) != tc.wantErr || (err == nil && len(out) != tc.n) {
			t.Errorf("%d bytes under maxOut %d: len %d, err %v", tc.n, tc.maxOut, len(out), err)
		}
	}
}

// The lzma_alone header's 4-byte dictionary size is honoured by the reader up
// to 2 GiB-1: 13 header bytes allocated 1 GiB (measured 1 073 778 264).
func TestBombLZMADictionaryClamped(t *testing.T) {
	h := make([]byte, lzma.HeaderLen)
	h[0] = 0x5D
	binary.LittleEndian.PutUint32(h[1:], 1<<30)
	binary.LittleEndian.PutUint64(h[5:], 1<<40)
	refused(t, "13-byte lzma header, 1 GiB dictionary", 1<<20, func() {
		if _, err := (lzmaDecompressor{}).decompress(h, 131072); err == nil {
			t.Error("garbage lzma body accepted")
		}
	})

	// A real stream whose header declares a dictionary far above the block's
	// output still decodes: the clamp changes the allocation, not the bytes.
	want := bytes.Repeat([]byte("squashfs-lzma-"), 3000)
	var buf bytes.Buffer
	w, err := lzma.WriterConfig{Size: int64(len(want)), DictCap: 64 * miB}.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(want)
	w.Close()
	if d := binary.LittleEndian.Uint32(buf.Bytes()[1:]); d != 64*miB {
		t.Fatalf("test stream declares dictionary %d, want 64 MiB", d)
	}
	var got []byte
	refused(t, "64 MiB-dictionary lzma stream", 1<<20, func() {
		got, err = lzmaDecompressor{}.decompress(buf.Bytes(), len(want))
	})
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("clamped decode: len %d, err %v", len(got), err)
	}
}

// makeXZ returns a one-block xz stream of payload; prop, when non-zero,
// replaces the LZMA2 dictionary property in the block header (and its CRC32 is
// recomputed, so the stream stays well-formed apart from the claim).
func makeXZ(t *testing.T, payload []byte, blockSize int64, prop byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := xz.WriterConfig{DictCap: 4096, BlockSize: blockSize}.NewWriter(&b)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(payload)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	s := b.Bytes()
	if prop != 0 {
		hl := (int(s[12]) + 1) * 4
		if s[14] != 0x21 || s[15] != 1 {
			t.Fatalf("unexpected block header % x", s[12:12+hl])
		}
		s[16] = prop
		binary.LittleEndian.PutUint32(s[12+hl-4:], crc32.ChecksumIEEE(s[12:12+hl-4]))
	}
	return s
}

// The xz reader allocates the block header's declared LZMA2 dictionary and
// raises its own limit to it: a stream declaring 4 GiB allocated 4 295 013 784
// bytes to decode the five bytes "hello".
func TestBombXZDictionary(t *testing.T) {
	payload := []byte("hello")
	refused(t, "4 GiB dictionary claim", 1<<20, func() {
		if _, err := (xzDecompressor{}).decompress(makeXZ(t, payload, 0, 40), 131072); !errors.Is(err, ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
		}
	})

	var atLimit byte
	for p := byte(0); p <= 40; p++ {
		if d, _ := lzma.DecodeDictCap(p); d == xzMaxDictSize {
			atLimit = p
		}
	}
	if atLimit == 0 {
		t.Fatal("no dictionary property encodes xzMaxDictSize")
	}
	if out, err := (xzDecompressor{}).decompress(makeXZ(t, payload, 0, atLimit), 131072); err != nil || string(out) != "hello" {
		t.Errorf("dictionary exactly at the limit: %q, %v", out, err)
	}
	if _, err := (xzDecompressor{}).decompress(makeXZ(t, payload, 0, atLimit+1), 131072); !errors.Is(err, ErrCorrupt) {
		t.Errorf("dictionary one step over the limit: err = %v, want ErrCorrupt", err)
	}
}

// Only the first block's header is inspected, so streams carrying more than one
// block or more than one stream are refused (a squashfs block is one block of
// one stream; each further header could declare its own dictionary).
func TestXZSingleBlockSingleStream(t *testing.T) {
	multi := makeXZ(t, bytes.Repeat([]byte("0123456789"), 400), 1024, 0)
	if _, err := (xzDecompressor{}).decompress(multi, 131072); !errors.Is(err, ErrCorrupt) {
		t.Errorf("multi-block stream: err = %v, want ErrCorrupt", err)
	}
	one := makeXZ(t, []byte("hello"), 0, 0)
	if _, err := (xzDecompressor{}).decompress(append(append([]byte(nil), one...), one...), 131072); err == nil {
		t.Error("two concatenated streams accepted")
	}
	for _, short := range [][]byte{nil, one[:10], one[:len(one)-1], one[:23]} {
		if _, err := (xzDecompressor{}).decompress(short, 131072); err == nil {
			t.Errorf("truncated stream of %d bytes accepted", len(short))
		}
	}
	// And the ordinary case still round-trips.
	if out, err := (xzDecompressor{}).decompress(one, 131072); err != nil || string(out) != "hello" {
		t.Errorf("plain stream: %q, %v", out, err)
	}
}

// ───────────────────────────── fuzz targets ────────────────────────────

// FuzzDecompress feeds every decompressor arbitrary bytes under a 4 KiB output
// limit. Properties: no panic, the output never exceeds the limit, and no
// call allocates more than 64 MiB (the xz dictionary ceiling is 16 MiB).
func FuzzDecompress(f *testing.F) {
	enc, _ := zstd.NewWriter(nil)
	// The first argument selects a compression id as id%6+1.
	f.Add(uint8(compZSTD-1), enc.EncodeAll(make([]byte, 1<<20), nil))
	f.Add(uint8(compXZ-1), []byte("\xfd7zXZ\x00\x00\x01"))
	for _, c := range []Compressor{CompressGZIP, CompressXZ, CompressLZ4, CompressZSTD} {
		if cm, err := newCompressor(c); err == nil {
			if z, err := cm.compress([]byte("hello hello hello hello")); err == nil {
				f.Add(uint8(cm.id()-1), z)
			}
		}
	}
	lz := make([]byte, lzma.HeaderLen)
	lz[0] = 0x5D
	binary.LittleEndian.PutUint32(lz[1:], 1<<30)
	binary.LittleEndian.PutUint64(lz[5:], 1<<40)
	f.Add(uint8(compLZMA-1), lz)
	var xb bytes.Buffer
	if w, err := (xz.WriterConfig{DictCap: 4096}).NewWriter(&xb); err == nil {
		w.Write([]byte("hello"))
		w.Close()
		s := xb.Bytes()
		hl := (int(s[12]) + 1) * 4
		s[16] = 40
		binary.LittleEndian.PutUint32(s[12+hl-4:], crc32.ChecksumIEEE(s[12:12+hl-4]))
		f.Add(uint8(compXZ-1), s)
	}

	f.Fuzz(func(t *testing.T, id uint8, src []byte) {
		d, err := newDecompressor(uint16(id%6) + 1)
		if err != nil {
			return
		}
		const maxOut = 4096
		var out []byte
		n := allocated(func() { out, _ = d.decompress(src, maxOut) })
		if len(out) > maxOut {
			t.Fatalf("output %d bytes > limit %d", len(out), maxOut)
		}
		if n > 64*miB {
			t.Fatalf("decompressor %d allocated %d bytes for a %d-byte block", id%6+1, n, len(src))
		}
	})
}

// FuzzReadInode drives readInode over arbitrary inode-table bytes: the block
// list, symlink target and every size field are attacker-controlled.
func FuzzReadInode(f *testing.F) {
	f.Add(extFileInode(1<<41, 0))
	f.Add(extFileInode(131072*3, 12))
	f.Add(extFileInode(1<<60, 0))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		fs := metaFS(payload)
		var in *inode
		n := allocated(func() { in, _ = readInode(fs, 0) })
		if n > 8*miB {
			t.Fatalf("readInode allocated %d bytes for a %d-byte table", n, len(payload))
		}
		_ = in
	})
}

// FuzzReadXattr drives the xattr pair reader over arbitrary kv bytes with an
// arbitrary declared count.
func FuzzReadXattr(f *testing.F) {
	f.Add(xattrPair("comment", 12), uint32(1))
	f.Add(xattrPair("a", 0), uint32(0xFFFFFFFF))
	f.Add([]byte{0, 1, 1, 0, 'a', 8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, uint32(2))
	f.Fuzz(func(t *testing.T, kv []byte, count uint32) {
		fs := metaFS(kv)
		n := allocated(func() { _, _ = fs.readXattrPairs(&xattrTable{}, xattrIDEntry{count: count}) })
		if n > 64*miB {
			t.Fatalf("readXattrPairs allocated %d bytes for a %d-byte list, count %d", n, len(kv), count)
		}
	})
}
