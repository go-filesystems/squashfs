// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
)

// defaultBlockSize is the data block size used by Build (matches mksquashfs).
const defaultBlockSize = 131072

// Superblock flag bits relevant to the writer.
const (
	flagUncompressedInodes = 0x0001
	flagUncompressedData   = 0x0002
	flagNoFragments        = 0x0010
	flagNoXattrs           = 0x0200
)

// BuildOptions configures image creation.
type BuildOptions struct {
	// Uncompressed stores metadata and data blocks without compression.
	// When set it overrides Compressor.
	Uncompressed bool

	// Compressor selects the block compressor. The zero value is gzip
	// (zlib), matching mksquashfs's default. Ignored when Uncompressed is set.
	Compressor Compressor

	// NoFragments disables fragment packing, reverting to the legacy behaviour
	// where every regular file is stored as full data blocks. When false
	// (default) the tails of files smaller than one block, and whole sub-block
	// files, are packed into shared fragment blocks to shrink the image.
	NoFragments bool
}

// node is an in-memory tree element used while building an image.
type node struct {
	name     string
	mode     uint16 // full mode (type bits + perms)
	uid, gid uint32
	target   string  // symlink target
	data     []byte  // regular-file contents
	children []*node // directory entries
	// filled in during emit:
	inodeRef uint64
	inodeNum uint32
}

// BuildFromDir creates a SquashFS image at imgPath from the directory tree
// rooted at srcDir. It is the write-side counterpart of the reader: the result
// is a standard SquashFS 4.0 image that unsquashfs (and this package) can read.
func BuildFromDir(imgPath, srcDir string, opts BuildOptions) error {
	root, err := scanDir(srcDir)
	if err != nil {
		return err
	}
	img, err := buildImage(root, opts)
	if err != nil {
		return err
	}
	return os.WriteFile(imgPath, img, 0o644)
}

// scanDir builds a node tree from a host directory.
func scanDir(dir string) (*node, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	root := &node{name: "", mode: sIFDIR | uint16(fi.Mode().Perm())}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		child, err := scanEntry(filepath.Join(dir, e.Name()), e.Name())
		if err != nil {
			return nil, err
		}
		if child != nil {
			root.children = append(root.children, child)
		}
	}
	return root, nil
}

func scanEntry(path, name string) (*node, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	switch {
	case fi.IsDir():
		n := &node{name: name, mode: sIFDIR | uint16(fi.Mode().Perm())}
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			c, err := scanEntry(filepath.Join(path, e.Name()), e.Name())
			if err != nil {
				return nil, err
			}
			if c != nil {
				n.children = append(n.children, c)
			}
		}
		return n, nil
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		return &node{name: name, mode: sIFLNK | 0o777, target: target}, nil
	case fi.Mode().IsRegular():
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return &node{name: name, mode: sIFREG | uint16(fi.Mode().Perm()), data: data}, nil
	default:
		return nil, nil // skip devices/fifos/sockets for now
	}
}

// imageWriter accumulates the on-disk image as it is built.
type imageWriter struct {
	opts      BuildOptions
	comp      compressor   // block encoder (nil when Uncompressed)
	data      bytes.Buffer // whole image; starts with the 96-byte superblock placeholder
	inodes    *metaWriter
	dirs      *metaWriter
	inodeNum  uint32
	blockSize uint32

	// Fragment packing state (used unless opts.NoFragments).
	fragBuf     []byte          // pending uncompressed fragment block payload
	fragEntries []fragmentEntry // emitted fragment blocks (one per table entry)
}

// metaWriter buffers metadata and emits 8 KiB blocks (each: 2-byte header +
// payload). It tracks enough state to hand out inode/dir references.
type metaWriter struct {
	iw  *imageWriter
	out bytes.Buffer // emitted metadata blocks (header + payload)
	buf []byte       // pending uncompressed payload (< metaBlockMax)
}

// ref returns the reference (blockStart<<16 | inBlockOffset) of the next byte.
func (m *metaWriter) ref() uint64 {
	return uint64(m.out.Len())<<16 | uint64(len(m.buf))
}

func (m *metaWriter) write(p []byte) {
	m.buf = append(m.buf, p...)
	for len(m.buf) >= metaBlockMax {
		m.emit(m.buf[:metaBlockMax])
		m.buf = append([]byte(nil), m.buf[metaBlockMax:]...)
	}
}

func (m *metaWriter) finish() {
	if len(m.buf) > 0 {
		m.emit(m.buf)
		m.buf = nil
	}
}

// emit writes one metadata block (2-byte header + payload) to m.out.
func (m *metaWriter) emit(payload []byte) {
	stored, compressed := m.iw.compress(payload, m.iw.opts.Uncompressed)
	hdr := uint16(len(stored))
	if !compressed {
		hdr |= metaHeaderCompressedBit
	}
	var h [2]byte
	binary.LittleEndian.PutUint16(h[:], hdr)
	m.out.Write(h[:])
	m.out.Write(stored)
}

// compress returns the stored bytes and whether they are compressed. It falls
// back to storing raw when compression fails or does not shrink the block
// (matching mksquashfs / what the reader expects).
func (iw *imageWriter) compress(payload []byte, forceRaw bool) (stored []byte, compressed bool) {
	if forceRaw || iw.comp == nil || len(payload) == 0 {
		return payload, false
	}
	out, err := iw.comp.compress(payload)
	if err != nil || len(out) >= len(payload) {
		return payload, false // no gain (or incompressible) — store raw
	}
	return out, true
}

// buildImage lays out and returns a complete SquashFS image for the tree.
func buildImage(root *node, opts BuildOptions) ([]byte, error) {
	iw := &imageWriter{opts: opts, blockSize: defaultBlockSize}
	if !opts.Uncompressed {
		c, err := newCompressor(opts.Compressor)
		if err != nil {
			return nil, err
		}
		iw.comp = c
	}
	iw.inodes = &metaWriter{iw: iw}
	iw.dirs = &metaWriter{iw: iw}

	// Superblock placeholder; data blocks follow immediately.
	iw.data.Write(make([]byte, superblockSize))

	// Emit the tree (post-order: children before their parent directory).
	if err := iw.emitNode(root); err != nil {
		return nil, err
	}
	// Flush any remaining packed fragment data before the metadata tables.
	iw.flushFragment()
	rootRef := root.inodeRef
	inodeCount := iw.inodeNum

	iw.inodes.finish()
	iw.dirs.finish()

	inodeTableStart := uint64(iw.data.Len())
	iw.data.Write(iw.inodes.out.Bytes())
	dirTableStart := uint64(iw.data.Len())
	iw.data.Write(iw.dirs.out.Bytes())

	// Fragment table: one or more metadata blocks of 16-byte entries, then a
	// flat array of u64 metadata-block offsets. Absent when no fragments.
	fragTableStart := bytesUsedNoTable
	if len(iw.fragEntries) > 0 {
		fragTableStart = iw.writeFragmentTable()
	}

	// ID table: one metadata block holding the single id value (0), then a
	// u64 index pointing at that block.
	idValuesAt := uint64(iw.data.Len())
	var idBlock [4]byte // one id = 0
	iw.writeMetaBlock(idBlock[:])
	idTableStart := uint64(iw.data.Len())
	var idPtr [8]byte
	binary.LittleEndian.PutUint64(idPtr[:], idValuesAt)
	iw.data.Write(idPtr[:])

	bytesUsed := uint64(iw.data.Len())

	// Patch the superblock.
	sb := iw.superblock(rootRef, inodeCount, inodeTableStart, dirTableStart,
		idTableStart, fragTableStart, uint32(len(iw.fragEntries)), bytesUsed)
	out := iw.data.Bytes()
	copy(out[:superblockSize], sb)
	return out, nil
}

// bytesUsedNoTable is the FragTableStart value written when there are no
// fragments; mksquashfs points it just past the data, the reader ignores it
// when FragCount is 0.
const bytesUsedNoTable = ^uint64(0)

// writeFragmentTable writes the fragment entries (16-byte records packed into
// metadata blocks) followed by a u64 index of block offsets, and returns the
// index start (the FragTableStart superblock field).
func (iw *imageWriter) writeFragmentTable() uint64 {
	le := binary.LittleEndian
	var blockOffsets []uint64
	for i := 0; i < len(iw.fragEntries); i += fragPerMetaBlock {
		end := i + fragPerMetaBlock
		if end > len(iw.fragEntries) {
			end = len(iw.fragEntries)
		}
		blockOffsets = append(blockOffsets, uint64(iw.data.Len()))
		payload := make([]byte, 0, (end-i)*fragEntrySize)
		for _, fe := range iw.fragEntries[i:end] {
			var rec [fragEntrySize]byte
			le.PutUint64(rec[0:], fe.start)
			le.PutUint32(rec[8:], fe.sizeWord)
			// rec[12:16] unused (0)
			payload = append(payload, rec[:]...)
		}
		iw.writeMetaBlock(payload)
	}
	start := uint64(iw.data.Len())
	for _, off := range blockOffsets {
		var b [8]byte
		le.PutUint64(b[:], off)
		iw.data.Write(b[:])
	}
	return start
}

// writeMetaBlock appends a single standalone metadata block to the image.
func (iw *imageWriter) writeMetaBlock(payload []byte) {
	stored, compressed := iw.compress(payload, iw.opts.Uncompressed)
	hdr := uint16(len(stored))
	if !compressed {
		hdr |= metaHeaderCompressedBit
	}
	var h [2]byte
	binary.LittleEndian.PutUint16(h[:], hdr)
	iw.data.Write(h[:])
	iw.data.Write(stored)
}

// emitNode writes data, directory entries and the inode for n (post-order),
// setting n.inodeRef / n.inodeNum.
func (iw *imageWriter) emitNode(n *node) error {
	switch {
	case n.mode&0xF000 == sIFDIR:
		for _, c := range n.children {
			if err := iw.emitNode(c); err != nil {
				return err
			}
		}
		return iw.emitDirInode(n)
	case n.mode&0xF000 == sIFLNK:
		iw.emitSymlinkInode(n)
		return nil
	default: // regular file
		return iw.emitFileInode(n)
	}
}

func (iw *imageWriter) nextInode() uint32 { iw.inodeNum++; return iw.inodeNum }

// emitFileInode writes a file's data blocks then its basic-file inode. When
// fragment packing is enabled the trailing partial block (size mod blockSize)
// is appended to a shared fragment block instead of a full data block.
func (iw *imageWriter) emitFileInode(n *node) error {
	useFrag := !iw.opts.NoFragments
	bs := int(iw.blockSize)

	// Number of bytes stored as full data blocks. With fragments enabled this
	// is floor(size/blockSize)*blockSize, leaving the remainder for a fragment;
	// otherwise the whole file is full blocks (last one short).
	fullLen := len(n.data)
	if useFrag {
		fullLen = (len(n.data) / bs) * bs
	}

	blocksStart := uint64(iw.data.Len())
	var blockSizes []uint32
	for off := 0; off < fullLen; off += bs {
		end := off + bs
		if end > fullLen {
			end = fullLen
		}
		blockSizes = append(blockSizes, iw.writeDataBlock(n.data[off:end]))
	}

	fragIdx := uint32(invalidFrag)
	fragOffset := uint32(0)
	if useFrag && fullLen < len(n.data) {
		fragIdx, fragOffset = iw.addToFragment(n.data[fullLen:])
	}

	n.inodeNum = iw.nextInode()
	n.inodeRef = iw.inodes.ref()

	le := binary.LittleEndian
	hdr := make([]byte, 16+16)
	le.PutUint16(hdr[0:], inodeBasicFile)
	le.PutUint16(hdr[2:], n.mode&0o7777)
	le.PutUint16(hdr[4:], 0) // uid idx
	le.PutUint16(hdr[6:], 0) // gid idx
	le.PutUint32(hdr[8:], 0) // mtime
	le.PutUint32(hdr[12:], n.inodeNum)
	le.PutUint32(hdr[16:], uint32(blocksStart)) // start_block (32-bit)
	le.PutUint32(hdr[20:], fragIdx)             // fragment table index
	le.PutUint32(hdr[24:], fragOffset)          // offset within fragment block
	le.PutUint32(hdr[28:], uint32(len(n.data))) // file_size
	body := hdr
	for _, s := range blockSizes {
		var b [4]byte
		le.PutUint32(b[:], s)
		body = append(body, b[:]...)
	}
	iw.inodes.write(body)
	return nil
}

// addToFragment appends tail to the current fragment block, flushing first if
// it would overflow blockSize, and returns the (fragmentIndex, offset) for the
// file inode. The fragment index is the entry the block will occupy once
// emitted: flushed blocks become entries in order, so the index equals the
// number of already-emitted entries plus (when the buffer is non-empty) one for
// the block now being accumulated.
func (iw *imageWriter) addToFragment(tail []byte) (idx, offset uint32) {
	if len(iw.fragBuf)+len(tail) > int(iw.blockSize) {
		iw.flushFragment()
	}
	offset = uint32(len(iw.fragBuf))
	idx = uint32(len(iw.fragEntries)) // entry index of the in-progress block
	iw.fragBuf = append(iw.fragBuf, tail...)
	return idx, offset
}

// flushFragment writes the pending fragment block (if any) as one data block
// and records its fragment-table entry.
func (iw *imageWriter) flushFragment() {
	if len(iw.fragBuf) == 0 {
		return
	}
	start := uint64(iw.data.Len())
	sz := iw.writeDataBlock(iw.fragBuf)
	iw.fragEntries = append(iw.fragEntries, fragmentEntry{start: start, sizeWord: sz})
	iw.fragBuf = iw.fragBuf[:0]
}

// writeDataBlock appends one (optionally compressed) data block and returns its
// size word (low 24 bits = on-disk length; bit 24 set = stored uncompressed).
func (iw *imageWriter) writeDataBlock(block []byte) uint32 {
	stored, compressed := iw.compress(block, iw.opts.Uncompressed)
	iw.data.Write(stored)
	sz := uint32(len(stored))
	if !compressed {
		sz |= dataUncompressedBit
	}
	return sz
}

func (iw *imageWriter) emitSymlinkInode(n *node) {
	n.inodeNum = iw.nextInode()
	n.inodeRef = iw.inodes.ref()
	le := binary.LittleEndian
	body := make([]byte, 16+8)
	le.PutUint16(body[0:], inodeBasicSymlink)
	le.PutUint16(body[2:], n.mode&0o7777)
	le.PutUint32(body[12:], n.inodeNum)
	le.PutUint32(body[16:], 1) // nlink
	le.PutUint32(body[20:], uint32(len(n.target)))
	body = append(body, []byte(n.target)...)
	iw.inodes.write(body)
}

// emitDirInode writes n's directory listing into the dir table, then its
// basic-directory inode. Children must already be emitted.
func (iw *imageWriter) emitDirInode(n *node) error {
	sort.Slice(n.children, func(i, j int) bool { return n.children[i].name < n.children[j].name })

	dirStart := iw.dirs.ref()
	dirBlock := uint32(dirStart >> 16)
	dirOffset := uint16(dirStart & 0xFFFF)

	listing := iw.buildDirListing(n)
	iw.dirs.write(listing)

	n.inodeNum = iw.nextInode()
	n.inodeRef = iw.inodes.ref()

	le := binary.LittleEndian
	body := make([]byte, 16+16)
	le.PutUint16(body[0:], inodeBasicDir)
	le.PutUint16(body[2:], n.mode&0o7777)
	le.PutUint32(body[12:], n.inodeNum)
	le.PutUint32(body[16:], dirBlock)                  // start_block (dir table)
	le.PutUint32(body[20:], uint32(len(n.children))+2) // nlink (entries + . + ..)
	le.PutUint16(body[24:], uint16(len(listing)+3))    // file_size (+3 bias)
	le.PutUint16(body[26:], dirOffset)
	le.PutUint32(body[28:], 1) // parent inode (approx; refined for non-root not required by unsquashfs -l)
	iw.inodes.write(body)
	return nil
}

// buildDirListing encodes n's children as SquashFS directory headers+entries.
// All children of one inode-table metadata block share a header (≤256 entries).
func (iw *imageWriter) buildDirListing(n *node) []byte {
	le := binary.LittleEndian
	var out []byte
	i := 0
	for i < len(n.children) {
		// Group entries that share the same inode-table metadata block and fit
		// within 256 per header.
		baseBlock := uint32(n.children[i].inodeRef >> 16)
		baseInode := n.children[i].inodeNum
		j := i
		for j < len(n.children) && j-i < 256 && uint32(n.children[j].inodeRef>>16) == baseBlock {
			j++
		}
		hdr := make([]byte, 12)
		le.PutUint32(hdr[0:], uint32(j-i-1)) // count - 1
		le.PutUint32(hdr[4:], baseBlock)     // start block
		le.PutUint32(hdr[8:], baseInode)     // base inode number
		out = append(out, hdr...)
		for _, c := range n.children[i:j] {
			e := make([]byte, 8)
			le.PutUint16(e[0:], uint16(c.inodeRef&0xFFFF))                         // offset in inode block
			le.PutUint16(e[2:], uint16(int16(int64(c.inodeNum)-int64(baseInode)))) // inode delta
			le.PutUint16(e[4:], dirEntryType(c.mode))                              // type
			le.PutUint16(e[6:], uint16(len(c.name)-1))                             // name length - 1
			e = append(e, []byte(c.name)...)
			out = append(out, e...)
		}
		i = j
	}
	return out
}

// dirEntryType maps a mode to the SquashFS basic inode type stored in dir
// entries.
func dirEntryType(mode uint16) uint16 {
	switch mode & 0xF000 {
	case sIFDIR:
		return inodeBasicDir
	case sIFLNK:
		return inodeBasicSymlink
	default:
		return inodeBasicFile
	}
}

// superblock builds the 96-byte SquashFS 4.0 superblock.
func (iw *imageWriter) superblock(rootRef uint64, inodeCount uint32, inodeStart, dirStart, idStart, fragStart uint64, fragCount uint32, bytesUsed uint64) []byte {
	compID := uint16(compGZIP)
	if iw.comp != nil {
		compID = iw.comp.id()
	}
	le := binary.LittleEndian
	b := make([]byte, superblockSize)
	le.PutUint32(b[0x00:], Magic)
	le.PutUint32(b[0x04:], inodeCount)
	le.PutUint32(b[0x08:], 0) // mod_time
	le.PutUint32(b[0x0C:], iw.blockSize)
	le.PutUint32(b[0x10:], fragCount)
	le.PutUint16(b[0x14:], compID)
	le.PutUint16(b[0x16:], blockLog(iw.blockSize))
	flags := uint16(flagNoXattrs)
	if fragCount == 0 {
		flags |= flagNoFragments
	}
	if iw.opts.Uncompressed {
		flags |= flagUncompressedInodes | flagUncompressedData
	}
	le.PutUint16(b[0x18:], flags)
	le.PutUint16(b[0x1A:], 1) // id count
	le.PutUint16(b[0x1C:], 4) // version major
	le.PutUint16(b[0x1E:], 0) // version minor
	le.PutUint64(b[0x20:], rootRef)
	le.PutUint64(b[0x28:], bytesUsed)
	le.PutUint64(b[0x30:], idStart)
	le.PutUint64(b[0x38:], 0xFFFFFFFFFFFFFFFF) // xattr table: none
	le.PutUint64(b[0x40:], inodeStart)
	le.PutUint64(b[0x48:], dirStart)
	le.PutUint64(b[0x50:], fragStart)          // fragment table (or sentinel when none)
	le.PutUint64(b[0x58:], 0xFFFFFFFFFFFFFFFF) // lookup/export table: none
	return b
}

func blockLog(bs uint32) uint16 {
	l := uint16(0)
	for v := bs; v > 1; v >>= 1 {
		l++
	}
	return l
}
