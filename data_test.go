// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import "testing"

// TestBlockOnDiskBytesWidth pins the width at which a data-block size word is
// masked, which is the whole substance of the ppc64le defect blockOnDiskBytes
// documents: the low 24 bits are the on-disk length, bit 24 is the
// "uncompressed" flag, and clearing that flag must leave a value whose HIGH 32
// bits are zero.
//
// Why not an end-to-end read test instead? Because an end-to-end read cannot
// see this, on any machine a contributor is likely to have:
//
//   - It is architecture-specific. On amd64 and arm64 — the two native CI legs
//     — the 32-bit form is correct, so a round-trip read passes there forever.
//   - On ppc64le it was register-allocation-dependent. The wrapped rlwinm did
//     corrupt the high word, but the compiler happened to spill the result with
//     a 32-bit MOVW and reload it with a zero-extending MOVWZ, throwing the
//     corruption away. An unrelated edit that keeps the value in a register
//     brings the bug back with no change to this file.
//   - Even on ppc64le it needs a fixture with two or more non-sparse full data
//     blocks, because only the SECOND block's offset is computed from a
//     corrupted advance. Small fixtures whose whole content lives in a tail
//     fragment never advance the offset at all.
//
// So the property is asserted directly, on the helper, over the exact bit
// patterns that trigger it. This test is architecture-independent by
// construction: it states what the value must be, not what one compiler does
// with it.
func TestBlockOnDiskBytesWidth(t *testing.T) {
	cases := []struct {
		name string
		word uint32
		want uint64
	}{
		{"sparse", 0, 0},
		{"flag only, zero length", dataUncompressedBit, 0},
		{"compressed length", 0x000003BE, 958},
		// The exact word from the field report: an uncompressed 958-byte
		// block. Under the wrapped mask its offset advance came out as
		// 862<<32|958 rather than 958.
		{"uncompressed length", dataUncompressedBit | 0x000003BE, 958},
		{"max 24-bit length", 0x00FFFFFF, 0xFFFFFF},
		{"max 24-bit length, uncompressed", 0x01FFFFFF, 0xFFFFFF},
		// Bits above 24 are not defined by the format, so a corrupt or hostile
		// image can set them. They must be carried through untouched and, above
		// all, must not leak into the high word: only bit 24 is cleared.
		{"all bits set", 0xFFFFFFFF, 0xFEFFFFFF},
		{"high bits set, flag clear", 0xFE000000, 0xFE000000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := blockOnDiskBytes(tc.word)
			if uint64(got) != tc.want {
				t.Fatalf("blockOnDiskBytes(%#08x) = %#x, want %#x", tc.word, uint64(got), tc.want)
			}
			// The load-bearing assertion. A wrapped rlwinm (MB=8 > ME=6)
			// duplicates the 32-bit value into both halves of the register;
			// anything that reintroduces a 32-bit mask and widens it without a
			// fresh zero-extension shows up here as a non-zero high word.
			if hi := uint64(got) >> 32; hi != 0 {
				t.Fatalf("blockOnDiskBytes(%#08x) high word = %#x, want 0 "+
					"(the value was duplicated into the high half)", tc.word, hi)
			}
			// A length is never negative, whatever the word contained.
			if got < 0 {
				t.Fatalf("blockOnDiskBytes(%#08x) = %d, want >= 0", tc.word, got)
			}
		})
	}
}

// TestBlockOnDiskSize checks that the pair-returning form agrees with
// blockOnDiskBytes on the length and reports the flag the right way round:
// bit 24 SET means the block is stored UNCOMPRESSED.
func TestBlockOnDiskSize(t *testing.T) {
	cases := []struct {
		word           uint32
		wantN          int64
		wantCompressed bool
	}{
		{0, 0, true},
		{0x000003BE, 958, true},
		{dataUncompressedBit | 0x000003BE, 958, false},
		{dataUncompressedBit, 0, false},
		{0xFFFFFFFF, 0xFEFFFFFF, false},
	}
	for _, tc := range cases {
		n, compressed := blockOnDiskSize(tc.word)
		if n != tc.wantN || compressed != tc.wantCompressed {
			t.Errorf("blockOnDiskSize(%#08x) = (%d, %v), want (%d, %v)",
				tc.word, n, compressed, tc.wantN, tc.wantCompressed)
		}
		if n != blockOnDiskBytes(tc.word) {
			t.Errorf("blockOnDiskSize(%#08x) length %d disagrees with blockOnDiskBytes %d",
				tc.word, n, blockOnDiskBytes(tc.word))
		}
	}
}

// TestReadFileMultiBlockOffsetAdvance walks a file long enough to need several
// full data blocks, so readFile's `off += n` advance actually runs more than
// once, and compares it byte for byte with the same content read through
// OpenFile/ReadAt, which computes its block offsets with the same helper. It
// does not, on its own, prove anything about instruction selection — see the
// comment on TestBlockOnDiskBytesWidth — but it is the end-to-end shape that
// WOULD have failed on the ppc64le CI leg, and it keeps the multi-block path
// exercised.
func TestReadFileMultiBlockOffsetAdvance(t *testing.T) {
	fs := openImage(t)
	defer fs.Close()

	const name = "/big.bin" // 300000 bytes: two full 128 KiB blocks plus a tail
	want, err := fs.ReadFile(name)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", name, err)
	}
	if int64(len(want)) <= int64(fs.sb.BlockSize) {
		t.Fatalf("%s is %d bytes, not multi-block at block size %d — the fixture "+
			"no longer exercises the offset advance", name, len(want), fs.sb.BlockSize)
	}

	f, err := fs.OpenFile(name)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", name, err)
	}
	defer f.Close()
	got := make([]byte, len(want))
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: byte %d differs: ReadAt %#x, ReadFile %#x", name, i, got[i], want[i])
		}
	}
}
