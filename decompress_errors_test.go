// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ulikunitz/xz/lzma"
)

// TestNewDecompressor_AllIDs verifies newDecompressor resolves every supported
// compression id and rejects an unknown one.
func TestNewDecompressor_AllIDs(t *testing.T) {
	for _, id := range []uint16{compGZIP, compLZMA, compLZO, compXZ, compLZ4, compZSTD} {
		if _, err := newDecompressor(id); err != nil {
			t.Errorf("newDecompressor(%d): unexpected error %v", id, err)
		}
	}
	if _, err := newDecompressor(0xFFFF); !errors.Is(err, ErrUnsupportedCompression) {
		t.Errorf("newDecompressor(unknown): got %v, want ErrUnsupportedCompression", err)
	}
}

// TestDecompressors_RejectGarbage feeds each decompressor an invalid stream and
// expects a graceful error rather than a panic — covering the per-compressor
// error branch.
func TestDecompressors_RejectGarbage(t *testing.T) {
	garbage := bytes.Repeat([]byte{0xFF, 0x00, 0xAB, 0xCD}, 16)
	for _, id := range []uint16{compGZIP, compLZMA, compLZO, compXZ, compLZ4, compZSTD} {
		d, err := newDecompressor(id)
		if err != nil {
			t.Fatalf("newDecompressor(%d): %v", id, err)
		}
		if _, err := d.decompress(garbage, 4096); err == nil {
			t.Errorf("decompressor id %d: expected error on garbage input, got nil", id)
		}
	}
}

// TestDecompress_ExceedsMaxOut covers the "decompressed block exceeds maxOut"
// guard: a valid stream whose output is larger than the caller-supplied ceiling
// must be rejected as corrupt rather than allocating unbounded memory.
func TestDecompress_ExceedsMaxOut(t *testing.T) {
	want := bytes.Repeat([]byte("squashfs-overrun-"), 512)
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
	d, err := newDecompressor(compLZMA)
	if err != nil {
		t.Fatalf("newDecompressor: %v", err)
	}
	// maxOut far below the true size must trip the overrun guard.
	if _, err := d.decompress(buf.Bytes(), 8); !errors.Is(err, ErrCorrupt) {
		t.Errorf("decompress with tiny maxOut: got %v, want ErrCorrupt", err)
	}
}
