// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	lzo "github.com/anchore/go-lzo"
	"github.com/klauspost/compress/zlib"
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

// xzDecompressor decodes SquashFS "xz" blocks (standard .xz streams / LZMA2,
// without BCJ filters — mksquashfs's default).
type xzDecompressor struct{}

func (xzDecompressor) decompress(src []byte, maxOut int) ([]byte, error) {
	if err := checkXZ(src); err != nil {
		return nil, err
	}
	r, err := xz.ReaderConfig{SingleStream: true}.NewReader(bytes.NewReader(src))
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

// xzMaxDictSize is the largest LZMA2 dictionary an xz block may declare, 16 MiB.
// The xz reader allocates the declared dictionary before decoding anything (a
// 4 GiB claim cost 4 GiB), and raises it to the declared size whatever the
// caller configures, so the claim has to be read out of the block header
// first. A block's output is at most 1 MiB, so a dictionary beyond that is
// dead weight; the xz CLI's presets 0-7 and this package's own writer (8 MiB)
// sit below the limit, and the reference reader sizes the dictionary from the
// image's compressor options (linux fs/squashfs/xz_wrapper.c) rather than from
// the stream.
const xzMaxDictSize = 16 << 20

// checkXZ validates, without allocating, the parts of an .xz stream that
// decide how much memory the reader will reserve: the stream must be a single
// stream with at most one block (a squashfs block is compressed by one call),
// and that block's LZMA2 dictionary must not exceed xzMaxDictSize. Format:
// the .xz file format specification, sections 2.1 (stream header, footer),
// 3.1 (block header) and 4 (index).
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
	if records == 0 {
		return nil
	}
	// Block header: size byte, flags, optional sizes, then the filter list.
	hl := (int(src[12]) + 1) * 4
	if src[12] == 0 || 12+hl > len(src) {
		return bad("block header")
	}
	hdr := src[12 : 12+hl]
	flags := hdr[1]
	pos := 2
	for _, present := range [...]byte{0x40, 0x80} { // compressed / uncompressed size
		if flags&present != 0 {
			_, k := binary.Uvarint(hdr[pos:])
			if k <= 0 {
				return bad("block header sizes")
			}
			pos += k
		}
	}
	for i := 0; i < int(flags&3)+1; i++ {
		id, k := binary.Uvarint(hdr[pos:])
		if k <= 0 {
			return bad("filter id")
		}
		pos += k
		psize, k := binary.Uvarint(hdr[pos:])
		if k <= 0 || psize > uint64(len(hdr)-pos-k) {
			return bad("filter properties")
		}
		pos += k
		if id == 0x21 { // LZMA2
			if psize != 1 {
				return bad("lzma2 properties")
			}
			dict, err := lzma.DecodeDictCap(hdr[pos])
			if err != nil || dict > xzMaxDictSize {
				return bad(fmt.Sprintf("lzma2 dictionary %d bytes exceeds %d", dict, xzMaxDictSize))
			}
		}
		pos += int(psize)
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
	var in io.Reader = bytes.NewReader(src)
	if len(src) >= lzma.HeaderLen {
		// The 4-byte dictionary size in the header is attacker-controlled and
		// the reader allocates a dictionary of that size (up to 2 GiB) before
		// decoding a byte: 13 header bytes cost 1 GiB. A dictionary larger
		// than the block's output is never read past, so clamp the header's
		// value to maxOut (floor: the format's minimum). The decoded bytes
		// are identical.
		var hdr [lzma.HeaderLen]byte
		copy(hdr[:], src)
		lim := uint32(max(maxOut, lzma.MinDictCap))
		if binary.LittleEndian.Uint32(hdr[1:5]) > lim {
			binary.LittleEndian.PutUint32(hdr[1:5], lim)
		}
		in = io.MultiReader(bytes.NewReader(hdr[:]), bytes.NewReader(src[lzma.HeaderLen:]))
	}
	r, err := lzma.NewReader(in)
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
