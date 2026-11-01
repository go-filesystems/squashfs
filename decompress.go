// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	lzo "github.com/anchore/go-lzo"
	"github.com/go-compressions/xz"
	"github.com/go-compressions/xz/lzma"
	"github.com/klauspost/compress/zlib"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// decompressor decompresses a single SquashFS block. Implementations are
// selected from the superblock's compression id.
type decompressor interface {
	decompress(src []byte, maxOut int) ([]byte, error)
}

// newDecompressor returns the decompressor for the given compression id.
// gzip (zlib, mksquashfs's default), xz, lzo, zstd, lz4 and legacy lzma are
// all decoded; any other compression id returns ErrUnsupportedCompression.
func newDecompressor(compression uint16) (decompressor, error) {
	switch compression {
	case compGZIP:
		return gzipDecompressor{}, nil
	case compXZ:
		return xzDecompressor{}, nil
	case compLZO:
		return lzoDecompressor{}, nil
	case compZSTD:
		return zstdDecompressor{}, nil
	case compLZ4:
		return lz4Decompressor{}, nil
	case compLZMA:
		return lzmaDecompressor{}, nil
	default:
		return nil, fmt.Errorf("%w: id %d", ErrUnsupportedCompression, compression)
	}
}

// gzipDecompressor decodes SquashFS "gzip" blocks, which are raw zlib streams
// (RFC 1950), not gzip-framed (RFC 1952).
type gzipDecompressor struct{}

func (gzipDecompressor) decompress(src []byte, maxOut int) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("squashfs: zlib: %w", err)
	}
	defer zr.Close()
	// maxOut bounds the output (block_size for data, 8 KiB for metadata) so a
	// corrupt stream can't drive an unbounded allocation. +1 detects overrun.
	out, err := io.ReadAll(io.LimitReader(zr, int64(maxOut)+1))
	if err != nil {
		return nil, fmt.Errorf("squashfs: zlib read: %w", err)
	}
	if len(out) > maxOut {
		return nil, fmt.Errorf("%w: decompressed block exceeds %d bytes", ErrCorrupt, maxOut)
	}
	return out, nil
}

// xzDecompressor decodes SquashFS "xz" blocks: standard .xz streams (LZMA2,
// with or without the Delta and BCJ filters).
type xzDecompressor struct{}

func (xzDecompressor) decompress(src []byte, maxOut int) ([]byte, error) {
	if err := checkXZ(src); err != nil {
		return nil, err
	}
	r, err := xz.ReaderConfig{SingleStream: true, MaxDictionary: dictCeiling(maxOut)}.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, decodeErr("xz", err, maxOut)
	}
	out, err := io.ReadAll(io.LimitReader(r, int64(maxOut)+1))
	if err != nil {
		return nil, decodeErr("xz read", err, maxOut)
	}
	if len(out) > maxOut {
		return nil, fmt.Errorf("%w: decompressed block exceeds %d bytes", ErrCorrupt, maxOut)
	}
	return out, nil
}

// dictCeiling is the MaxDictionary handed to the xz and lzma readers: the
// block's output limit, floored at the format's smallest dictionary (and so
// never zero, which the readers read as their 256 MiB default). A dictionary
// larger than the output is never read past, and the readers grow their window
// with the data they produce and refuse (ErrTooLarge) to grow it beyond this
// figure, so what a block header declares (up to 4 GiB) decides nothing: the
// memory a block can cost is bounded by maxOut whatever its header says.
func dictCeiling(maxOut int) int { return max(maxOut, lzma.MinDictCap) }

// decodeErr wraps an xz or lzma reader error. ErrTooLarge from the readers
// means the block produced more than the ceiling dictCeiling gave them, that
// is, more than maxOut bytes: the same refusal, with the same ErrCorrupt, as
// the output-size check of every other decompressor here.
func decodeErr(what string, err error, maxOut int) error {
	if errors.Is(err, xz.ErrTooLarge) {
		return fmt.Errorf("%w: %s: decompressed block exceeds %d bytes: %w", ErrCorrupt, what, maxOut, err)
	}
	return fmt.Errorf("squashfs: %s: %w", what, err)
}

// checkXZ validates, without allocating, the shape of an .xz stream: a single
// stream with at most one block (a squashfs block is compressed by one call;
// anything else is not a squashfs block). It does not look at the dictionary
// the block header declares: the reader does not allocate it (see
// dictCeiling). Format: the .xz file format specification, sections 2.1
// (stream header, footer), 3.1 (block header) and 4 (index).
func checkXZ(src []byte) error {
	bad := func(what string) error { return fmt.Errorf("%w: xz: %s", ErrCorrupt, what) }
	// stream header (12) + footer (12) is the least a stream can be.
	if len(src) < 24 || string(src[len(src)-2:]) != "YZ" {
		return bad("not a single complete stream")
	}
	// Index: backward size in the footer gives its length; it holds the
	// record count (one record per block).
	idxSize := (uint64(binary.LittleEndian.Uint32(src[len(src)-8:])) + 1) * 4
	if idxSize > uint64(len(src)-24) {
		return bad("index does not fit")
	}
	idx := src[len(src)-12-int(idxSize) : len(src)-12]
	records, n := binary.Uvarint(idx[1:])
	if idx[0] != 0 || n <= 0 || records > 1 {
		return bad("more than one block")
	}
	return nil
}

// lzmaDecompressor decodes SquashFS legacy "lzma" blocks (compression id 2).
//
// Framing assumption: each block is a self-contained lzma_alone stream (the
// classic .lzma container squashfs-tools' lzma_wrapper.c produces) — a 13-byte
// header (1-byte properties, 4-byte little-endian dictionary size, 8-byte
// little-endian uncompressed size) followed by the raw LZMA1 stream. squashfs
// writes the true uncompressed size into the header rather than the 0xFFFF…
// "unknown size" sentinel and emits no end-of-stream marker, so the decoder
// stops exactly at that length. lzma.NewReader parses this header directly.
type lzmaDecompressor struct{}

func (lzmaDecompressor) decompress(src []byte, maxOut int) ([]byte, error) {
	// The 4-byte dictionary size in the header is attacker-controlled; the
	// reader does not allocate it, its window follows the data and never
	// exceeds dictCeiling(maxOut).
	r, err := lzma.ReaderConfig{MaxDictionary: dictCeiling(maxOut)}.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, decodeErr("lzma", err, maxOut)
	}
	out, err := io.ReadAll(io.LimitReader(r, int64(maxOut)+1))
	if err != nil {
		return nil, decodeErr("lzma read", err, maxOut)
	}
	if len(out) > maxOut {
		return nil, fmt.Errorf("%w: decompressed block exceeds %d bytes", ErrCorrupt, maxOut)
	}
	return out, nil
}

// lzoDecompressor decodes SquashFS "lzo" blocks (raw LZO1X).
type lzoDecompressor struct{}

func (lzoDecompressor) decompress(src []byte, maxOut int) ([]byte, error) {
	dst := make([]byte, maxOut)
	n, err := lzo.Decompress(src, dst)
	if err != nil {
		return nil, fmt.Errorf("squashfs: lzo: %w", err)
	}
	return dst[:n], nil
}

// lz4Decompressor decodes SquashFS "lz4" blocks (raw LZ4 block format).
type lz4Decompressor struct{}

func (lz4Decompressor) decompress(src []byte, maxOut int) ([]byte, error) {
	dst := make([]byte, maxOut)
	n, err := lz4.UncompressBlock(src, dst)
	if err != nil {
		return nil, fmt.Errorf("squashfs: lz4: %w", err)
	}
	return dst[:n], nil
}

// zstdDecoder is a shared, stateless decoder (safe for concurrent DecodeAll).
//
// It is capped at maxBlockSize, the largest block the format allows. Without
// the cap the library's default is 64 GiB: DecodeAll then inflates the whole
// frame and only afterwards does the maxOut check below run, so a 7 KiB block
// of zero runs (an RLE block costs 3 bytes per 128 KiB) cost 64 MiB of output
// before being refused. With it, the frame header's content size is checked
// first and the block loop stops at the cap.
var zstdDecoder = func() *zstd.Decoder {
	d, _ := zstd.NewReader(nil,
		zstd.WithDecoderMaxMemory(maxBlockSize),
		zstd.WithDecoderMaxWindow(maxBlockSize))
	return d
}()

// zstdDecompressor decodes SquashFS "zstd" blocks (standard zstd frames).
type zstdDecompressor struct{}

func (zstdDecompressor) decompress(src []byte, maxOut int) ([]byte, error) {
	out, err := zstdDecoder.DecodeAll(src, make([]byte, 0, maxOut))
	if err != nil {
		return nil, fmt.Errorf("squashfs: zstd: %w", err)
	}
	if len(out) > maxOut {
		return nil, fmt.Errorf("%w: decompressed block exceeds %d bytes", ErrCorrupt, maxOut)
	}
	return out, nil
}
