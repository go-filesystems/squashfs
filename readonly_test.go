// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenFile covers the path-based OpenFile constructor and the file-backed
// Close path (fs.closer != nil), plus its two error returns.
func TestOpenFile(t *testing.T) {
	img := goodImage(t)
	path := filepath.Join(t.TempDir(), "img.sqfs")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}

	fs, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile(good): %v", err)
	}
	if fs.Superblock() == nil {
		t.Fatal("Superblock() returned nil")
	}
	// Close must release the backing file handle (closer branch).
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// os.Open failure: a path that does not exist.
	if _, err := OpenFile(filepath.Join(t.TempDir(), "nope.sqfs")); err == nil {
		t.Error("OpenFile(missing): expected error, got nil")
	}

	// Open (parse) failure: a real file that is not a SquashFS image.
	bad := filepath.Join(t.TempDir(), "bad.sqfs")
	if err := os.WriteFile(bad, []byte("not a squashfs image at all"), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	if _, err := OpenFile(bad); err == nil {
		t.Error("OpenFile(non-squashfs): expected parse error, got nil")
	}
}

// TestReadOnlyMethods verifies that every mutating filesystem.Filesystem method
// returns ErrReadOnly — SquashFS is a read-only archive format. These stubs had
// no coverage; this exercises each one.
func TestReadOnlyMethods(t *testing.T) {
	fs := openImage(t)
	checks := []struct {
		name string
		err  error
	}{
		{"WriteFile", fs.WriteFile("/x", []byte("data"), 0o644)},
		{"MkDir", fs.MkDir("/d", 0o755)},
		{"DeleteFile", fs.DeleteFile("/x")},
		{"DeleteDir", fs.DeleteDir("/d")},
		{"Rename", fs.Rename("/x", "/y")},
	}
	for _, c := range checks {
		if !errors.Is(c.err, ErrReadOnly) {
			t.Errorf("%s: got %v, want ErrReadOnly", c.name, c.err)
		}
	}
}
