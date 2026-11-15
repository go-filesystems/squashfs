// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import "sync"

// metaEntry is a decompressed metadata block plus the absolute file offset of
// the block that follows it on disk.
type metaEntry struct {
	data []byte
	next int64
}

// blockCache caches decompressed blocks keyed by their absolute on-disk offset.
//
// SquashFS is read-only, so a block at a given offset never changes for the
// lifetime of an open image: a full cache is always consistent and never needs
// invalidation. During a directory walk the same inode-table and directory-table
// metadata blocks are re-read (and, without this, re-decompressed) for every
// file; caching the decompressed bytes turns those repeated decompressions into
// map lookups, which is the dominant read-throughput win.
//
// The cache is mutex-guarded so it preserves FS's documented
// safe-for-concurrent-use contract. Bounding is by entry count: metadata blocks
// are <= 8 KiB and data/fragment blocks <= block_size (default 128 KiB), and the
// total metadata region of a real image is a few thousand blocks at most, so a
// generous cap keeps memory predictable while still serving an entire walk from
// cache in practice.
type blockCache struct {
	mu       sync.Mutex
	meta     map[int64]metaEntry // decompressed metadata blocks
	data     map[dataKey][]byte  // decompressed data/fragment blocks
	metaCap  int
	dataCap  int
	metaKeys []int64 // insertion order, for FIFO eviction once metaCap is hit
	dataKeys []dataKey
	frag     map[uint32]fragmentEntry // fragment-table entries (bounded by maxFragCache)
	fragKeys []uint32
}

// Default cache caps. Metadata blocks are tiny (<= 8 KiB) and few, so we keep a
// large metadata cache; data/fragment blocks can be up to block_size each, so
// the data cache is capped more tightly to bound memory.
const (
	defaultMetaCacheBlocks = 16384 // up to ~128 MiB of metadata, normally far less
	defaultDataCacheBlocks = 64    // up to ~8 MiB at the 128 KiB default block size
)

func newBlockCache() *blockCache {
	return &blockCache{
		meta:    make(map[int64]metaEntry),
		data:    make(map[dataKey][]byte),
		metaCap: defaultMetaCacheBlocks,
		dataCap: defaultDataCacheBlocks,
	}
}

// getMeta returns a cached decompressed metadata block, or false if absent.
// A nil cache (an FS constructed without one) reports a miss and stores nothing,
// degrading transparently to the uncached path.
func (c *blockCache) getMeta(off int64) (metaEntry, bool) {
	if c == nil {
		return metaEntry{}, false
	}
	c.mu.Lock()
	e, ok := c.meta[off]
	c.mu.Unlock()
	return e, ok
}

// putMeta stores a decompressed metadata block, evicting the oldest entry if the
// cap is exceeded. The cached slice must not be mutated by callers.
func (c *blockCache) putMeta(off int64, e metaEntry) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if _, exists := c.meta[off]; !exists {
		if len(c.meta) >= c.metaCap && len(c.metaKeys) > 0 {
			old := c.metaKeys[0]
			c.metaKeys = c.metaKeys[1:]
			delete(c.meta, old)
		}
		c.metaKeys = append(c.metaKeys, off)
	}
	c.meta[off] = e
	c.mu.Unlock()
}

// getData returns a cached decompressed data/fragment block, or false if absent.
func (c *blockCache) getData(k dataKey) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	b, ok := c.data[k]
	c.mu.Unlock()
	return b, ok
}

// putData stores a decompressed data/fragment block, evicting the oldest entry
// if the cap is exceeded. The cached slice must not be mutated by callers.
func (c *blockCache) putData(k dataKey, b []byte) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if _, exists := c.data[k]; !exists {
		if len(c.data) >= c.dataCap && len(c.dataKeys) > 0 {
			old := c.dataKeys[0]
			c.dataKeys = c.dataKeys[1:]
			delete(c.data, old)
		}
		c.dataKeys = append(c.dataKeys, k)
	}
	c.data[k] = b
	c.mu.Unlock()
}

// dataKey identifies a cached data/fragment block by where it is AND how it was
// read: the offset alone let a crafted image name one offset as a compressed
// block and as a raw one and be served the bytes of whichever came first.
type dataKey struct {
	off      int64
	sizeWord uint32 // on-disk length and the uncompressed bit
}

// maxFragCache bounds the cached fragment-table entries (16 bytes of map entry
// each): FragCount comes from the image and can name billions.
const maxFragCache = 4096

func (c *blockCache) getFrag(idx uint32) (fragmentEntry, bool) {
	if c == nil {
		return fragmentEntry{}, false
	}
	c.mu.Lock()
	e, ok := c.frag[idx]
	c.mu.Unlock()
	return e, ok
}

func (c *blockCache) putFrag(idx uint32, e fragmentEntry) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.frag == nil {
		c.frag = make(map[uint32]fragmentEntry)
	}
	if _, exists := c.frag[idx]; !exists {
		if len(c.frag) >= maxFragCache && len(c.fragKeys) > 0 {
			delete(c.frag, c.fragKeys[0])
			c.fragKeys = c.fragKeys[1:]
		}
		c.fragKeys = append(c.fragKeys, idx)
	}
	c.frag[idx] = e
	c.mu.Unlock()
}
