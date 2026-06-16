// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"compress/zlib"
	"fmt"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
)

// Compressor selects the block compressor used by BuildFromDir. It mirrors the
// compression ids stored in the superblock. Only encoders the package can also
// decode are offered; LZO is read-only (no Go encoder available) and LZMA
// (legacy) is not produced.
type Compressor uint16

// Supported write compressors.
const (
	CompressGZIP Compressor = compGZIP // raw zlib stream (mksquashfs default)
	CompressZSTD Compressor = compZSTD
	CompressXZ   Compressor = compXZ
	CompressLZ4  Compressor = compLZ4
)

// compressor encodes a single SquashFS block (metadata, data or fragment) the
// same way the matching decompressor decodes it. The returned bytes are the
// on-disk payload; the caller decides — by comparing lengths — whether to keep
// them or store the block raw.
type compressor interface {
	// id is the superblock compression identifier.
	id() uint16
	// compress encodes src. An error means "store raw" is the only option.
	compress(src []byte) ([]byte, error)
}

// newCompressor returns the encoder for c, or an error for an unknown id.
func newCompressor(c Compressor) (compressor, error) {
	switch c {
	case 0, CompressGZIP:
		return gzipCompressor{}, nil
	case CompressZSTD:
		return zstdCompressor{}, nil
	case CompressXZ:
		return xzCompressor{}, nil
	case CompressLZ4:
		return lz4Compressor{}, nil
	default:
		return nil, fmt.Errorf("%w: write compressor id %d", ErrUnsupportedCompression, uint16(c))
	}
}

// gzipCompressor produces raw zlib streams (RFC 1950), matching the SquashFS
// "gzip" on-disk encoding the reader expects.
type gzipCompressor struct{}

func (gzipCompressor) id() uint16 { return compGZIP }

func (gzipCompressor) compress(src []byte) ([]byte, error) {
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	if _, err := zw.Write(src); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// zstdEncoder is a shared, stateless encoder (safe for concurrent EncodeAll).
var zstdEncoder = func() *zstd.Encoder {
	e, _ := zstd.NewWriter(nil)
	return e
}()

// zstdCompressor produces standard zstd frames.
type zstdCompressor struct{}

func (zstdCompressor) id() uint16 { return compZSTD }

func (zstdCompressor) compress(src []byte) ([]byte, error) {
	return zstdEncoder.EncodeAll(src, make([]byte, 0, len(src))), nil
}

// xzCompressor produces standard .xz streams (LZMA2, no BCJ filters), matching
// mksquashfs's default xz output.
type xzCompressor struct{}

func (xzCompressor) id() uint16 { return compXZ }

func (xzCompressor) compress(src []byte) ([]byte, error) {
	var b bytes.Buffer
	w, err := xz.NewWriter(&b)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(src); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// lz4Compressor produces raw LZ4 block-format output (no frame), matching the
// SquashFS "lz4" on-disk encoding.
type lz4Compressor struct{}

func (lz4Compressor) id() uint16 { return compLZ4 }

func (lz4Compressor) compress(src []byte) ([]byte, error) {
	dst := make([]byte, lz4.CompressBlockBound(len(src)))
	var c lz4.Compressor
	n, err := c.CompressBlock(src, dst)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		// LZ4 reports incompressible input as n==0; signal "store raw".
		return nil, fmt.Errorf("squashfs: lz4 block incompressible")
	}
	return dst[:n], nil
}
