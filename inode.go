// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"encoding/binary"
	"fmt"
)

// SquashFS inode types.
const (
	inodeBasicDir     = 1
	inodeBasicFile    = 2
	inodeBasicSymlink = 3
	inodeBasicBlkdev  = 4
	inodeBasicChrdev  = 5
	inodeBasicFifo    = 6
	inodeBasicSocket  = 7
	inodeExtDir       = 8
	inodeExtFile      = 9
	inodeExtSymlink   = 10
	inodeExtBlkdev    = 11
	inodeExtChrdev    = 12
	inodeExtFifo      = 13
	inodeExtSocket    = 14
)

// Unix mode type bits.
const (
	sIFIFO  = 0x1000
	sIFCHR  = 0x2000
	sIFDIR  = 0x4000
	sIFBLK  = 0x6000
	sIFREG  = 0x8000
	sIFLNK  = 0xA000
	sIFSOCK = 0xC000
)

// invalidFrag marks a regular-file inode with no tail-end fragment.
const invalidFrag = 0xFFFFFFFF

// inode is the decoded view of a SquashFS inode that the reader needs.
type inode struct {
	Type   uint16 // raw SquashFS inode type (1..14)
	Mode   uint16 // full mode: permission bits | Unix type bits
	UID    uint16 // index into the id table
	GID    uint16
	Mtime  uint32
	Number uint32
	Size   uint64 // regular-file size, or directory listing data size

	// Extended inodes carry an index into the xattr id table; basic inodes
	// have none, recorded as noXattr.
	xattrIdx uint32

	// Regular file.
	blocksStart uint64
	fragIdx     uint32
	fragOffset  uint32
	blockSizes  []uint32

	// Directory.
	dirStartBlock uint32 // metadata block offset, relative to dir table start
	dirOffset     uint16 // offset within that block

	// Symlink.
	symlinkTarget string
}

func (in *inode) isDir() bool     { return in.Type == inodeBasicDir || in.Type == inodeExtDir }
func (in *inode) isRegular() bool { return in.Type == inodeBasicFile || in.Type == inodeExtFile }
func (in *inode) isSymlink() bool { return in.Type == inodeBasicSymlink || in.Type == inodeExtSymlink }

// typeBits maps a SquashFS inode type to its Unix mode type bits.
func typeBits(t uint16) uint16 {
	switch t {
	case inodeBasicDir, inodeExtDir:
		return sIFDIR
	case inodeBasicFile, inodeExtFile:
		return sIFREG
	case inodeBasicSymlink, inodeExtSymlink:
		return sIFLNK
	case inodeBasicChrdev, inodeExtChrdev:
		return sIFCHR
	case inodeBasicBlkdev, inodeExtBlkdev:
		return sIFBLK
	case inodeBasicFifo, inodeExtFifo:
		return sIFIFO
	case inodeBasicSocket, inodeExtSocket:
		return sIFSOCK
	}
	return 0
}

// blockCount returns the number of full data blocks in the block list for a
// file of fileSize using blockSize, given whether a tail fragment is present.
func blockCount(fileSize uint64, blockSize uint32, hasFrag bool) int {
	if hasFrag {
		return int(fileSize / uint64(blockSize))
	}
	return int((fileSize + uint64(blockSize) - 1) / uint64(blockSize))
}

// readInode resolves an inode reference and decodes the inode.
func readInode(fs *FS, ref uint64) (*inode, error) {
	blockOff, inBlockOff := inodeRef(ref)
	c, err := newMetaCursor(fs, int64(fs.sb.InodeTableStart)+blockOff, inBlockOff)
	if err != nil {
		return nil, err
	}
	hdr, err := c.readN(16)
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	in := &inode{
		Type:     le.Uint16(hdr[0:]),
		UID:      le.Uint16(hdr[4:]),
		GID:      le.Uint16(hdr[6:]),
		Mtime:    le.Uint32(hdr[8:]),
		Number:   le.Uint32(hdr[12:]),
		xattrIdx: noXattr,
	}
	in.Mode = le.Uint16(hdr[2:]) | typeBits(in.Type)

	switch in.Type {
	case inodeBasicDir:
		b, err := c.readN(16) // start_block, nlink, file_size(u16), offset(u16), parent
		if err != nil {
			return nil, err
		}
		in.dirStartBlock = le.Uint32(b[0:])
		fileSize := uint64(le.Uint16(b[8:]))
		in.dirOffset = le.Uint16(b[10:])
		in.Size = dirDataSize(fileSize)

	case inodeExtDir:
		b, err := c.readN(24) // nlink, file_size(u32), start_block, parent, i_count(u16), offset(u16), xattr
		if err != nil {
			return nil, err
		}
		fileSize := uint64(le.Uint32(b[4:]))
		in.dirStartBlock = le.Uint32(b[8:])
		in.dirOffset = le.Uint16(b[18:])
		in.xattrIdx = le.Uint32(b[20:])
		in.Size = dirDataSize(fileSize)

	case inodeBasicFile:
		b, err := c.readN(16) // start_block(u32), fragment, offset, file_size(u32)
		if err != nil {
			return nil, err
		}
		in.blocksStart = uint64(le.Uint32(b[0:]))
		in.fragIdx = le.Uint32(b[4:])
		in.fragOffset = le.Uint32(b[8:])
		in.Size = uint64(le.Uint32(b[12:]))
		if err := in.readBlockList(c, fs.sb.BlockSize); err != nil {
			return nil, err
		}

	case inodeExtFile:
		b, err := c.readN(40) // start_block(u64), file_size(u64), sparse(u64), nlink, fragment, offset, xattr
		if err != nil {
			return nil, err
		}
		in.blocksStart = le.Uint64(b[0:])
		in.Size = le.Uint64(b[8:])
		in.fragIdx = le.Uint32(b[28:])
		in.fragOffset = le.Uint32(b[32:])
		in.xattrIdx = le.Uint32(b[36:])
		if err := in.readBlockList(c, fs.sb.BlockSize); err != nil {
			return nil, err
		}

	case inodeBasicSymlink, inodeExtSymlink:
		b, err := c.readN(8) // nlink, symlink_size
		if err != nil {
			return nil, err
		}
		n := le.Uint32(b[4:])
		if n > metaBlockMax {
			return nil, fmt.Errorf("%w: symlink length %d", ErrCorrupt, n)
		}
		target, err := c.readN(int(n))
		if err != nil {
			return nil, err
		}
		in.symlinkTarget = string(target)
		in.Size = uint64(n)
		if in.Type == inodeExtSymlink {
			// Extended symlinks carry a trailing xattr index after the target.
			xb, err := c.readN(4)
			if err != nil {
				return nil, err
			}
			in.xattrIdx = le.Uint32(xb)
		}

	case inodeBasicChrdev, inodeBasicBlkdev, inodeBasicFifo, inodeBasicSocket,
		inodeExtChrdev, inodeExtBlkdev, inodeExtFifo, inodeExtSocket:
		// Special files carry no data we need for read; mode/type suffice.

	default:
		return nil, fmt.Errorf("%w: inode type %d", ErrCorrupt, in.Type)
	}
	return in, nil
}

// readBlockList reads the trailing block-size array of a file inode.
func (in *inode) readBlockList(c *metaCursor, blockSize uint32) error {
	hasFrag := in.fragIdx != invalidFrag
	n := blockCount(in.Size, blockSize, hasFrag)
	if n < 0 || n > 1<<24 {
		return fmt.Errorf("%w: implausible block count %d", ErrCorrupt, n)
	}
	in.blockSizes = make([]uint32, n)
	for i := 0; i < n; i++ {
		b, err := c.readN(4)
		if err != nil {
			return err
		}
		in.blockSizes[i] = binary.LittleEndian.Uint32(b)
	}
	return nil
}

// dirDataSize converts a stored directory file_size (which carries a +3 bias:
// an empty directory has stored size 3) into the real listing byte count.
func dirDataSize(stored uint64) uint64 {
	if stored <= 3 {
		return 0
	}
	return stored - 3
}
