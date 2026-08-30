// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"

	filesystem "github.com/go-filesystems/interface"
)

// Verify implementation of the optional read-at-an-offset interface.
var _ filesystem.Opener = (*FS)(nil)

// squashfsFile is an open regular file on a SquashFS image, backing
// filesystem.File.
//
// SquashFS is compressed, which sounds like it should rule random access out,
// and does not: a file is cut into fixed-size LOGICAL blocks (block_size,
// 128 KiB by default) that are compressed INDEPENDENTLY, and the inode carries
// the on-disk length of each one. So the logical block holding byte N is
// N/block_size, and its position on disk is the running sum of the preceding
// lengths — a prefix sum computed once, at OpenFile, over metadata the driver
// has already decoded. Serving a byte range costs the one or two blocks it
// touches, not the file. The tail of a small file may instead live packed into
// a shared fragment block, handled the same way.
//
// Decompression goes through fs.readBlock, so it uses the SAME block cache
// ReadFile uses — nothing here bypasses it. That matters more than usual here:
// a fragment block is shared between many files' tails, and sequential ReadAt
// calls within one block hit the cache rather than decompressing again.
//
// Every field is written once during OpenFile and only read afterwards, so
// concurrent ReadAt calls need no synchronisation of their own, as io.ReaderAt
// requires; the cache underneath is mutex-guarded and the decompressors are
// stateless. The one mutable field, closed, is atomic.
type squashfsFile struct {
	fs *FS
	// blockOffsets[i] is the absolute image offset of logical block i, and
	// blockWords[i] its size word (compressed flag + on-disk length). A zero
	// length means a sparse block, which reads as zeros.
	blockOffsets []int64
	blockWords   []uint32
	// fragIdx is invalidFrag when the file has no tail fragment; otherwise
	// the tail past the full blocks lives at fragOffset in that fragment.
	fragIdx    uint32
	fragOffset uint32
	size       int64
	blockSize  int64
	closed     atomic.Bool
}

var _ filesystem.File = (*squashfsFile)(nil)

// blockOnDiskBytes returns the on-disk length of a data block, as an int64,
// masking the uncompressed flag in 64-BIT width.
//
// It exists because the obvious 32-bit form — `n, _ := blockOnDiskSize(sz)`
// followed by `off += int64(n)` — is miscompiled on ppc64le. `sz &^ (1<<24)`
// on a uint32 lowers to `rlwinm RA,RS,0,8,6`, and MB=8 > ME=6 makes that a
// WRAPPED mask: on a 64-bit implementation it covers the high word too, and
// the rotate operand is the 32-bit value concatenated with itself, so the
// result is the value duplicated into both halves. Widening it to int64
// without re-materialising a zero-extension then adds n*(2^32+1) instead of n.
//
// This is not hypothetical. On the emulated ppc64le CI leg, the second block
// of a three-block file came back at offset 862<<32|958 instead of 958, and
// the read failed with EOF; every other architecture was correct. readFile in
// data.go escapes the same shape only by accident — its n happens to be
// spilled and reloaded with a zero-extending lwz. Masking in 64-bit width
// emits no rlwinm at all, so the hazard cannot recur here whatever the
// register allocator does.
func blockOnDiskBytes(sz uint32) int64 {
	return int64(sz) &^ int64(dataUncompressedBit)
}

// OpenFile opens the regular file at path for random access, following
// symlinks exactly as ReadFile does.
//
// It decodes the inode and computes the prefix sum of the block list — the map
// — but reads and decompresses no file data.
func (fs *FS) OpenFile(path string) (filesystem.File, error) {
	in, err := resolve(fs, path, true)
	if err != nil {
		return nil, err
	}
	if !in.isRegular() {
		return nil, fmt.Errorf("%w: %s", ErrNotRegular, path)
	}
	return fs.newFile(in)
}

// newFile builds the File for a decoded regular-file inode. Split out of
// OpenFile so the map construction can be exercised against a hand-made inode.
func (fs *FS) newFile(in *inode) (filesystem.File, error) {
	blockSize := uint64(fs.sb.BlockSize)
	// in.Size is attacker-controlled — for an extended-file inode it is a raw
	// on-disk u64. readFile bounds it by the bytes the block list plus one
	// tail fragment can supply before allocating; this path allocates nothing,
	// but the same bound is what makes an inode corrupt rather than merely
	// large, and applying it keeps OpenFile and ReadFile agreeing about which
	// inodes are readable at all.
	maxBytes := (uint64(len(in.blockSizes)) + 1) * blockSize
	if in.Size > maxBytes {
		return nil, fmt.Errorf("%w: file size %d exceeds %d reachable bytes",
			ErrCorrupt, in.Size, maxBytes)
	}

	offsets := make([]int64, len(in.blockSizes))
	words := make([]uint32, len(in.blockSizes))
	off := int64(in.blocksStart)
	for i, sz := range in.blockSizes {
		offsets[i] = off
		words[i] = sz
		off += blockOnDiskBytes(sz)
	}
	return &squashfsFile{
		fs:           fs,
		blockOffsets: offsets,
		blockWords:   words,
		fragIdx:      in.fragIdx,
		fragOffset:   in.fragOffset,
		size:         int64(in.Size),
		blockSize:    int64(blockSize),
	}, nil
}

// Size returns the file's length in bytes, from the inode decoded at OpenFile.
// No I/O.
func (f *squashfsFile) Size() int64 { return f.size }

// Close releases the File. SquashFS files hold no per-file handle — the image
// handle stays owned by the FS — so Close only marks the File unusable, which
// turns a use-after-close into a clear os.ErrClosed instead of a silent read
// through a stale block map. It is idempotent.
func (f *squashfsFile) Close() error {
	f.closed.Store(true)
	return nil
}

// ReadAt implements io.ReaderAt to the letter, the contract io.SectionReader
// and every generic consumer silently depend on:
//
//   - p is filled completely with a nil error whenever the bytes exist;
//   - n < len(p) comes back only together with a non-nil error;
//   - a read running into the end of the file returns io.EOF with whatever
//     bytes it did get, and an offset at or past Size() returns 0, io.EOF.
//
// Each iteration maps the current offset to a logical block, fetches that one
// block through fs.readBlock (hence through the shared decompressed-block
// cache), and copies out only the requested slice of it. Offsets past the last
// full block land in the tail fragment. A sparse block — on-disk length zero —
// yields zeros with no read at all, exactly as readFile assembles it.
func (f *squashfsFile) ReadAt(p []byte, off int64) (int, error) {
	if f.closed.Load() {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, fmt.Errorf("squashfs: ReadAt: negative offset %d", off)
	}
	if off >= f.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) {
		cur := off + int64(n)
		if cur >= f.size {
			return n, io.EOF
		}
		want := int64(len(p) - n)
		if rem := f.size - cur; want > rem {
			want = rem
		}
		idx := int(cur / f.blockSize)
		within := cur % f.blockSize

		if idx >= len(f.blockOffsets) {
			// Past the full blocks: the bytes are in the tail fragment.
			m, err := f.readFragmentInto(p[n:n+int(want)], cur)
			n += m
			if err != nil {
				return n, err
			}
			continue
		}

		// Clip to the end of this logical block; the next iteration picks up
		// the following one.
		if lim := f.blockSize - within; want > lim {
			want = lim
		}
		// The logical length of this block: a full block, or the file's tail.
		logical := f.blockSize
		if rem := f.size - int64(idx)*f.blockSize; logical > rem {
			logical = rem
		}
		if blockOnDiskBytes(f.blockWords[idx]) == 0 {
			// Sparse block: zeros, no I/O — what readFile appends for it.
			for j := int64(0); j < want; j++ {
				p[n+int(j)] = 0
			}
			n += int(want)
			continue
		}
		data, err := f.fs.readBlock(f.blockOffsets[idx], f.blockWords[idx])
		if err != nil {
			return n, err
		}
		if int64(len(data)) < logical {
			return n, fmt.Errorf("%w: short data block (%d < %d)", ErrCorrupt, len(data), logical)
		}
		n += copy(p[n:n+int(want)], data[within:])
	}
	return n, nil
}

// readFragmentInto fills dst from the tail fragment, starting at file offset
// cur. It applies the same bounds check readFile does before slicing the
// fragment, so a forged fragOffset cannot read another file's tail.
func (f *squashfsFile) readFragmentInto(dst []byte, cur int64) (int, error) {
	if f.fragIdx == invalidFrag {
		return 0, fmt.Errorf("%w: offset %d past the block list and no fragment", ErrCorrupt, cur)
	}
	frag, err := f.fs.readFragment(f.fragIdx)
	if err != nil {
		return 0, err
	}
	// The fragment holds the whole tail: from the end of the full blocks to
	// the end of the file.
	tailStart := int64(len(f.blockOffsets)) * f.blockSize
	start := int64(f.fragOffset) + (cur - tailStart)
	end := int64(f.fragOffset) + (f.size - tailStart)
	if start < 0 || end > int64(len(frag)) {
		return 0, fmt.Errorf("%w: fragment slice [%d:%d] exceeds %d", ErrCorrupt, start, end, len(frag))
	}
	return copy(dst, frag[start:]), nil
}
