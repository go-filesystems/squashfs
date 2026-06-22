// Command bench measures go-filesystems/squashfs read throughput.
//
// SquashFS is read-only, so only the "read" subcommand exists. It is part of
// the performance-parity harness (see BENCHMARKS.md) and is a standalone main
// package excluded from the library coverage gate.
//
//	bench read <image>   -- Open image, walk + read every file, print stats.
package main

import (
	"fmt"
	"os"
	"path"
	"sort"
	"time"

	squashfs "github.com/go-filesystems/squashfs"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "read" {
		fail("usage: bench read <image>")
	}
	img := os.Args[2]
	start := time.Now()
	fs, err := squashfs.OpenFile(img)
	if err != nil {
		fail("OpenFile: %v", err)
	}
	defer fs.Close()
	var files, bytesRead int64
	walk(fs, "/", &files, &bytesRead)
	dur := time.Since(start)
	fmt.Printf("READ ok files=%d bytes=%d wall_ns=%d\n", files, bytesRead, dur.Nanoseconds())
}

func walk(fs *squashfs.FS, dir string, files, bytesRead *int64) {
	entries, err := fs.ListDir(dir)
	if err != nil {
		fail("ListDir %q: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if n == "." || n == ".." {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := path.Join(dir, n)
		st, err := fs.Stat(p)
		if err != nil {
			fail("Stat %q: %v", p, err)
		}
		switch st.Mode() & 0xF000 {
		case 0x4000: // S_IFDIR
			walk(fs, p, files, bytesRead)
		case 0x8000: // S_IFREG
			data, err := fs.ReadFile(p)
			if err != nil {
				fail("ReadFile %q: %v", p, err)
			}
			*files++
			*bytesRead += int64(len(data))
		default:
			// symlink / special -- skip payload
		}
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "bench: "+format+"\n", a...)
	os.Exit(1)
}
