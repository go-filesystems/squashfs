// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"encoding/binary"
	"fmt"

	"github.com/go-volumes/safeio"
)

// dataUncompressedBit, when SET in a data/fragment block size word, marks the
// block as stored uncompressed; the low 24 bits hold the on-disk byte length.
const dataUncompressedBit = 1 << 24

// blockOnDiskSize returns (storedBytes, compressed) for a data-block size word.
func blockOnDiskSize(sz uint32) (n uint32, compressed bool) {
	return sz &^ dataUncompressedBit, sz&dataUncompressedBit == 0
}

// readBlock reads and (if needed) decompresses one data block of at most
// blockSize bytes located at absolute file offset off.
//
// Decompressed compressed blocks are cached keyed by their on-disk offset. The
// dominant beneficiary is fragment blocks: many small files pack their tails
// into the same shared fragment block, so without caching that one block is
// re-read and re-decompressed once per file. Callers only ever read from (and
// copy out of) the returned slice, so handing back the cache-owned slice is
// safe for the read-only image.
func (fs *FS) readBlock(off int64, sizeWord uint32) ([]byte, error) {
	n, compressed := blockOnDiskSize(sizeWord)
	if n == 0 {
		return nil, nil // sparse: caller zero-fills
	}
	if n > fs.sb.BlockSize && !compressed {
		return nil, fmt.Errorf("%w: data block %d > block size %d", ErrCorrupt, n, fs.sb.BlockSize)
	}
	if compressed {
		if b, ok := fs.cache.getData(off); ok {
			return b, nil
		}
	}
	raw := make([]byte, n)
	if _, err := fs.rs.ReadAt(raw, off); err != nil {
		return nil, fmt.Errorf("squashfs: read data block @%d: %w", off, err)
	}
	if !compressed {
		return raw, nil
	}
	out, err := fs.d.decompress(raw, int(fs.sb.BlockSize))
	if err != nil {
		return nil, err
	}
	fs.cache.putData(off, out)
	return out, nil
}

// readFile assembles the full contents of regular-file inode in.
func readFile(fs *FS, in *inode) ([]byte, error) {
	blockSize := uint64(fs.sb.BlockSize)
	off := int64(in.blocksStart)
	remaining := in.Size

	// Bound the pre-allocation hint. in.Size is attacker-controlled — for an
	// extended-file inode it is a raw on-disk u64 (inode.go), so a crafted
	// image can declare Size = 2^60 and OOM the host on this make() alone,
	// even though the block list and every block are independently capped.
	// The real assembled length can never exceed the bytes the block list
	// plus a single tail fragment can supply: len(blockSizes) full blocks +
	// one more block of fragment slack. (We cannot clamp by the image size:
	// the format is compressed, so a few KiB of image legitimately expand to
	// many blocks.) Clamp the capacity hint to that ceiling and reject
	// anything beyond it up front, so we neither over-allocate nor silently
	// truncate — the exact length is still re-verified after assembly.
	maxBytes := (uint64(len(in.blockSizes)) + 1) * blockSize
	if in.Size > maxBytes {
		return nil, fmt.Errorf("%w: file size %d exceeds %d reachable bytes",
			ErrCorrupt, in.Size, maxBytes)
	}
	hint, err := safeio.MakeBytes(int64(in.Size), int64(maxBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	out := hint[:0]

	for _, sz := range in.blockSizes {
		want := blockSize
		if remaining < want {
			want = remaining
		}
		n, _ := blockOnDiskSize(sz)
		if n == 0 {
			// Sparse block: a full block of zeros (clamped to the file tail).
			out = append(out, make([]byte, want)...)
		} else {
			data, err := fs.readBlock(off, sz)
			if err != nil {
				return nil, err
			}
			if uint64(len(data)) < want {
				return nil, fmt.Errorf("%w: short data block (%d < %d)", ErrCorrupt, len(data), want)
			}
			out = append(out, data[:want]...)
			off += int64(n)
		}
		remaining -= want
	}

	// Tail-end fragment, if any.
	if in.fragIdx != invalidFrag && remaining > 0 {
		frag, err := fs.readFragment(in.fragIdx)
		if err != nil {
			return nil, err
		}
		start := uint64(in.fragOffset)
		end := start + remaining
		if end > uint64(len(frag)) {
			return nil, fmt.Errorf("%w: fragment slice [%d:%d] exceeds %d", ErrCorrupt, start, end, len(frag))
		}
		out = append(out, frag[start:end]...)
		remaining = 0
	}

	if uint64(len(out)) != in.Size {
		return nil, fmt.Errorf("%w: assembled %d bytes, want %d", ErrCorrupt, len(out), in.Size)
	}
	return out, nil
}

// fragmentEntry locates a fragment's data block.
type fragmentEntry struct {
	start    uint64 // absolute file offset of the fragment data block
	sizeWord uint32 // block size word (compressed bit + on-disk length)
}

const fragEntrySize = 16 // start(8) + size(4) + unused(4)
const fragPerMetaBlock = metaBlockMax / fragEntrySize

// readFragmentEntry reads fragment-table entry idx via the two-level table:
// a flat array of u64 metadata-block offsets at FragTableStart, each block
// holding up to 512 fragment entries.
func (fs *FS) readFragmentEntry(idx uint32) (fragmentEntry, error) {
	if uint32(idx) >= fs.sb.FragCount {
		return fragmentEntry{}, fmt.Errorf("%w: fragment index %d >= %d", ErrCorrupt, idx, fs.sb.FragCount)
	}
	blockIdx := int(idx) / fragPerMetaBlock
	inBlock := int(idx) % fragPerMetaBlock

	var ptr [8]byte
	if _, err := fs.rs.ReadAt(ptr[:], int64(fs.sb.FragTableStart)+int64(blockIdx)*8); err != nil {
		return fragmentEntry{}, fmt.Errorf("squashfs: read frag index: %w", err)
	}
	metaOff := int64(binary.LittleEndian.Uint64(ptr[:]))
	block, _, err := fs.readMetaBlockCached(metaOff)
	if err != nil {
		return fragmentEntry{}, err
	}
	o := inBlock * fragEntrySize
	if o+fragEntrySize > len(block) {
		return fragmentEntry{}, fmt.Errorf("%w: fragment entry %d past block", ErrCorrupt, idx)
	}
	le := binary.LittleEndian
	return fragmentEntry{
		start:    le.Uint64(block[o:]),
		sizeWord: le.Uint32(block[o+8:]),
	}, nil
}

// readFragment returns the decompressed fragment data block for index idx.
func (fs *FS) readFragment(idx uint32) ([]byte, error) {
	fe, err := fs.readFragmentEntry(idx)
	if err != nil {
		return nil, err
	}
	return fs.readBlock(int64(fe.start), fe.sizeWord)
}
