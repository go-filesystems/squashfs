// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
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
	case compLZMA, compLZO, compXZ, compLZ4, compZSTD:
		return nil, fmt.Errorf("%w: id %d", ErrUnsupportedCompression, compression)
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
