// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"

	lzo "github.com/anchore/go-lzo"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// decompressor decompresses a single SquashFS block. Implementations are
// selected from the superblock's compression id.
type decompressor interface {
	decompress(src []byte, maxOut int) ([]byte, error)
}

// newDecompressor returns the decompressor for the given compression id.
// Only gzip (zlib stream) is implemented so far — it is mksquashfs's default;
// the other compressors return ErrUnsupportedCompression until added.
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

// xzDecompressor decodes SquashFS "xz" blocks (standard .xz streams / LZMA2,
// without BCJ filters — mksquashfs's default).
type xzDecompressor struct{}

func (xzDecompressor) decompress(src []byte, maxOut int) ([]byte, error) {
	r, err := xz.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("squashfs: xz: %w", err)
	}
	out, err := io.ReadAll(io.LimitReader(r, int64(maxOut)+1))
	if err != nil {
		return nil, fmt.Errorf("squashfs: xz read: %w", err)
	}
	if len(out) > maxOut {
		return nil, fmt.Errorf("%w: decompressed block exceeds %d bytes", ErrCorrupt, maxOut)
	}
	return out, nil
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
	r, err := lzma.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("squashfs: lzma: %w", err)
	}
	out, err := io.ReadAll(io.LimitReader(r, int64(maxOut)+1))
	if err != nil {
		return nil, fmt.Errorf("squashfs: lzma read: %w", err)
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
var zstdDecoder = func() *zstd.Decoder {
	d, _ := zstd.NewReader(nil)
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
