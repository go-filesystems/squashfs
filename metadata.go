// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"encoding/binary"
	"fmt"
	"io"
)

// metaBlockMax is the maximum size of an uncompressed metadata block.
const metaBlockMax = 8192

// metaHeaderCompressedBit, when CLEAR in the 16-bit metadata block header,
// means the block payload is compressed; when SET, the payload is stored raw.
const metaHeaderCompressedBit = 0x8000

// readMetaBlockCached returns a decompressed metadata block at absolute file
// offset at, serving it from the per-open cache when possible. Because the image
// is read-only the block at a given offset is immutable, so the cache never
// needs invalidation. The returned slice is the cached backing array and must
// not be mutated (all callers copy out via metaCursor or by slicing reads).
func (fs *FS) readMetaBlockCached(at int64) (data []byte, next int64, err error) {
	if e, ok := fs.cache.getMeta(at); ok {
		return e.data, e.next, nil
	}
	data, next, err = readMetaBlock(fs.rs, fs.d, at)
	if err != nil {
		return nil, 0, err
	}
	fs.cache.putMeta(at, metaEntry{data: data, next: next})
	return data, next, nil
}

// readMetaBlock reads one metadata block starting at absolute file offset at.
// It returns the (decompressed) payload and the file offset of the next block.
func readMetaBlock(rs io.ReaderAt, d decompressor, at int64) (data []byte, next int64, err error) {
	var hdr [2]byte
	if _, err = rs.ReadAt(hdr[:], at); err != nil {
		return nil, 0, fmt.Errorf("squashfs: read meta header @%d: %w", at, err)
	}
	h := binary.LittleEndian.Uint16(hdr[:])
	stored := h &^ metaHeaderCompressedBit // low 15 bits = on-disk payload size
	if stored == 0 || int(stored) > metaBlockMax {
		return nil, 0, fmt.Errorf("%w: meta block size %d @%d", ErrCorrupt, stored, at)
	}
	raw := make([]byte, stored)
	if _, err = rs.ReadAt(raw, at+2); err != nil {
		return nil, 0, fmt.Errorf("squashfs: read meta payload @%d: %w", at+2, err)
	}
	next = at + 2 + int64(stored)
	if h&metaHeaderCompressedBit != 0 {
		return raw, next, nil // stored uncompressed
	}
	data, err = d.decompress(raw, metaBlockMax)
	if err != nil {
		return nil, 0, err
	}
	return data, next, nil
}

// metaCursor streams bytes out of the metadata-block region, transparently
// crossing block boundaries. SquashFS inode and directory structures are
// addressed by a reference: the byte offset of a metadata block (relative to a
// table start) plus a byte offset inside that block's decompressed payload.
type metaCursor struct {
	fs   *FS
	next int64  // absolute file offset of the next block to load
	cur  []byte // current decompressed block (cache-owned: read-only)
	pos  int    // read position within cur
}

// newMetaCursor positions a cursor at inBlockOff bytes into the metadata block
// located at absolute file offset blockStart.
func newMetaCursor(fs *FS, blockStart int64, inBlockOff int) (*metaCursor, error) {
	c := &metaCursor{fs: fs, next: blockStart}
	if err := c.load(); err != nil {
		return nil, err
	}
	if inBlockOff > len(c.cur) {
		return nil, fmt.Errorf("%w: in-block offset %d > block %d", ErrCorrupt, inBlockOff, len(c.cur))
	}
	c.pos = inBlockOff
	return c, nil
}

func (c *metaCursor) load() error {
	data, next, err := c.fs.readMetaBlockCached(c.next)
	if err != nil {
		return err
	}
	c.cur = data
	c.next = next
	c.pos = 0
	return nil
}

// read fills p, advancing across metadata blocks as needed.
func (c *metaCursor) read(p []byte) error {
	for len(p) > 0 {
		if c.pos >= len(c.cur) {
			if err := c.load(); err != nil {
				return err
			}
			if len(c.cur) == 0 {
				return io.ErrUnexpectedEOF
			}
		}
		n := copy(p, c.cur[c.pos:])
		c.pos += n
		p = p[n:]
	}
	return nil
}

// readN returns the next n bytes as a fresh slice.
func (c *metaCursor) readN(n int) ([]byte, error) {
	b := make([]byte, n)
	if err := c.read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// inodeRef splits a 48-bit inode/dir reference into the metadata block offset
// (relative to a table start) and the offset within that block.
func inodeRef(ref uint64) (blockOff int64, inBlockOff int) {
	return int64(ref >> 16), int(ref & 0xFFFF)
}
