// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"encoding/binary"
	"fmt"
)

// noXattr marks an inode that carries no extended attributes. It is the value
// stored in an inode's xattr index when none are present.
const noXattr = 0xFFFFFFFF

// Xattr type prefixes. SquashFS stores attribute names without their namespace
// prefix and records the namespace in the entry's type field (low bits).
var xattrPrefixes = map[uint16]string{
	0: "user.",
	1: "trusted.",
	2: "security.",
}

// xattrFlagOOL is set in an entry type when the value is stored out-of-line:
// the inline value bytes are instead an 8-byte reference to an earlier value in
// the key/value metadata region (block<<16 | in-block offset, relative to the
// kv region start).
const xattrFlagOOL = 0x0100

// maxXattrBytes caps the names and values one inode's attribute list may
// return, 16 MiB; Linux itself limits a value to 64 KiB (XATTR_SIZE_MAX) and a
// list to 64 KiB (XATTR_LIST_MAX). It is a variable so tests can lower it.
var maxXattrBytes = 16 << 20

// xattrIDEntry locates one inode's attribute list within the key/value region.
type xattrIDEntry struct {
	ref   uint64 // reference (block<<16 | offset) into the kv region
	count uint32 // number of key/value pairs
	size  uint32 // total uncompressed bytes of this list (unused while reading)
}

const xattrIDEntrySize = 16 // xattr(8) + count(4) + size(4)
const xattrIDPerMetaBlock = metaBlockMax / xattrIDEntrySize

// xattrTable is the parsed xattr id table header plus the absolute file offset
// of the key/value metadata region.
type xattrTable struct {
	kvStart int64  // absolute file offset of the first kv metadata block
	idCount uint32 // number of xattr id entries
	nBlocks uint64 // id-table metadata blocks; their u64 offsets follow the header
}

// hasXattrTable reports whether the image carries an xattr table. An absent
// table is encoded as the all-ones sentinel in XattrTableStart.
func (fs *FS) hasXattrTable() bool {
	return fs.sb.XattrTableStart != 0xFFFFFFFFFFFFFFFF
}

// readXattrTable parses the xattr id table header at XattrTableStart and the
// array of u64 metadata-block pointers that follows it.
//
// Layout (struct squashfs_xattr_id_table): xattr_table_start(u64),
// xattr_ids(u32), unused(u32), then ceil(xattr_ids/512) u64 block pointers.
func (fs *FS) readXattrTable() (*xattrTable, error) {
	if !fs.hasXattrTable() {
		return nil, fmt.Errorf("%w: image has no xattr table", ErrNotFound)
	}
	var hdr [16]byte
	if _, err := fs.rs.ReadAt(hdr[:], int64(fs.sb.XattrTableStart)); err != nil {
		return nil, fmt.Errorf("squashfs: read xattr id table header: %w", err)
	}
	le := binary.LittleEndian
	t := &xattrTable{
		kvStart: int64(le.Uint64(hdr[0:])),
		idCount: le.Uint32(hdr[8:]),
	}
	// idCount is a claim: up to 2^32 entries, i.e. an 8-million-pointer index
	// (64 MiB, twice, once as bytes and once as int64s) that this function used
	// to read whole before any lookup needed it. The pointers are read one at a
	// time, on demand (readXattrIDEntry). The reference reader requires the
	// index to end exactly at bytes_used (linux fs/squashfs/xattr_id.c); the
	// same check, when the sizes are known, rejects the lie up front.
	nBlocks := (uint64(t.idCount) + xattrIDPerMetaBlock - 1) / xattrIDPerMetaBlock
	if fs.size > 0 {
		end := uint64(fs.sb.XattrTableStart) + 16 + nBlocks*8
		if uint64(fs.sb.XattrTableStart) > uint64(fs.size) || end > uint64(fs.size) {
			return nil, fmt.Errorf("%w: xattr id index of %d blocks runs past the image", ErrCorrupt, nBlocks)
		}
	}
	t.nBlocks = nBlocks
	return t, nil
}

// readXattrIDEntry reads xattr id entry idx via the two-level table: a flat
// array of u64 metadata-block offsets, each block holding up to 512 entries.
func (fs *FS) readXattrIDEntry(t *xattrTable, idx uint32) (xattrIDEntry, error) {
	if idx >= t.idCount {
		return xattrIDEntry{}, fmt.Errorf("%w: xattr id %d >= %d", ErrCorrupt, idx, t.idCount)
	}
	blockIdx := uint64(idx) / xattrIDPerMetaBlock
	inBlock := int(idx % xattrIDPerMetaBlock)
	if blockIdx >= t.nBlocks {
		return xattrIDEntry{}, fmt.Errorf("%w: xattr id block %d past index", ErrCorrupt, blockIdx)
	}
	var ptr [8]byte
	if _, err := fs.rs.ReadAt(ptr[:], int64(fs.sb.XattrTableStart)+16+int64(blockIdx)*8); err != nil {
		return xattrIDEntry{}, fmt.Errorf("squashfs: read xattr id index: %w", err)
	}
	block, _, err := fs.readMetaBlockCached(int64(binary.LittleEndian.Uint64(ptr[:])))
	if err != nil {
		return xattrIDEntry{}, err
	}
	o := inBlock * xattrIDEntrySize
	if o+xattrIDEntrySize > len(block) {
		return xattrIDEntry{}, fmt.Errorf("%w: xattr id entry %d past block", ErrCorrupt, idx)
	}
	le := binary.LittleEndian
	return xattrIDEntry{
		ref:   le.Uint64(block[o:]),
		count: le.Uint32(block[o+8:]),
		size:  le.Uint32(block[o+12:]),
	}, nil
}

// readXattrPairs reads the count key/value pairs of one xattr id entry from the
// key/value metadata region, resolving out-of-line values. It returns the
// attributes as fully-qualified names (namespace prefix restored).
func (fs *FS) readXattrPairs(t *xattrTable, e xattrIDEntry) (map[string][]byte, error) {
	blockOff, inBlockOff := inodeRef(e.ref)
	c, err := newMetaCursor(fs, t.kvStart+blockOff, inBlockOff)
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	// e.count is a claim (up to 2^32): sizing the map from it reserved 6.4 GB
	// for a count of 2^26 and aborts the process outright for larger ones.
	// Let the map grow with the pairs that really arrive.
	out := make(map[string][]byte, min(e.count, 64))
	budget := maxXattrBytes // names + values + a per-pair overhead, see below
	for i := uint32(0); i < e.count; i++ {
		// Out-of-line values may all point at one stored value, so a few bytes
		// of list can yield a copy of up to 8 KiB each: the list's output is
		// not bounded by its size. Charge every pair its name, its value and a
		// fixed 8 bytes (the smallest a pair can occupy on disk), so the loop
		// also ends for a count of 2^32 empty pairs.
		if budget -= 8; budget < 0 {
			return nil, fmt.Errorf("%w: xattr list exceeds %d bytes", ErrTooLarge, maxXattrBytes)
		}
		keyHdr, err := c.readN(4) // type(u16), name_size(u16)
		if err != nil {
			return nil, err
		}
		typ := le.Uint16(keyHdr[0:])
		nameSize := le.Uint16(keyHdr[2:])
		if int(nameSize) > metaBlockMax {
			return nil, fmt.Errorf("%w: xattr name size %d", ErrCorrupt, nameSize)
		}
		nameBytes, err := c.readN(int(nameSize))
		if err != nil {
			return nil, err
		}
		prefix, ok := xattrPrefixes[typ&0x00FF]
		if !ok {
			return nil, fmt.Errorf("%w: xattr type %d", ErrCorrupt, typ)
		}
		name := prefix + string(nameBytes)

		valHdr, err := c.readN(4) // vsize(u32)
		if err != nil {
			return nil, err
		}
		vsize := le.Uint32(valHdr)
		if typ&xattrFlagOOL != 0 {
			// Out-of-line: the 8-byte payload is a reference to an earlier
			// value in the kv region. vsize is 8 here.
			if vsize != 8 {
				return nil, fmt.Errorf("%w: xattr OOL value size %d", ErrCorrupt, vsize)
			}
			refBytes, err := c.readN(8)
			if err != nil {
				return nil, err
			}
			val, err := fs.readXattrValueAt(t, le.Uint64(refBytes))
			if err != nil {
				return nil, err
			}
			if budget -= len(name) + len(val); budget < 0 {
				return nil, fmt.Errorf("%w: xattr list exceeds %d bytes", ErrTooLarge, maxXattrBytes)
			}
			out[name] = val
			continue
		}
		if int(vsize) > metaBlockMax {
			return nil, fmt.Errorf("%w: xattr value size %d", ErrCorrupt, vsize)
		}
		val, err := c.readN(int(vsize))
		if err != nil {
			return nil, err
		}
		if budget -= len(name) + len(val); budget < 0 {
			return nil, fmt.Errorf("%w: xattr list exceeds %d bytes", ErrTooLarge, maxXattrBytes)
		}
		out[name] = val
	}
	return out, nil
}

// readXattrValueAt resolves an out-of-line value referenced by ref (block<<16 |
// offset, relative to the kv region start): a vsize(u32) followed by the bytes.
func (fs *FS) readXattrValueAt(t *xattrTable, ref uint64) ([]byte, error) {
	blockOff, inBlockOff := inodeRef(ref)
	c, err := newMetaCursor(fs, t.kvStart+blockOff, inBlockOff)
	if err != nil {
		return nil, err
	}
	hdr, err := c.readN(4)
	if err != nil {
		return nil, err
	}
	vsize := binary.LittleEndian.Uint32(hdr)
	if int(vsize) > metaBlockMax {
		return nil, fmt.Errorf("%w: xattr OOL value size %d", ErrCorrupt, vsize)
	}
	return c.readN(int(vsize))
}
