// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import "testing"

// TestSequentialSmallReadAllocs: once a block is decoded, the 4 KiB sequential
// reads inside it allocate nothing, with read-ahead on or off.
func TestSequentialSmallReadAllocs(t *testing.T) {
	body := parBody()
	img := parImage(t, CompressLZ4, false, body)
	for _, w := range []int{1, 4} {
		fs := openPar(t, img, w)
		f, _ := fs.OpenFile("/big.bin")
		buf := make([]byte, 4096)
		// Decode the whole window first, then measure reads inside block 0..3.
		for off := int64(0); off < 4*defaultBlockSize; off += 4096 {
			f.ReadAt(buf, off)
		}
		off := int64(0)
		a := testing.AllocsPerRun(31, func() {
			f.ReadAt(buf, off)
			off += 4096
		})
		if a != 0 {
			t.Fatalf("workers=%d: %.2f allocs per cached sequential ReadAt", w, a)
		}
	}
}
