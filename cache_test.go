// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"testing"
)

// TestBlockCache_MetaHitMiss exercises a metadata cache miss, store, and hit,
// plus an in-place overwrite of an existing offset (which must not grow the key
// list or evict).
func TestBlockCache_MetaHitMiss(t *testing.T) {
	c := newBlockCache(1 << 17)
	if _, ok := c.getMeta(10); ok {
		t.Fatal("expected miss on empty cache")
	}
	c.putMeta(10, metaEntry{data: []byte("abc"), next: 13})
	e, ok := c.getMeta(10)
	if !ok || string(e.data) != "abc" || e.next != 13 {
		t.Fatalf("hit mismatch: %v %q %d", ok, e.data, e.next)
	}
	// Re-put the same offset: overwrite without changing the key order/length.
	c.putMeta(10, metaEntry{data: []byte("xyz"), next: 14})
	if len(c.metaKeys) != 1 {
		t.Fatalf("re-put grew key list to %d", len(c.metaKeys))
	}
	e, _ = c.getMeta(10)
	if string(e.data) != "xyz" || e.next != 14 {
		t.Fatalf("overwrite failed: %q %d", e.data, e.next)
	}
}

// TestBlockCache_MetaEviction fills the metadata cache past its cap and checks
// FIFO eviction of the oldest offset.
func TestBlockCache_MetaEviction(t *testing.T) {
	c := newBlockCache(1 << 17)
	c.metaCap = 2
	c.putMeta(1, metaEntry{data: []byte("a")})
	c.putMeta(2, metaEntry{data: []byte("b")})
	c.putMeta(3, metaEntry{data: []byte("c")}) // evicts offset 1
	if _, ok := c.getMeta(1); ok {
		t.Fatal("offset 1 should have been evicted")
	}
	for _, off := range []int64{2, 3} {
		if _, ok := c.getMeta(off); !ok {
			t.Fatalf("offset %d should be present", off)
		}
	}
}

// TestBlockCache_DataHitMiss exercises the data/fragment cache path, including
// an overwrite.
func TestBlockCache_DataHitMiss(t *testing.T) {
	c := newBlockCache(1 << 17)
	if _, ok := c.getData(dk(5)); ok {
		t.Fatal("expected miss")
	}
	c.putData(dk(5), []byte("five"))
	if b, ok := c.getData(dk(5)); !ok || !bytes.Equal(b, []byte("five")) {
		t.Fatalf("data hit mismatch: %v %q", ok, b)
	}
	c.putData(dk(5), []byte("FIVE")) // overwrite, no growth
	if n := c.data.Len(); n != 1 {
		t.Fatalf("data re-put holds %d entries, want 1", n)
	}
	if b, _ := c.getData(dk(5)); string(b) != "FIVE" {
		t.Fatalf("overwrite failed: %q", b)
	}
}

// TestBlockCache_NilSafe confirms a nil cache degrades to no-op miss/store so an
// FS built without a cache (e.g. in tests) never panics.
func TestBlockCache_NilSafe(t *testing.T) {
	var c *blockCache
	if _, ok := c.getMeta(1); ok {
		t.Fatal("nil getMeta should miss")
	}
	c.putMeta(1, metaEntry{}) // no panic, no store
	if _, ok := c.getData(dk(1)); ok {
		t.Fatal("nil getData should miss")
	}
	c.putData(dk(1), nil) // no panic, no store
	c.setDataCapMax(1 << 20)
}

// dk is a data-cache key for a block at offset n.
func dk(n int64) dataKey { return dataKey{off: n} }

// The data cache starts at 64 blocks and, with its growth rule living in
// inflight.Cache, grows only when an evicted block is asked for again.
func TestDataCacheStartsAt64Blocks(t *testing.T) {
	const bs = 1 << 17
	c := newBlockCache(bs)
	if got, want := c.data.Cap(), int64(defaultDataCacheBlocks*bs); got != want {
		t.Fatalf("start = %d, want %d", got, want)
	}
	blk := make([]byte, bs)
	for i := range 1000 { // a long sequential pass: every key once
		k := dataKey{off: int64(i), sizeWord: 1}
		if _, ok := c.getData(k); !ok {
			c.putData(k, blk)
		}
	}
	if got := c.data.Cap(); got != defaultDataCacheBlocks*bs || c.data.Len() != defaultDataCacheBlocks {
		t.Fatalf("sequential pass: cap %d, held %d", got, c.data.Len())
	}
	for round := range 5 { // a random workload over 200 blocks: evicted ones come back
		for i := range 200 {
			k := dataKey{off: int64((i*7 + round) % 200), sizeWord: 1}
			if _, ok := c.getData(k); !ok {
				c.putData(k, blk)
			}
		}
	}
	if c.data.Cap() <= defaultDataCacheBlocks*bs {
		t.Fatalf("cap %d did not grow on re-reads of evicted blocks", c.data.Cap())
	}
}

// Tiny blocks are counted at minCountedBlock each, so the bound is never
// hundreds of millions of entries.
func TestDataCacheTinyBlocksCountedAtFloor(t *testing.T) {
	c := newBlockCache(1)
	if got, want := c.data.Cap(), int64(defaultDataCacheBlocks*minCountedBlock); got != want {
		t.Fatalf("start = %d, want %d", got, want)
	}
	for i := range 10 * defaultDataCacheBlocks {
		c.putData(dataKey{off: int64(i)}, []byte{1})
	}
	if n := c.data.Len(); n != defaultDataCacheBlocks {
		t.Fatalf("holds %d tiny blocks, want %d", n, defaultDataCacheBlocks)
	}
}

func TestSetMaxDataCache(t *testing.T) {
	fs := &FS{sb: &Superblock{BlockSize: 1 << 17}, cache: newBlockCache(1 << 17)}
	start := fs.cache.data.Cap()
	fs.SetMaxDataCache(1) // below the starting size: held at the starting size
	if got := fs.cache.data.Cap(); got != start {
		t.Errorf("cap %d, want the starting %d", got, start)
	}
	fs.SetMaxDataCache(0)                                               // nothing to set
	(&FS{}).SetMaxDataCache(1 << 20)                                    // no superblock: no panic
	(&FS{sb: &Superblock{BlockSize: 1 << 17}}).SetMaxDataCache(1 << 20) // no cache: no panic
}

// A cache that grew shrinks when the bound is lowered.
func TestSetMaxDataCacheShrinks(t *testing.T) {
	const bs = 1 << 17
	c := newBlockCache(bs)
	fs := &FS{sb: &Superblock{BlockSize: bs}, cache: c}
	blk := make([]byte, bs)
	for round := range 4 {
		for i := range 300 {
			k := dataKey{off: int64((i*7 + round) % 300)}
			if _, ok := c.getData(k); !ok {
				c.putData(k, blk)
			}
		}
	}
	if c.data.Cap() <= defaultDataCacheBlocks*bs {
		t.Fatal("setup: cache did not grow")
	}
	fs.SetMaxDataCache(8 << 20)
	if c.data.Cap() != 8<<20 || c.data.Len() != defaultDataCacheBlocks {
		t.Fatalf("cap %d, held %d, want %d, %d", c.data.Cap(), c.data.Len(), 8<<20, defaultDataCacheBlocks)
	}
}
