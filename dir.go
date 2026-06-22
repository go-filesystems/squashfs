// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"encoding/binary"
	"fmt"
)

// dirEntry is a parsed directory entry.
type dirEntry struct {
	Name     string
	InodeRef uint64 // full 48-bit reference, ready for readInode
	Number   uint32
	Type     uint16 // SquashFS inode type stored in the entry
}

// readDir parses the directory listing of dir into entries. SquashFS stores a
// directory as a run of headers, each introducing up to 256 entries that share
// a common inode-table metadata block and a base inode number.
func readDir(fs *FS, dir *inode) ([]dirEntry, error) {
	if dir.Size == 0 {
		return nil, nil
	}
	c, err := newMetaCursor(fs,
		int64(fs.sb.DirTableStart)+int64(dir.dirStartBlock), int(dir.dirOffset))
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	remaining := int(dir.Size)
	var out []dirEntry
	for remaining >= 12 {
		hdr, err := c.readN(12)
		if err != nil {
			return nil, err
		}
		remaining -= 12
		count := int(le.Uint32(hdr[0:])) + 1 // stored value is "entries - 1"
		startBlock := uint64(le.Uint32(hdr[4:]))
		baseInode := int64(int32(le.Uint32(hdr[8:])))
		if count < 1 || count > 256 {
			return nil, fmt.Errorf("%w: dir header count %d", ErrCorrupt, count)
		}
		for i := 0; i < count && remaining >= 8; i++ {
			eh, err := c.readN(8)
			if err != nil {
				return nil, err
			}
			remaining -= 8
			offset := uint64(le.Uint16(eh[0:]))
			inodeDelta := int64(int16(le.Uint16(eh[2:])))
			etype := le.Uint16(eh[4:])
			nameLen := int(le.Uint16(eh[6:])) + 1 // stored value is "len - 1"
			if nameLen > remaining || nameLen > 256 {
				return nil, fmt.Errorf("%w: dir entry name length %d", ErrCorrupt, nameLen)
			}
			nameB, err := c.readN(nameLen)
			if err != nil {
				return nil, err
			}
			remaining -= nameLen
			out = append(out, dirEntry{
				Name:     string(nameB),
				InodeRef: startBlock<<16 | offset,
				Number:   uint32(baseInode + inodeDelta),
				Type:     etype,
			})
		}
	}
	return out, nil
}
