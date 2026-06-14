// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs_test

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-filesystems/squashfs"
)

// SquashFS is a read-only archive format: its filesystem.Filesystem mutators
// (WriteFile/MkDir/DeleteFile/...) return ErrReadOnly. These benchmarks
// therefore exercise the BUILD path (BuildFromDir) and the READ path
// (Open / ReadFile / ListDir / Stat), which is the whole of the live surface.

const (
	// benchNumFiles is the number of small files placed in the source tree.
	benchNumFiles = 100
	// benchSmallSize is the size of each small file.
	benchSmallSize = 1024
	// benchBigSize is the size of the single large file read back by
	// BenchmarkReadFileSeq. A few MiB is enough to dominate the per-call
	// fixed costs without making the suite slow.
	benchBigSize = 4 << 20 // 4 MiB
	// benchBigName is the path of the large file inside the image.
	benchBigName = "big.bin"
)

// makeSourceTree populates dir with a modest tree: benchNumFiles small files,
// one multi-MiB file, and a nested subdirectory. The contents are
// deterministic per path so build output is reproducible across runs.
func makeSourceTree(tb testing.TB, dir string) {
	tb.Helper()

	// Small files at the top level.
	for i := 0; i < benchNumFiles; i++ {
		name := filepath.Join(dir, fmt.Sprintf("file%03d.txt", i))
		if err := os.WriteFile(name, fill(benchSmallSize, int64(i)), 0o644); err != nil {
			tb.Fatalf("write %s: %v", name, err)
		}
	}

	// A nested directory with a handful of entries, so ListDir/Stat have a
	// non-trivial directory table to walk.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		tb.Fatalf("mkdir %s: %v", sub, err)
	}
	for i := 0; i < 10; i++ {
		name := filepath.Join(sub, fmt.Sprintf("nested%02d.dat", i))
		if err := os.WriteFile(name, fill(benchSmallSize, int64(1000+i)), 0o644); err != nil {
			tb.Fatalf("write %s: %v", name, err)
		}
	}

	// One large file, exercised by BenchmarkReadFileSeq.
	if err := os.WriteFile(filepath.Join(dir, benchBigName), fill(benchBigSize, 42), 0o644); err != nil {
		tb.Fatalf("write big file: %v", err)
	}
}

// fill returns n bytes of pseudo-random, seed-derived data. Random-ish content
// keeps the gzip compressor honest (no trivial all-zero runs) while staying
// deterministic for a given seed.
func fill(n int, seed int64) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewSource(seed))
	_, _ = r.Read(b)
	return b
}

// buildImage builds an image from a freshly populated source tree and returns
// its path. Used to prime the read-path benchmarks.
func buildImage(tb testing.TB, opts squashfs.BuildOptions) string {
	tb.Helper()
	src := tb.TempDir()
	makeSourceTree(tb, src)
	img := filepath.Join(tb.TempDir(), "bench.squashfs")
	if err := squashfs.BuildFromDir(img, src, opts); err != nil {
		tb.Fatalf("BuildFromDir: %v", err)
	}
	return img
}

// BenchmarkBuild measures the full build path: scanning a host directory tree
// and emitting a gzip-compressed SquashFS image to disk.
func BenchmarkBuild(b *testing.B) {
	src := b.TempDir()
	makeSourceTree(b, src)
	outDir := b.TempDir()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		img := filepath.Join(outDir, fmt.Sprintf("out%d.squashfs", i))
		if err := squashfs.BuildFromDir(img, src, squashfs.BuildOptions{}); err != nil {
			b.Fatalf("BuildFromDir: %v", err)
		}
	}
}

// BenchmarkOpen measures parsing the superblock and constructing a reader for a
// prebuilt image (via Open over an *os.File held open across iterations).
func BenchmarkOpen(b *testing.B) {
	img := buildImage(b, squashfs.BuildOptions{})
	f, err := os.Open(img)
	if err != nil {
		b.Fatalf("open image: %v", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		b.Fatalf("stat image: %v", err)
	}
	size := st.Size()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fs, err := squashfs.Open(f, size)
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		_ = fs
	}
}

// BenchmarkReadFileSeq measures sequential read throughput of the large file,
// covering block decompression and the fragment/block-list walk. b.SetBytes
// makes go test report MB/s.
func BenchmarkReadFileSeq(b *testing.B) {
	img := buildImage(b, squashfs.BuildOptions{})
	fs, err := squashfs.OpenFile(img)
	if err != nil {
		b.Fatalf("OpenFile: %v", err)
	}
	defer fs.Close()

	// Confirm the file is present and grab its size for SetBytes.
	data, err := fs.ReadFile("/" + benchBigName)
	if err != nil {
		b.Fatalf("ReadFile(/%s): %v", benchBigName, err)
	}
	b.SetBytes(int64(len(data)))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := fs.ReadFile("/" + benchBigName)
		if err != nil {
			b.Fatalf("ReadFile: %v", err)
		}
		if len(got) != len(data) {
			b.Fatalf("short read: got %d want %d", len(got), len(data))
		}
	}
}

// BenchmarkListDir measures enumerating the root directory, which holds the
// benchNumFiles small files plus the big file and the subdirectory.
func BenchmarkListDir(b *testing.B) {
	img := buildImage(b, squashfs.BuildOptions{})
	fs, err := squashfs.OpenFile(img)
	if err != nil {
		b.Fatalf("OpenFile: %v", err)
	}
	defer fs.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries, err := fs.ListDir("/")
		if err != nil {
			b.Fatalf("ListDir: %v", err)
		}
		if len(entries) == 0 {
			b.Fatal("ListDir returned no entries")
		}
	}
}

// BenchmarkStat measures resolving a path (root-relative walk + inode read)
// for a single nested file.
func BenchmarkStat(b *testing.B) {
	img := buildImage(b, squashfs.BuildOptions{})
	fs, err := squashfs.OpenFile(img)
	if err != nil {
		b.Fatalf("OpenFile: %v", err)
	}
	defer fs.Close()

	const target = "/sub/nested00.dat"
	if _, err := fs.Stat(target); err != nil {
		b.Fatalf("Stat(%s): %v", target, err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := fs.Stat(target); err != nil {
			b.Fatalf("Stat: %v", err)
		}
	}
}
