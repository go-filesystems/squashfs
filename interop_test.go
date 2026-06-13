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

func findTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range []string{"/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin"} {
		c := filepath.Join(d, name)
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// TestInterop_LargeDir packs a directory with more entries than fit under a
// single SquashFS directory header (256) and more inodes than fit in one 8 KiB
// metadata block, exercising the multi-header directory walk and the cursor's
// block-crossing in both the inode and directory tables.
func TestInterop_LargeDir(t *testing.T) {
	mksquashfs := findTool("mksquashfs")
	if mksquashfs == "" {
		t.Skip("mksquashfs not available")
	}
	const n = 600
	src := t.TempDir()
	for i := 0; i < n; i++ {
		name := filepath.Join(src, fileName(i))
		if err := os.WriteFile(name, []byte(fileName(i)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img := filepath.Join(t.TempDir(), "big.squashfs")
	if out, err := exec.Command(mksquashfs, src, img, "-noappend", "-no-progress").CombinedOutput(); err != nil {
		t.Fatalf("mksquashfs: %v\n%s", err, out)
	}
	fs, err := OpenFile(img)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fs.Close()

	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	if len(entries) != n {
		t.Errorf("ListDir(/) returned %d entries, want %d", len(entries), n)
	}
	// Spot-check that a late entry both lists and reads back correctly.
	want := fileName(n-1) + "\n"
	got, err := fs.ReadFile("/" + fileName(n-1))
	if err != nil || string(got) != want {
		t.Errorf("ReadFile(/%s) = %q, %v; want %q", fileName(n-1), got, err, want)
	}
}

// TestInterop_Compressors masters the same tree with each supported mksquashfs
// compressor and verifies the driver decodes it. A compressor the local
// mksquashfs build lacks is skipped, not failed.
func TestInterop_Compressors(t *testing.T) {
	mksquashfs := findTool("mksquashfs")
	if mksquashfs == "" {
		t.Skip("mksquashfs not available")
	}
	src := t.TempDir()
	small := []byte("compressor parity\n")
	big := pattern(300000) // multi-block + fragment
	if err := os.WriteFile(filepath.Join(src, "small.txt"), small, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, comp := range []string{"gzip", "zstd", "lzo", "xz"} {
		t.Run(comp, func(t *testing.T) {
			img := filepath.Join(t.TempDir(), comp+".squashfs")
			out, err := exec.Command(mksquashfs, src, img, "-noappend", "-no-progress", "-comp", comp).CombinedOutput()
			if err != nil {
				if bytes.Contains(bytes.ToLower(out), []byte("not support")) ||
					bytes.Contains(bytes.ToLower(out), []byte("unrecognised")) ||
					bytes.Contains(bytes.ToLower(out), []byte("invalid compressor")) {
					t.Skipf("mksquashfs lacks %s: %s", comp, out)
				}
				t.Fatalf("mksquashfs -comp %s: %v\n%s", comp, err, out)
			}
			fs, err := OpenFile(img)
			if err != nil {
				t.Fatalf("OpenFile (%s): %v", comp, err)
			}
			defer fs.Close()
			if got, err := fs.ReadFile("/big.bin"); err != nil || !bytes.Equal(got, big) {
				t.Errorf("[%s] ReadFile(/big.bin): err=%v equal=%v", comp, err, bytes.Equal(got, big))
			}
			if got, err := fs.ReadFile("/small.txt"); err != nil || !bytes.Equal(got, small) {
				t.Errorf("[%s] ReadFile(/small.txt): err=%v equal=%v", comp, err, bytes.Equal(got, small))
			}
		})
	}
}

func fileName(i int) string {
	return "f" + string(rune('0'+i/100%10)) + string(rune('0'+i/10%10)) + string(rune('0'+i%10))
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

// TestInterop_MksquashfsRead builds a source tree, packs it with the real
// mksquashfs (default gzip compression, 128 KiB blocks), and verifies the
// driver reads every file, directory and symlink back exactly. Skipped when
// mksquashfs is unavailable.
func TestInterop_MksquashfsRead(t *testing.T) {
	mksquashfs := findTool("mksquashfs")
	if mksquashfs == "" {
		t.Skip("mksquashfs not available — skipping squashfs interop test")
	}

	src := t.TempDir()
	small := []byte("hello squashfs\n")                  // tail-packed into a fragment
	big := pattern(300000)                                // 2 full 128K blocks + fragment tail
	exact := pattern(131072)                              // exactly one full block, no fragment
	nested := []byte("a file in a sub-directory\n")
	files := map[string][]byte{
		"small.txt":      small,
		"big.bin":        big,
		"exact.bin":      exact,
		"empty.txt":      {},
		"sub/nested.txt": nested,
	}
	for name, data := range files {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("small.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	img := filepath.Join(t.TempDir(), "fs.squashfs")
	cmd := exec.Command(mksquashfs, src, img, "-noappend", "-no-progress", "-comp", "gzip")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mksquashfs: %v\n%s", err, out)
	}

	fs, err := OpenFile(img)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fs.Close()

	// File contents.
	for name, want := range files {
		got, err := fs.ReadFile("/" + name)
		if err != nil {
			t.Errorf("ReadFile(/%s): %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("ReadFile(/%s): %d bytes, want %d (equal=%v)", name, len(got), len(want), bytes.Equal(got, want))
		}
	}

	// Symlink: ReadLink target + following it.
	if tgt, err := fs.ReadLink("/link"); err != nil || tgt != "small.txt" {
		t.Errorf("ReadLink(/link) = %q, %v; want %q", tgt, err, "small.txt")
	}
	if got, err := fs.ReadFile("/link"); err != nil || !bytes.Equal(got, small) {
		t.Errorf("ReadFile(/link) follow: %v (equal=%v)", err, bytes.Equal(got, small))
	}

	// Stat.
	st, err := fs.Stat("/big.bin")
	if err != nil {
		t.Errorf("Stat(/big.bin): %v", err)
	} else if st.Size() != uint64(len(big)) {
		t.Errorf("Stat(/big.bin) size = %d, want %d", st.Size(), len(big))
	}

	// Directory listing.
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name()] = true
	}
	for _, name := range []string{"small.txt", "big.bin", "exact.bin", "empty.txt", "sub", "link"} {
		if !seen[name] {
			t.Errorf("ListDir(/) missing %q", name)
		}
	}
	if sub, err := fs.ListDir("/sub"); err != nil || len(sub) != 1 || sub[0].Name() != "nested.txt" {
		t.Errorf("ListDir(/sub): err=%v len=%d; want exactly [nested.txt]", err, len(sub))
	}
}
