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

// TestInterop_WriteUnsquashfs builds an image with this package and verifies
// the real unsquashfs can extract it back, contents intact. This is the
// write-side counterpart of the read interop tests. Skipped without unsquashfs.
func TestInterop_WriteUnsquashfs(t *testing.T) {
	unsq := findTool("unsquashfs")
	if unsq == "" {
		t.Skip("unsquashfs not available")
	}
	src := t.TempDir()
	files := buildTree(t, src)

	img := filepath.Join(t.TempDir(), "out.squashfs")
	if err := BuildFromDir(img, src, BuildOptions{}); err != nil {
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
}
