// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

// metaBlockBytes wraps payload as a single uncompressed SquashFS metadata block
// (2-byte header with the "uncompressed" bit set, then the raw payload).
func metaBlockBytes(payload []byte) []byte {
	var h [2]byte
	binary.LittleEndian.PutUint16(h[:], uint16(len(payload))|metaHeaderCompressedBit)
	return append(h[:], payload...)
}

// TestXattr_ReadBack hand-crafts an xattr key/value region, id table and id
// table header in memory, then exercises the table readers end to end —
// including a fully-qualified name prefix and an out-of-line (OOL) value that
// references an earlier inline value.
//
// mksquashfs/unsquashfs are not available locally, so this is a self-consistent
// unit test built from the on-disk layout documented in xattr.go; it does NOT
// prove byte-for-byte interop with squashfs-tools-produced images.
func TestXattr_ReadBack(t *testing.T) {
	le := binary.LittleEndian

	// --- Key/value region (one metadata block) ---
	// Entry 0 of id 0: inline "user.comment" = "hi".
	// Entry 1 of id 0: OOL "user.alias" -> references the value of entry 0.
	// Id 1: inline "security.selinux" = "ctx".
	var kv []byte

	// id 0, pair 0: inline. This value sits at kv-region offset valOff0.
	kv = appendXattrKey(kv, 0, "comment") // user.
	valOff0 := len(kv)                    // offset of this value record (vsize+bytes)
	kv = appendXattrInlineVal(kv, []byte("hi"))

	// id 0, pair 1: OOL, value reference points at valOff0.
	kv = appendXattrKey(kv, 0|xattrFlagOOL, "alias")
	kv = appendXattrOOLVal(kv, uint64(valOff0)) // ref = block 0 << 16 | valOff0

	id0Count := uint32(2)
	id0Size := uint32(len(kv)) // bytes consumed by id 0's pairs

	// id 1, pair 0: inline.
	id1Off := len(kv)
	kv = appendXattrKey(kv, 2, "selinux") // security.
	kv = appendXattrInlineVal(kv, []byte("ctx"))
	id1Count := uint32(1)
	id1Size := uint32(len(kv) - id1Off)

	// --- Assemble the image buffer ---
	var img bytes.Buffer
	// Leave room for a 96-byte superblock so absolute offsets are realistic.
	img.Write(make([]byte, superblockSize))

	kvStart := int64(img.Len())
	img.Write(metaBlockBytes(kv))

	// id table: two 16-byte entries in one metadata block.
	var idTbl []byte
	idTbl = appendXattrID(idTbl, 0<<16|0, id0Count, id0Size)        // id 0 ref -> kv offset 0
	idTbl = appendXattrID(idTbl, uint64(id1Off), id1Count, id1Size) // id 1 ref -> kv offset id1Off
	idBlockAt := int64(img.Len())
	img.Write(metaBlockBytes(idTbl))

	// id table header: xattr_table_start(=kvStart), xattr_ids(=2), unused, then
	// one u64 pointer to the id-table metadata block.
	xattrTableStart := int64(img.Len())
	var hdr [16]byte
	le.PutUint64(hdr[0:], uint64(kvStart))
	le.PutUint32(hdr[8:], 2) // two ids
	img.Write(hdr[:])
	var ptr [8]byte
	le.PutUint64(ptr[:], uint64(idBlockAt))
	img.Write(ptr[:])

	// Minimal FS with an uncompressed-metadata-friendly decompressor (gzip is
	// fine; the blocks are stored uncompressed so it is never invoked).
	d, _ := newDecompressor(compGZIP)
	fs := &FS{
		rs: bytes.NewReader(img.Bytes()),
		sb: &Superblock{XattrTableStart: uint64(xattrTableStart)},
		d:  d,
	}

	tbl, err := fs.readXattrTable()
	if err != nil {
		t.Fatalf("readXattrTable: %v", err)
	}
	if tbl.idCount != 2 || tbl.kvStart != kvStart {
		t.Fatalf("table = %+v; want idCount=2 kvStart=%d", tbl, kvStart)
	}

	// id 0: inline + OOL.
	e0, err := fs.readXattrIDEntry(tbl, 0)
	if err != nil {
		t.Fatalf("readXattrIDEntry(0): %v", err)
	}
	got0, err := fs.readXattrPairs(tbl, e0)
	if err != nil {
		t.Fatalf("readXattrPairs(0): %v", err)
	}
	want0 := map[string][]byte{
		"user.comment": []byte("hi"),
		"user.alias":   []byte("hi"), // OOL points at user.comment's value
	}
	if !reflect.DeepEqual(got0, want0) {
		t.Errorf("id0 = %v; want %v", got0, want0)
	}

	// id 1: inline, different namespace.
	e1, err := fs.readXattrIDEntry(tbl, 1)
	if err != nil {
		t.Fatalf("readXattrIDEntry(1): %v", err)
	}
	got1, err := fs.readXattrPairs(tbl, e1)
	if err != nil {
		t.Fatalf("readXattrPairs(1): %v", err)
	}
	want1 := map[string][]byte{"security.selinux": []byte("ctx")}
	if !reflect.DeepEqual(got1, want1) {
		t.Errorf("id1 = %v; want %v", got1, want1)
	}

	// Out-of-range id is rejected.
	if _, err := fs.readXattrIDEntry(tbl, 2); err == nil {
		t.Errorf("readXattrIDEntry(2) = nil error; want corrupt")
	}

	// inodeXattrs wires an inode's xattr index through the table. id 1 here.
	got, err := fs.inodeXattrs(&inode{xattrIdx: 1})
	if err != nil {
		t.Fatalf("inodeXattrs(idx=1): %v", err)
	}
	if !reflect.DeepEqual(got, want1) {
		t.Errorf("inodeXattrs(idx=1) = %v; want %v", got, want1)
	}
	// An inode with no xattr index short-circuits to (nil, nil).
	if got, err := fs.inodeXattrs(&inode{xattrIdx: noXattr}); err != nil || got != nil {
		t.Errorf("inodeXattrs(noXattr) = %v, %v; want nil, nil", got, err)
	}
}

// TestXattr_NoTable verifies that images without an xattr table report no
// attributes rather than erroring.
func TestXattr_NoTable(t *testing.T) {
	fs := &FS{sb: &Superblock{XattrTableStart: 0xFFFFFFFFFFFFFFFF}}
	if fs.hasXattrTable() {
		t.Fatal("hasXattrTable = true for sentinel start")
	}
	got, err := fs.inodeXattrs(&inode{xattrIdx: 5})
	if err != nil || got != nil {
		t.Errorf("inodeXattrs(no table) = %v, %v; want nil, nil", got, err)
	}
}

func appendXattrKey(b []byte, typ uint16, name string) []byte {
	var h [4]byte
	binary.LittleEndian.PutUint16(h[0:], typ)
	binary.LittleEndian.PutUint16(h[2:], uint16(len(name)))
	b = append(b, h[:]...)
	return append(b, name...)
}

func appendXattrInlineVal(b []byte, val []byte) []byte {
	var h [4]byte
	binary.LittleEndian.PutUint32(h[:], uint32(len(val)))
	b = append(b, h[:]...)
	return append(b, val...)
}

func appendXattrOOLVal(b []byte, ref uint64) []byte {
	var h [4]byte
	binary.LittleEndian.PutUint32(h[:], 8) // OOL value size is the 8-byte ref
	b = append(b, h[:]...)
	var r [8]byte
	binary.LittleEndian.PutUint64(r[:], ref)
	return append(b, r[:]...)
}

func appendXattrID(b []byte, ref uint64, count, size uint32) []byte {
	var e [16]byte
	binary.LittleEndian.PutUint64(e[0:], ref)
	binary.LittleEndian.PutUint32(e[8:], count)
	binary.LittleEndian.PutUint32(e[12:], size)
	return append(b, e[:]...)
}
