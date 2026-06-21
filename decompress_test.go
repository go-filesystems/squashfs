// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
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
