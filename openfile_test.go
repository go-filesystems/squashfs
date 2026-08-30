// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// probeOpener asserts the capability is reachable the way a caller reaches it —
// through the filesystem.Filesystem interface, not the concrete *FS.
func probeOpener(t *testing.T, fs *FS) filesystem.Opener {
	t.Helper()
	var generic filesystem.Filesystem = fs
	o, ok := generic.(filesystem.Opener)
	if !ok {
		t.Fatal("squashfs does not satisfy filesystem.Opener")
	}
	return o
}

// checkAgainstReadFile is the verification that matters: for a file on a real
// image, ReadAt must return EXACTLY the corresponding slice of what ReadFile
// returns.
//
// It sweeps two ways. A dense pass reads the whole file in chunks whose size is
// coprime with the 128 KiB block size (777, 65537, 131071), so successive reads
// straddle block and fragment boundaries at a different phase each time — the
// pass no off-by-one-block survives. Then it targets the boundaries themselves:
// every logical block edge and the start of the tail fragment, ±1.
func checkAgainstReadFile(t *testing.T, fs *FS, path string) {
	t.Helper()
	want, err := fs.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	f, err := probeOpener(t, fs).OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", path, err)
	}
	defer f.Close()

	size := int64(len(want))
	if f.Size() != size {
		t.Fatalf("%s: Size() = %d, want %d (len of ReadFile)", path, f.Size(), size)
	}

	for _, chunk := range []int{777, 65537, 131071} {
		got := make([]byte, 0, size)
		buf := make([]byte, chunk)
		for off := int64(0); off < size; off += int64(chunk) {
			n, err := f.ReadAt(buf, off)
			full := off+int64(chunk) <= size
			if full && err != nil {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) err = %v, want nil", path, chunk, off, err)
			}
			if !full && !errors.Is(err, io.EOF) {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) err = %v, want io.EOF", path, chunk, off, err)
			}
			got = append(got, buf[:n]...)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: chunked ReadAt (chunk=%d) differs from ReadFile", path, chunk)
		}
	}

	bs := int64(fs.sb.BlockSize)
	offsets := map[int64]bool{0: true, size: true}
	if size > 0 {
		offsets[size-1] = true
	}
	for b := bs; b < size+bs; b += bs {
		for _, o := range []int64{b - 1, b, b + 1} {
			if o >= 0 && o <= size {
				offsets[o] = true
			}
		}
	}
	for off := range offsets {
		for _, l := range []int{1, 9, int(bs) - 1, int(bs), int(bs) + 1} {
			if l <= 0 {
				continue
			}
			p := make([]byte, l)
			n, err := f.ReadAt(p, off)
			short := off+int64(l) > size
			wantN := l
			if short {
				wantN = int(size - off)
			}
			if n != wantN {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) n = %d, want %d", path, l, off, n, wantN)
			}
			if short {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("%s: ReadAt(len=%d, off=%d) err = %v, want io.EOF", path, l, off, err)
				}
			} else if err != nil {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) err = %v, want nil", path, l, off, err)
			}
			if !bytes.Equal(p[:n], want[off:off+int64(n)]) {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) bytes differ from ReadFile[%d:%d]", path, l, off, off, off+int64(n))
			}
		}
	}

	// io.SectionReader is the consumer the io.ReaderAt contract protects.
	got, err := io.ReadAll(io.NewSectionReader(f, 0, size))
	if err != nil {
		t.Fatalf("%s: ReadAll(SectionReader): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: SectionReader round-trip differs from ReadFile", path)
	}
}

// TestInterop_MksquashfsOpenerReadAt is the cross-tool proof: images mastered
// by the real mksquashfs, in every compressor it offers here, read back through
// ReadAt. The layout — block boundaries, which tails share a fragment, how well
// each block compresses — is somebody else's. CI installs squashfs-tools, so
// this runs there; it skips when the tool is absent.
func TestInterop_MksquashfsOpenerReadAt(t *testing.T) {
	mksquashfs := findTool("mksquashfs")
	if mksquashfs == "" {
		t.Skip("mksquashfs not available — skipping squashfs OpenFile interop test")
	}
	src := t.TempDir()
	files := map[string][]byte{
		// Several full 128 KiB blocks plus a tail: the multi-block path and
		// the fragment path in one file.
		"big.bin": pattern(400000),
		// Highly compressible, so its blocks are tiny on disk and the prefix
		// sum of block lengths is what locates them — an implementation that
		// assumed a fixed on-disk stride reads garbage here.
		"zeros.bin": make([]byte, 300000),
		// Sub-block files whose tails share one fragment block.
		"a.txt": []byte("first small file\n"),
		"b.txt": bytes.Repeat([]byte("second\n"), 100),
		// Exactly one block: no fragment at all.
		"exact.bin":      pattern(131072),
		"sub/nested.bin": pattern(200000),
	}
	for name, content := range files {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	for _, comp := range []string{"gzip", "xz", "zstd", "lz4", "lzo"} {
		t.Run(comp, func(t *testing.T) {
			img := filepath.Join(t.TempDir(), "img.squashfs")
			args := []string{src, img, "-noappend", "-no-progress", "-comp", comp}
			if comp == "lz4" {
				args = append(args, "-Xhc")
			}
			out, err := exec.Command(mksquashfs, args...).CombinedOutput()
			if err != nil {
				t.Skipf("mksquashfs lacks %s: %s", comp, out)
			}
			fs, err := OpenFile(img)
			if err != nil {
				t.Fatalf("OpenFile(%s): %v", img, err)
			}
			defer fs.Close()

			for name := range files {
				checkAgainstReadFile(t, fs, "/"+name)
			}

			// And the bytes really are the host's, before mastering.
			f, err := probeOpener(t, fs).OpenFile("/big.bin")
			if err != nil {
				t.Fatalf("OpenFile(/big.bin): %v", err)
			}
			defer f.Close()
			got := make([]byte, 5000)
			if n, err := f.ReadAt(got, 250000); n != 5000 || err != nil {
				t.Fatalf("ReadAt(5000, 250000) = %d, %v", n, err)
			}
			if !bytes.Equal(got, files["big.bin"][250000:255000]) {
				t.Fatal("ReadAt bytes differ from the file mastered into the image")
			}
		})
	}
}

// TestOpenerFileBuiltImage repeats the proof on images this package masters,
// compressed and uncompressed and with fragments disabled — the last one
// forcing every file onto the full-block path with no tail fragment.
func TestOpenerFileBuiltImage(t *testing.T) {
	body := pattern(400000)
	cases := []struct {
		name string
		opts BuildOptions
	}{
		{"gzip", BuildOptions{}},
		{"uncompressed", BuildOptions{Uncompressed: true}},
		{"nofragments", BuildOptions{NoFragments: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := &node{
				name: "",
				mode: sIFDIR | 0o755,
				children: []*node{
					{name: "big.bin", mode: sIFREG | 0o644, data: body},
					{name: "small.txt", mode: sIFREG | 0o644, data: []byte("hello squashfs\n")},
					{name: "empty.bin", mode: sIFREG | 0o644},
					{name: "zeros.bin", mode: sIFREG | 0o644, data: make([]byte, 300000)},
					{name: "exact.bin", mode: sIFREG | 0o644, data: pattern(defaultBlockSize)},
					{name: "sub", mode: sIFDIR | 0o755, children: []*node{
						{name: "nested.bin", mode: sIFREG | 0o644, data: pattern(200000)},
					}},
				},
			}
			img, err := buildImage(root, tc.opts)
			if err != nil {
				t.Fatalf("buildImage: %v", err)
			}
			fs, err := Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer fs.Close()
			for _, p := range []string{"/big.bin", "/small.txt", "/empty.bin", "/zeros.bin", "/exact.bin", "/sub/nested.bin"} {
				checkAgainstReadFile(t, fs, p)
			}

			// Report the map each file actually got, so the suite cannot
			// silently exercise one shape three times.
			f, err := probeOpener(t, fs).OpenFile("/big.bin")
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			sf := f.(*squashfsFile)
			t.Logf("%s /big.bin: %d blocks, fragment=%v, size=%d, blockSize=%d",
				tc.name, len(sf.blockOffsets), sf.fragIdx != invalidFrag, sf.size, sf.blockSize)
			f.Close()
		})
	}
}

// TestOpenerFileEOFSemantics pins the io.ReaderAt end-of-file rules. A short
// read with a nil error is the failure mode that breaks io.SectionReader.
func TestOpenerFileEOFSemantics(t *testing.T) {
	fs := openImage(t)
	defer fs.Close()
	f, err := probeOpener(t, fs).OpenFile("/small.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	const body = "hello squashfs\n"
	if f.Size() != int64(len(body)) {
		t.Fatalf("Size() = %d, want %d", f.Size(), len(body))
	}
	p := make([]byte, 5)
	if n, err := f.ReadAt(p, 6); n != 5 || err != nil || string(p) != "squas" {
		t.Fatalf("ReadAt(5,6) = %d, %v, %q", n, err, p)
	}
	// Straddling the end: bytes AND io.EOF.
	if n, err := f.ReadAt(p, int64(len(body))-2); n != 2 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt straddling end = %d, %v; want 2, io.EOF", n, err)
	}
	// At and past Size(): 0, io.EOF.
	if n, err := f.ReadAt(p, int64(len(body))); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt at Size() = %d, %v; want 0, io.EOF", n, err)
	}
	if n, err := f.ReadAt(p, 1<<40); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt past Size() = %d, %v; want 0, io.EOF", n, err)
	}
	// Zero-length read inside the file is a full read.
	if n, err := f.ReadAt(nil, 2); n != 0 || err != nil {
		t.Fatalf("ReadAt(empty,2) = %d, %v; want 0, nil", n, err)
	}
	// Negative offset errors instead of panicking.
	if n, err := f.ReadAt(p, -1); n != 0 || err == nil {
		t.Fatalf("ReadAt(-1) = %d, %v; want an error", n, err)
	}
	// Close is idempotent; a read after it fails loudly.
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n, err := f.ReadAt(p, 0); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("ReadAt after Close = %d, %v; want 0, os.ErrClosed", n, err)
	}
}

// TestOpenerFileRejects covers the refusal paths: a directory, a path that does
// not resolve. Symlinks are followed, as ReadFile follows them.
func TestOpenerFileRejects(t *testing.T) {
	fs := openImage(t)
	defer fs.Close()
	o := probeOpener(t, fs)

	if _, err := o.OpenFile("/sub"); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("OpenFile(dir) = %v, want ErrNotRegular", err)
	}
	if _, err := o.OpenFile("/"); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("OpenFile(/) = %v, want ErrNotRegular", err)
	}
	if _, err := o.OpenFile("/nope.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenFile(missing) = %v, want ErrNotFound", err)
	}
	// "/link" points at small.txt: the symlink is followed.
	f, err := o.OpenFile("/link")
	if err != nil {
		t.Fatalf("OpenFile(symlink): %v", err)
	}
	defer f.Close()
	if f.Size() != int64(len("hello squashfs\n")) {
		t.Fatalf("Size() through symlink = %d", f.Size())
	}
}

// TestOpenerFileConcurrentReads exercises the concurrency guarantee io.ReaderAt
// makes and a mount depends on. It matters more here than elsewhere: every read
// goes through the shared decompressed-block cache, so under -race this is what
// would catch an unsynchronised cache access on the new path.
func TestOpenerFileConcurrentReads(t *testing.T) {
	fs := openImage(t)
	defer fs.Close()
	want, err := fs.ReadFile("/big.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	f, err := probeOpener(t, fs).OpenFile("/big.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i) * 9377
			if off > f.Size() {
				off = f.Size()
			}
			p := make([]byte, 40000+i)
			n, err := f.ReadAt(p, off)
			if err != nil && !errors.Is(err, io.EOF) {
				errCh <- fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			if !bytes.Equal(p[:n], want[off:off+int64(n)]) {
				errCh <- fmt.Errorf("goroutine %d: bytes differ at off=%d", i, off)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// TestOpenerFileSparseBlock covers the sparse-block branch: a zero on-disk
// length means a whole block of zeros with no read at all, which is exactly
// what readFile appends for it. The shape is built by hand because the writer
// never emits one.
func TestOpenerFileSparseBlock(t *testing.T) {
	fs := openImage(t)
	defer fs.Close()
	bs := int64(fs.sb.BlockSize)
	in := &inode{
		Type:       inodeBasicFile,
		Size:       uint64(2 * bs),
		blockSizes: []uint32{0, 0},
		fragIdx:    invalidFrag,
	}
	f, err := fs.newFile(in)
	if err != nil {
		t.Fatalf("newFile: %v", err)
	}
	defer f.Close()
	want, err := readFile(fs, in)
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	got := make([]byte, 2*bs)
	if n, err := f.ReadAt(got, 0); int64(n) != 2*bs || err != nil {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("sparse read differs from ReadFile")
	}
	// A read starting inside the second sparse block is still zeros.
	tail := make([]byte, 16)
	if n, err := f.ReadAt(tail, bs+7); n != 16 || err != nil {
		t.Fatalf("ReadAt in second sparse block = %d, %v", n, err)
	}
	if !bytes.Equal(tail, make([]byte, 16)) {
		t.Fatalf("sparse read gave % x, want zeros", tail)
	}
}

// TestNewFileSizeExceedsReachable covers the corruption bound readFile also
// applies: a declared size larger than the block list plus one fragment can
// supply is a forged inode, not a big file.
func TestNewFileSizeExceedsReachable(t *testing.T) {
	fs := openImage(t)
	defer fs.Close()
	in := &inode{
		Type:       inodeBasicFile,
		Size:       uint64(fs.sb.BlockSize)*3 + 1,
		blockSizes: []uint32{1, 1},
		fragIdx:    invalidFrag,
	}
	if _, err := fs.newFile(in); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("newFile(over-declared size) = %v, want ErrCorrupt", err)
	}
	// readFile refuses the same inode, so the two paths agree.
	if _, err := readFile(fs, in); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("readFile(over-declared size) = %v, want ErrCorrupt", err)
	}
}

// TestReadAtMissingFragment covers the case where the offset runs past the
// block list and the inode declares no fragment: the file claims bytes nothing
// can supply.
func TestReadAtMissingFragment(t *testing.T) {
	fs := openImage(t)
	defer fs.Close()
	bs := int64(fs.sb.BlockSize)
	in := &inode{
		Type:       inodeBasicFile,
		Size:       uint64(bs + 10),
		blockSizes: []uint32{0},
		fragIdx:    invalidFrag,
	}
	f, err := fs.newFile(in)
	if err != nil {
		t.Fatalf("newFile: %v", err)
	}
	defer f.Close()
	n, err := f.ReadAt(make([]byte, 4), bs)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadAt past blocks with no fragment = %v, want ErrCorrupt", err)
	}
	if n != 0 {
		t.Fatalf("ReadAt n = %d, want 0", n)
	}
}

// TestReadAtFragmentOutOfRange covers the fragment bounds check: a forged
// fragOffset must not let a file read past its own tail into another's.
func TestReadAtFragmentOutOfRange(t *testing.T) {
	fs := openImage(t)
	defer fs.Close()
	// Borrow a real file's fragment, then push the offset out of range.
	src, err := fs.OpenFile("/small.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	sf := src.(*squashfsFile)
	if sf.fragIdx == invalidFrag {
		t.Skip("small.txt has no fragment in this image")
	}
	src.Close()

	f := &squashfsFile{
		fs:         fs,
		fragIdx:    sf.fragIdx,
		fragOffset: 1 << 30,
		size:       16,
		blockSize:  int64(fs.sb.BlockSize),
	}
	if _, err := f.ReadAt(make([]byte, 4), 0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadAt with a forged fragOffset = %v, want ErrCorrupt", err)
	}

	// A bad fragment INDEX must surface the fragment table's own error.
	g := &squashfsFile{
		fs:        fs,
		fragIdx:   fs.sb.FragCount + 10,
		size:      16,
		blockSize: int64(fs.sb.BlockSize),
	}
	if _, err := g.ReadAt(make([]byte, 4), 0); err == nil {
		t.Fatal("ReadAt with an out-of-range fragment index: want an error")
	}
}

// failingReaderAt fails every read at or past failFrom so a block read error
// can be injected at an exact image offset.
type failingReaderAt struct {
	inner    io.ReaderAt
	failFrom int64
}

func (r failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.failFrom {
		return 0, io.ErrUnexpectedEOF
	}
	return r.inner.ReadAt(p, off)
}

// TestReadAtBlockReadError covers the I/O error branch of the data-block path:
// the failure must surface rather than become a silent short read.
func TestReadAtBlockReadError(t *testing.T) {
	img := goodImage(t)
	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	f, err := fs.OpenFile("/big.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()
	sf := f.(*squashfsFile)
	if len(sf.blockOffsets) == 0 {
		t.Skip("big.bin has no full blocks in this image")
	}
	// Fail from the file's first data block onwards, after the map is built.
	fs.rs = failingReaderAt{inner: bytes.NewReader(img), failFrom: sf.blockOffsets[0]}
	n, err := f.ReadAt(make([]byte, 64), 0)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadAt err = %v, want io.ErrUnexpectedEOF", err)
	}
	if n != 0 {
		t.Fatalf("ReadAt n = %d, want 0", n)
	}
}

// TestReadAtShortBlock covers the guard readFile also carries: a block that
// decompresses to fewer bytes than its logical length is corrupt, and must be
// reported rather than silently short-read.
func TestReadAtShortBlock(t *testing.T) {
	fs := openImage(t)
	defer fs.Close()
	src, err := fs.OpenFile("/small.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	sf := src.(*squashfsFile)
	src.Close()
	if sf.fragIdx == invalidFrag {
		t.Skip("small.txt has no fragment in this image")
	}
	// Point a "full block" at the fragment block, which decompresses to far
	// fewer bytes than one logical block.
	fe, err := fs.readFragmentEntry(sf.fragIdx)
	if err != nil {
		t.Fatalf("readFragmentEntry: %v", err)
	}
	f := &squashfsFile{
		fs:           fs,
		blockOffsets: []int64{int64(fe.start)},
		blockWords:   []uint32{fe.sizeWord},
		fragIdx:      invalidFrag,
		size:         int64(fs.sb.BlockSize),
		blockSize:    int64(fs.sb.BlockSize),
	}
	if _, err := f.ReadAt(make([]byte, 16), 0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadAt over a short block = %v, want ErrCorrupt", err)
	}
}
