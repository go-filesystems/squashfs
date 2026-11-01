// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"

	"github.com/ulikunitz/xz/lzma"
)

// TestLZMA_RoundTrip compresses known bytes with the same library's lzma
// (lzma_alone) writer and round-trips them back through the squashfs legacy
// LZMA (compression id 2) decompressor.
//
// mksquashfs/unsquashfs are not available locally, so this is a self-consistent
// unit test of the framing assumption documented on lzmaDecompressor — it does
// NOT prove byte-for-byte interop with squashfs-tools-produced images.
// xzStored builds a one-block .xz stream with check type "none" whose LZMA2
// payload is one stored chunk, behind the given filter flags (each one a
// filter id, a properties size and the properties; the LZMA2 filter is
// appended). Every number is below 128, so each VLI is one byte.
func xzStored(payload []byte, filters ...[]byte) []byte {
	crc := func(b []byte) []byte { return binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(b)) }
	pad4 := func(b []byte) []byte {
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
		return b
	}
	flags := []byte{0, 0}
	hdr := []byte{0, byte(len(filters))} // size byte (patched), flags: filters-1+1
	for _, f := range filters {
		hdr = append(hdr, f...)
	}
	hdr = append(hdr, 0x21, 1, 0x10) // LZMA2, one property byte, 64 KiB dictionary
	hdr = append(pad4(hdr), 0, 0, 0, 0) // room for the CRC32
	hdr[0] = byte(len(hdr)/4 - 1)
	copy(hdr[len(hdr)-4:], crc(hdr[:len(hdr)-4]))

	data := append([]byte{1, byte((len(payload) - 1) >> 8), byte(len(payload) - 1)}, payload...)
	data = append(data, 0) // end of LZMA2 data
	unpadded := len(hdr) + len(data)

	idx := pad4([]byte{0, 1, byte(unpadded), byte(len(payload))})
	idx = append(idx, crc(idx)...)
	foot := binary.LittleEndian.AppendUint32(nil, uint32(len(idx)/4-1))
	foot = append(foot, flags...)

	s := append([]byte("\xfd7zXZ\x00"), flags...)
	s = append(s, crc(flags)...)
	s = append(s, hdr...)
	s = append(s, pad4(data)...)
	s = append(s, idx...)
	s = append(s, crc(foot)...)
	s = append(s, foot...)
	return append(s, 'Y', 'Z')
}

// The decoder handles the Delta and BCJ filters of an xz block, which the
// previous one refused as unsupported (squashfs-tools' -Xbcj writes them).
func TestXZFilters(t *testing.T) {
	want := []byte("hello, filters")
	for name, f := range map[string][]byte{
		"none":  nil,
		"x86":   {0x04, 0},
		"arm64": {0x0A, 0},
	} {
		var filters [][]byte
		if f != nil {
			filters = append(filters, f)
		}
		out, err := (xzDecompressor{}).decompress(xzStored(want, filters...), 4096)
		if err != nil || !bytes.Equal(out, want) {
			t.Errorf("%s: %q, %v", name, out, err)
		}
	}
	// Delta with distance 1: each output byte is the sum of the stored ones.
	out, err := (xzDecompressor{}).decompress(xzStored([]byte{1, 1, 1, 1}, []byte{0x03, 1, 0}), 4096)
	if err != nil || !bytes.Equal(out, []byte{1, 2, 3, 4}) {
		t.Errorf("delta: %v, %v", out, err)
	}
}

func TestLZMA_RoundTrip(t *testing.T) {
	d, err := newDecompressor(compLZMA)
	if err != nil {
		t.Fatalf("newDecompressor(compLZMA): %v", err)
	}

	inputs := map[string][]byte{
		"empty":      {},
		"short":      []byte("hello squashfs lzma\n"),
		"repetitive": bytes.Repeat([]byte("squashfs-"), 4096),
		"binary":     pattern(70000),
	}

	for name, want := range inputs {
		t.Run(name, func(t *testing.T) {
			// Encode with a known uncompressed size, matching how squashfs's
			// lzma_wrapper writes the true size into the lzma_alone header and
			// emits no end-of-stream marker.
			var buf bytes.Buffer
			cfg := lzma.WriterConfig{Size: int64(len(want)), EOSMarker: false}
			w, err := cfg.NewWriter(&buf)
			if err != nil {
				t.Fatalf("lzma NewWriter: %v", err)
			}
			if _, err := w.Write(want); err != nil {
				t.Fatalf("lzma Write: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("lzma Close: %v", err)
			}

			maxOut := len(want)
			if maxOut < 1 {
				maxOut = 1 // decompress rejects maxOut overruns; empty needs slack
			}
			got, err := d.decompress(buf.Bytes(), maxOut)
			if err != nil {
				t.Fatalf("decompress: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("round-trip mismatch: got %d bytes, want %d", len(got), len(want))
			}
		})
	}
}
