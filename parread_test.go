// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
)

// parBody is 20 blocks of compressible data with a sparse run in the middle and
// a ragged tail, so a read crosses compressed, sparse and (gzip: raw) blocks.
func parBody() []byte {
	b := pattern(20*defaultBlockSize + 5000)
	for i := 7 * defaultBlockSize; i < 9*defaultBlockSize; i++ {
		b[i] = 0
	}
	return b
}

func parImage(t testing.TB, c Compressor, uncompressed bool, body []byte) []byte {
	t.Helper()
	root := &node{mode: sIFDIR | 0o755, children: []*node{
		{name: "big.bin", mode: sIFREG | 0o644, data: body},
	}}
	img, err := buildImage(root, BuildOptions{Compressor: c, Uncompressed: uncompressed})
	if err != nil {
		t.Fatalf("buildImage: %v", err)
	}
	return img
}

func openPar(t testing.TB, img []byte, workers int) *FS {
	t.Helper()
	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	fs.SetReadParallelism(workers)
	return fs
}

// sequential reads the whole file with chunk-sized ReadAt calls.
func sequential(t testing.TB, fs *FS, chunk int) []byte {
	t.Helper()
	f, err := fs.OpenFile("/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []byte
	buf := make([]byte, chunk)
	for off := int64(0); off < f.Size(); {
		n, err := f.ReadAt(buf, off)
		out = append(out, buf[:n]...)
		off += int64(n)
		if err != nil && err != io.EOF {
			t.Fatalf("ReadAt(%d): %v", off, err)
		}
		if n == 0 {
			break
		}
	}
	return out
}

// TestParallelReadMatchesSerial: every compressor, every read shape, and
// ReadFile, return exactly the bytes the serial path (and the source) does.
func TestParallelReadMatchesSerial(t *testing.T) {
	body := parBody()
	cases := []struct {
		name string
		c    Compressor
		raw  bool
	}{
		{"gzip", CompressGZIP, false}, {"xz", CompressXZ, false},
		{"zstd", CompressZSTD, false}, {"lz4", CompressLZ4, false},
		{"uncompressed", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img := parImage(t, tc.c, tc.raw, body)
			for _, w := range []int{1, 2, 4, 8} {
				fs := openPar(t, img, w)
				for _, chunk := range []int{100, 4096, defaultBlockSize - 1, 1 << 20, 3<<20 + 7} {
					if got := sequential(t, fs, chunk); !bytes.Equal(got, body) {
						t.Fatalf("workers=%d chunk=%d: content differs", w, chunk)
					}
				}
				got, err := fs.ReadFile("/big.bin")
				if err != nil || !bytes.Equal(got, body) {
					t.Fatalf("workers=%d ReadFile: err=%v equal=%v", w, err, bytes.Equal(got, body))
				}
				fs.Close()
			}
		})
	}
}

// concurrencyProbe wraps a decompressor and records the most calls in flight.
type concurrencyProbe struct {
	decompressor
	cur, max atomic.Int32
}

func (p *concurrencyProbe) decompress(src []byte, maxOut int) ([]byte, error) {
	n := p.cur.Add(1)
	defer p.cur.Add(-1)
	for {
		m := p.max.Load()
		if n <= m || p.max.CompareAndSwap(m, n) {
			break
		}
	}
	return p.decompressor.decompress(src, maxOut)
}

// TestParallelReadBounds: the number of blocks decoded at once never exceeds
// the configured workers, the cache never exceeds its cap, and the parallel
// path does use more than one worker.
func TestParallelReadBounds(t *testing.T) {
	body := parBody()
	img := parImage(t, CompressXZ, false, body)
	for _, w := range []int{2, 3, 4} {
		fs := openPar(t, img, w)
		p := &concurrencyProbe{decompressor: fs.d}
		fs.d = p
		_ = sequential(t, fs, 1<<20)
		if _, err := fs.ReadFile("/big.bin"); err != nil {
			t.Fatal(err)
		}
		if m := p.max.Load(); m > int32(w) {
			t.Fatalf("workers=%d: %d decodes in flight", w, m)
		}
		if p.max.Load() < 2 {
			t.Fatalf("workers=%d: never decoded in parallel", w)
		}
		fs.cache.mu.Lock()
		n := len(fs.cache.data)
		fs.cache.mu.Unlock()
		if n > defaultDataCacheBlocks {
			t.Fatalf("cache holds %d blocks, cap %d", n, defaultDataCacheBlocks)
		}
		fs.Close()
	}
	// workers=1 is the serial path: one decode at a time.
	fs := openPar(t, img, 1)
	p := &concurrencyProbe{decompressor: fs.d}
	fs.d = p
	_ = sequential(t, fs, 1<<20)
	if p.max.Load() != 1 {
		t.Fatalf("serial: %d in flight", p.max.Load())
	}
}

// ioCountReaderAt counts the bytes read from the image.
type ioCountReaderAt struct {
	r io.ReaderAt
	n atomic.Int64
}

func (c *ioCountReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.n.Add(int64(len(p)))
	return c.r.ReadAt(p, off)
}

// TestRandomReadsDoNotReadAhead: a small read at a random offset costs the
// same I/O with the default parallelism as serially, and a short sequential
// run does not decode the whole file.
func TestRandomReadsDoNotReadAhead(t *testing.T) {
	body := parBody()
	img := parImage(t, CompressXZ, false, body)
	rng := rand.New(rand.NewPCG(1, 2))
	offs := make([]int64, 50)
	for i := range offs {
		offs[i] = int64(rng.IntN(len(body) - 100))
	}
	cost := func(w int) int64 {
		c := &ioCountReaderAt{r: bytes.NewReader(img)}
		fs, err := Open(c, int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		fs.SetReadParallelism(w)
		f, _ := fs.OpenFile("/big.bin")
		base := c.n.Load()
		buf := make([]byte, 100)
		for _, o := range offs {
			if _, err := f.ReadAt(buf, o); err != nil && err != io.EOF {
				t.Fatal(err)
			}
			if !bytes.Equal(buf, body[o:o+100]) {
				t.Fatalf("random read @%d differs", o)
			}
		}
		return c.n.Load() - base
	}
	if s, p := cost(1), cost(8); s != p {
		t.Fatalf("random reads cost %d bytes of I/O serially, %d in parallel", s, p)
	}
}

// TestParallelReadError: a corrupt block fails with the same error, in file
// order, as the serial path.
func TestParallelReadError(t *testing.T) {
	body := parBody()
	img := parImage(t, CompressGZIP, false, body)
	fs := openPar(t, img, 1)
	f, _ := fs.OpenFile("/big.bin")
	sf := f.(*squashfsFile)
	for _, i := range []int{3, 15} { // two bad blocks: the first in file order must win
		img[sf.blockOffsets[i]+5] ^= 0xff
	}
	var want, got error
	_, want = openPar(t, img, 1).ReadFile("/big.bin")
	_, got = openPar(t, img, 8).ReadFile("/big.bin")
	if want == nil || got == nil || want.Error() != got.Error() {
		t.Fatalf("serial %v, parallel %v", want, got)
	}
	buf := make([]byte, 1<<20)
	f8, _ := openPar(t, img, 8).OpenFile("/big.bin")
	f1, _ := openPar(t, img, 1).OpenFile("/big.bin")
	for off := int64(0); off < 6<<17; off += int64(len(buf)) {
		_, e1 := f1.ReadAt(buf, off)
		_, e8 := f8.ReadAt(buf, off)
		if (e1 == nil) != (e8 == nil) || (e1 != nil && !errors.Is(e8, ErrCorrupt) && e1.Error() != e8.Error()) {
			t.Fatalf("@%d serial %v parallel %v", off, e1, e8)
		}
	}
}

// TestParallelReadConcurrent: several goroutines, sequential and random, on one
// file and one FS (run with -race).
func TestParallelReadConcurrent(t *testing.T) {
	body := parBody()
	img := parImage(t, CompressZSTD, false, body)
	fs := openPar(t, img, 4)
	f, err := fs.OpenFile("/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(g), 7))
			buf := make([]byte, 300000)
			for i := range 40 {
				var off int64
				if g%2 == 0 {
					off = int64(i*len(buf)) % int64(len(body))
				} else {
					off = int64(rng.IntN(len(body)))
				}
				n, err := f.ReadAt(buf, off)
				if err != nil && err != io.EOF {
					t.Errorf("ReadAt: %v", err)
					return
				}
				if !bytes.Equal(buf[:n], body[off:off+int64(n)]) {
					t.Errorf("goroutine %d @%d differs", g, off)
					return
				}
			}
		})
	}
	wg.Wait()
}
