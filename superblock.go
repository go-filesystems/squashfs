// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Magic is the SquashFS superblock magic, "hsqs" in little-endian.
const Magic = 0x73717368

// Compression identifiers (sb.Compression).
const (
	compGZIP = 1
	compLZMA = 2
	compLZO  = 3
	compXZ   = 4
	compLZ4  = 5
	compZSTD = 6
)

// superblockSize is the on-disk size of the SquashFS 4.0 superblock.
const superblockSize = 96

// sbFlagCompressorOptions is set in Superblock.Flags when a compressor-options
// metadata block immediately follows the superblock.
const sbFlagCompressorOptions = 0x0400

// Superblock is the decoded SquashFS 4.0 superblock (little-endian on disk).
// Field order and offsets mirror struct squashfs_super_block.
type Superblock struct {
	Magic            uint32 // 0x00
	InodeCount       uint32 // 0x04
	ModTime          uint32 // 0x08 (unix seconds)
	BlockSize        uint32 // 0x0C
	FragCount        uint32 // 0x10
	Compression      uint16 // 0x14
	BlockLog         uint16 // 0x16 (log2 BlockSize)
	Flags            uint16 // 0x18
	IDCount          uint16 // 0x1A
	VersionMajor     uint16 // 0x1C
	VersionMinor     uint16 // 0x1E
	RootInode        uint64 // 0x20 (inode reference)
	BytesUsed        uint64 // 0x28
	IDTableStart     uint64 // 0x30
	XattrTableStart  uint64 // 0x38
	InodeTableStart  uint64 // 0x40
	DirTableStart    uint64 // 0x48
	FragTableStart   uint64 // 0x50
	LookupTableStart uint64 // 0x58
}

// readSuperblock reads and validates the superblock at offset 0.
func readSuperblock(rs io.ReaderAt) (*Superblock, error) {
	buf := make([]byte, superblockSize)
	if _, err := rs.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("squashfs: read superblock: %w", err)
	}
	le := binary.LittleEndian
	sb := &Superblock{
		Magic:            le.Uint32(buf[0x00:]),
		InodeCount:       le.Uint32(buf[0x04:]),
		ModTime:          le.Uint32(buf[0x08:]),
		BlockSize:        le.Uint32(buf[0x0C:]),
		FragCount:        le.Uint32(buf[0x10:]),
		Compression:      le.Uint16(buf[0x14:]),
		BlockLog:         le.Uint16(buf[0x16:]),
		Flags:            le.Uint16(buf[0x18:]),
		IDCount:          le.Uint16(buf[0x1A:]),
		VersionMajor:     le.Uint16(buf[0x1C:]),
		VersionMinor:     le.Uint16(buf[0x1E:]),
		RootInode:        le.Uint64(buf[0x20:]),
		BytesUsed:        le.Uint64(buf[0x28:]),
		IDTableStart:     le.Uint64(buf[0x30:]),
		XattrTableStart:  le.Uint64(buf[0x38:]),
		InodeTableStart:  le.Uint64(buf[0x40:]),
		DirTableStart:    le.Uint64(buf[0x48:]),
		FragTableStart:   le.Uint64(buf[0x50:]),
		LookupTableStart: le.Uint64(buf[0x58:]),
	}
	if sb.Magic != Magic {
		return nil, ErrBadMagic
	}
	if sb.VersionMajor != 4 || sb.VersionMinor != 0 {
		return nil, fmt.Errorf("%w: %d.%d", ErrUnsupportedVersion, sb.VersionMajor, sb.VersionMinor)
	}
	if sb.BlockSize == 0 || sb.BlockSize != 1<<sb.BlockLog {
		return nil, fmt.Errorf("%w: block_size %d != 1<<%d", ErrCorrupt, sb.BlockSize, sb.BlockLog)
	}
	return sb, nil
}

// readCompressorOptions validates and skips the compressor-options metadata
// block that follows the superblock when sb.Flags advertises one. The block is
// a standard metadata block (2-byte header + payload), almost always stored
// uncompressed. We don't use its contents — every table is reached by absolute
// offset — but parsing it confirms the framing is sound. A missing flag is a
// no-op so images without options (gzip/zstd/xz) are unaffected.
func readCompressorOptions(rs io.ReaderAt, sb *Superblock) error {
	if sb.Flags&sbFlagCompressorOptions == 0 {
		return nil
	}
	var hdr [2]byte
	if _, err := rs.ReadAt(hdr[:], superblockSize); err != nil {
		return fmt.Errorf("squashfs: read compressor options header: %w", err)
	}
	h := binary.LittleEndian.Uint16(hdr[:])
	size := int(h &^ 0x8000) // low 15 bits = on-disk payload size
	if size == 0 || size > metaBlockMax {
		return fmt.Errorf("%w: compressor options block size %d", ErrCorrupt, size)
	}
	buf := make([]byte, size)
	if _, err := rs.ReadAt(buf, superblockSize+2); err != nil {
		return fmt.Errorf("squashfs: read compressor options payload: %w", err)
	}
	return nil
}
