// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// buildTree writes a fixed source tree under dir and returns the file contents.
func buildTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{
		"hello.txt":      []byte("hello squashfs writer\n"),
		"big.bin":        pattern(300000), // multi-block
		"empty.txt":      {},
		"sub/nested.txt": []byte("nested\n"),
	}
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("hello.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	return files
}

// TestWrite_RoundTrip builds an image with BuildFromDir and reads it back with
// this package's own reader (no external tools needed).
func TestWrite_RoundTrip(t *testing.T) {
	for _, uncompressed := range []bool{false, true} {
		name := "gzip"
		if uncompressed {
			name = "uncompressed"
		}
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			files := buildTree(t, src)
			img := filepath.Join(t.TempDir(), "out.squashfs")
			if err := BuildFromDir(img, src, BuildOptions{Uncompressed: uncompressed}); err != nil {
				t.Fatalf("BuildFromDir: %v", err)
			}
			fs, err := OpenFile(img)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			defer fs.Close()

			for fname, want := range files {
				got, err := fs.ReadFile("/" + fname)
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("ReadFile(/%s): err=%v equal=%v", fname, err, bytes.Equal(got, want))
				}
			}
			if tgt, err := fs.ReadLink("/link"); err != nil || tgt != "hello.txt" {
				t.Errorf("ReadLink(/link) = %q, %v", tgt, err)
			}
			entries, err := fs.ListDir("/")
			if err != nil {
				t.Fatalf("ListDir(/): %v", err)
			}
			seen := map[string]bool{}
			for _, e := range entries {
				seen[e.Name()] = true
			}
			for _, n := range []string{"hello.txt", "big.bin", "empty.txt", "sub", "link"} {
				if !seen[n] {
					t.Errorf("ListDir(/) missing %q", n)
				}
			}
			if sub, err := fs.ListDir("/sub"); err != nil || len(sub) != 1 || sub[0].Name() != "nested.txt" {
				t.Errorf("ListDir(/sub): err=%v len=%d", err, len(sub))
			}
		})
	}
}

// fragTree writes a source tree mixing many sub-block files (whose tails or
// whole bodies pack into fragments) with a multi-block file, plus a file whose
// size is an exact block multiple (no fragment) and an empty file.
func fragTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{
		"empty.txt":      {},
		"tiny.txt":       []byte("x"),
		"small.bin":      pattern(100),                    // whole file -> fragment
		"medium.bin":     pattern(defaultBlockSize + 137), // one full block + frag tail
		"exact.bin":      pattern(defaultBlockSize),       // exact block, no fragment
		"big.bin":        pattern(3*defaultBlockSize + 9), // 3 blocks + frag tail
		"sub/a.txt":      []byte("alpha"),
		"sub/b.txt":      []byte("beta beta"),
		"sub/deep/c.txt": pattern(500),
	}
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// readAll round-trips every file in want through the package's own reader and
// asserts byte-exact contents.
func readAll(t *testing.T, fs *FS, want map[string][]byte) {
	t.Helper()
	for name, data := range want {
		got, err := fs.ReadFile("/" + name)
		if err != nil {
			t.Errorf("ReadFile(/%s): %v", name, err)
			continue
		}
		if !bytes.Equal(got, data) {
			t.Errorf("ReadFile(/%s): %d bytes, want %d (equal=false)", name, len(got), len(data))
		}
	}
}

// TestWrite_Fragments verifies that fragment packing produces a smaller image,
// sets a non-zero fragment count and clears flagNoFragments, while every file
// still round-trips byte-exact through this package's reader.
func TestWrite_Fragments(t *testing.T) {
	src := t.TempDir()
	want := fragTree(t, src)

	withFrag := filepath.Join(t.TempDir(), "frag.squashfs")
	noFrag := filepath.Join(t.TempDir(), "nofrag.squashfs")
	if err := BuildFromDir(withFrag, src, BuildOptions{}); err != nil {
		t.Fatalf("BuildFromDir (fragments): %v", err)
	}
	if err := BuildFromDir(noFrag, src, BuildOptions{NoFragments: true}); err != nil {
		t.Fatalf("BuildFromDir (no fragments): %v", err)
	}

	fs, err := OpenFile(withFrag)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fs.Close()

	sb := fs.Superblock()
	if sb.FragCount == 0 {
		t.Errorf("FragCount = 0, want > 0 (fragments not packed)")
	}
	// All seven sub-block tails are tiny and must share a single fragment block;
	// proving packing, not one-block-per-tail.
	if sb.FragCount != 1 {
		t.Errorf("FragCount = %d, want 1 (tails should share one fragment block)", sb.FragCount)
	}
	if sb.Flags&flagNoFragments != 0 {
		t.Errorf("flagNoFragments set despite packed fragments")
	}
	readAll(t, fs, want)

	if tr, err := fs.ListDir("/sub"); err != nil || len(tr) != 3 {
		t.Errorf("ListDir(/sub): err=%v len=%d want 3", err, len(tr))
	}
	if tr, err := fs.ListDir("/sub/deep"); err != nil || len(tr) != 1 || tr[0].Name() != "c.txt" {
		t.Errorf("ListDir(/sub/deep): err=%v len=%d", err, len(tr))
	}

	// Fragment packing should shrink the image versus full-block storage.
	si, err1 := os.Stat(withFrag)
	ni, err2 := os.Stat(noFrag)
	if err1 != nil || err2 != nil {
		t.Fatalf("stat: %v %v", err1, err2)
	}
	if si.Size() >= ni.Size() {
		t.Errorf("fragment image (%d) not smaller than no-fragment image (%d)", si.Size(), ni.Size())
	}
}

// TestWrite_Compressors round-trips an image built with each supported write
// compressor through the package's own reader, asserting the superblock
// records the chosen compressor and contents are byte-exact.
func TestWrite_Compressors(t *testing.T) {
	cases := []struct {
		name string
		comp Compressor
		id   uint16
	}{
		{"gzip", CompressGZIP, compGZIP},
		{"zstd", CompressZSTD, compZSTD},
		{"xz", CompressXZ, compXZ},
		{"lz4", CompressLZ4, compLZ4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := t.TempDir()
			want := fragTree(t, src)
			img := filepath.Join(t.TempDir(), "out.squashfs")
			if err := BuildFromDir(img, src, BuildOptions{Compressor: tc.comp}); err != nil {
				t.Fatalf("BuildFromDir(%s): %v", tc.name, err)
			}
			fs, err := OpenFile(img)
			if err != nil {
				t.Fatalf("OpenFile(%s): %v", tc.name, err)
			}
			defer fs.Close()
			if got := fs.Superblock().Compression; got != tc.id {
				t.Errorf("Compression = %d, want %d", got, tc.id)
			}
			readAll(t, fs, want)
		})
	}
}

// TestInterop_WriteUnsquashfs builds an image with this package and verifies
// the real unsquashfs can extract it back, contents intact. This is the
// write-side counterpart of the read interop tests. Skipped without unsquashfs.
func TestInterop_WriteUnsquashfs(t *testing.T) {
	unsq := findTool("unsquashfs")
	if unsq == "" {
		t.Skip("unsquashfs not available")
	}
	// Each supported write compressor must produce an image real unsquashfs
	// accepts and extracts byte-exact. LZ4 in particular needs the mandatory
	// compressor-options block; an image lacking it is rejected here.
	cases := []struct {
		name string
		comp Compressor
	}{
		{"gzip", CompressGZIP},
		{"zstd", CompressZSTD},
		{"xz", CompressXZ},
		{"lz4", CompressLZ4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := t.TempDir()
			files := buildTree(t, src)

			img := filepath.Join(t.TempDir(), "out.squashfs")
			if err := BuildFromDir(img, src, BuildOptions{Compressor: tc.comp}); err != nil {
				t.Fatalf("BuildFromDir: %v", err)
			}

			dest := filepath.Join(t.TempDir(), "extract")
			if out, err := exec.Command(unsq, "-d", dest, img).CombinedOutput(); err != nil {
				t.Fatalf("unsquashfs: %v\n%s", err, out)
			}

			for name, want := range files {
				got, err := os.ReadFile(filepath.Join(dest, name))
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("extracted /%s: err=%v equal=%v", name, err, bytes.Equal(got, want))
				}
			}
			// Symlink extracted with the right target.
			if tgt, err := os.Readlink(filepath.Join(dest, "link")); err != nil || tgt != "hello.txt" {
				t.Errorf("extracted /link = %q, %v; want hello.txt", tgt, err)
			}
		})
	}
}
