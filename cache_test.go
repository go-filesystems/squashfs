// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"bytes"
	"sync"
	"testing"
)

// TestBlockCache_MetaHitMiss exercises a metadata cache miss, store, and hit,
// plus an in-place overwrite of an existing offset (which must not grow the key
// list or evict).
func TestBlockCache_MetaHitMiss(t *testing.T) {
	c := newBlockCache()
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
	c := newBlockCache()
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

// TestBlockCache_DataHitMissEviction exercises the data/fragment cache path,
// including overwrite and FIFO eviction.
func TestBlockCache_DataHitMissEviction(t *testing.T) {
	c := newBlockCache()
	c.dataCap = 2
	if _, ok := c.getData(dk(5)); ok {
		t.Fatal("expected miss")
	}
	c.putData(dk(5), []byte("five"))
	if b, ok := c.getData(dk(5)); !ok || !bytes.Equal(b, []byte("five")) {
		t.Fatalf("data hit mismatch: %v %q", ok, b)
	}
	c.putData(dk(5), []byte("FIVE")) // overwrite, no growth
	if len(c.dataKeys) != 1 {
		t.Fatalf("data re-put grew key list to %d", len(c.dataKeys))
	}
	c.putData(dk(6), []byte("six"))
	c.putData(dk(7), []byte("seven")) // evicts offset 5
	if _, ok := c.getData(dk(5)); ok {
		t.Fatal("offset 5 should have been evicted")
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
}

// dk is a data-cache key for a block at offset n.
func dk(n int64) dataKey { return dataKey{off: n} }

// A sequential pass never asks for a block it evicted, so the cache stays at
// its starting size; a block asked for again after eviction grows it, up to the
// bound and no further.
func TestDataCacheGrowsOnlyOnRereadOfEvicted(t *testing.T) {
	c := newBlockCache()
	c.dataCap, c.dataCapMax = 4, 40
	blk := []byte{1}
	for i := range 1000 { // a long sequential pass: every key once
		k := dataKey{off: int64(i), sizeWord: 1}
		if _, ok := c.getData(k); !ok {
			c.putData(k, blk)
		}
	}
	if c.dataCap != 4 || len(c.data) != 4 {
		t.Fatalf("sequential pass: cap %d, held %d, want 4, 4", c.dataCap, len(c.data))
	}
	// a random workload over 100 blocks: the evicted ones come back
	for round := range 20 {
		for i := range 100 {
			k := dataKey{off: int64((i*7 + round) % 100), sizeWord: 1}
			if _, ok := c.getData(k); !ok {
				c.putData(k, blk)
			}
		}
	}
	if c.dataCap != 40 {
		t.Fatalf("after re-reads cap = %d, want the bound 40", c.dataCap)
	}
	if len(c.data) > 40 {
		t.Fatalf("holds %d blocks, bound 40", len(c.data))
	}
}

func TestDataCacheGhostIsBounded(t *testing.T) {
	c := newBlockCache()
	c.dataCap, c.dataCapMax = 1, 1
	for i := range 3 * ghostCap {
		c.putData(dataKey{off: int64(i)}, []byte{1})
	}
	if len(c.ghost) > ghostCap || len(c.ghostKeys) > ghostCap {
		t.Fatalf("ghost holds %d/%d keys, cap %d", len(c.ghost), len(c.ghostKeys), ghostCap)
	}
}

func TestSetMaxDataCache(t *testing.T) {
	fs := &FS{sb: &Superblock{BlockSize: 1 << 17}, cache: newBlockCache()}
	fs.SetMaxDataCache(DefaultMaxDataCacheBytes)
	if want := DefaultMaxDataCacheBytes / (1 << 17); fs.cache.dataCapMax != want {
		t.Errorf("max = %d blocks, want %d", fs.cache.dataCapMax, want)
	}
	fs.SetMaxDataCache(1) // below the starting size: held at the starting size
	if fs.cache.dataCapMax != defaultDataCacheBlocks {
		t.Errorf("max %d, want the starting %d", fs.cache.dataCapMax, defaultDataCacheBlocks)
	}
	before := fs.cache.dataCapMax
	fs.SetMaxDataCache(0) // nothing to set
	if fs.cache.dataCapMax != before {
		t.Errorf("SetMaxDataCache(0) changed the bound to %d", fs.cache.dataCapMax)
	}
	(&FS{}).SetMaxDataCache(1 << 20)                                    // no superblock: no panic
	(&FS{sb: &Superblock{BlockSize: 1 << 17}}).SetMaxDataCache(1 << 20) // no cache: no panic
}

// A crafted block size of 1 must not turn the byte bound into 2^28 entries.
func TestSetMaxDataCacheTinyBlockSize(t *testing.T) {
	fs := &FS{sb: &Superblock{BlockSize: 1}, cache: newBlockCache()}
	fs.SetMaxDataCache(DefaultMaxDataCacheBytes)
	if want := DefaultMaxDataCacheBytes / minCountedBlock; fs.cache.dataCapMax != want {
		t.Errorf("max = %d entries, want %d", fs.cache.dataCapMax, want)
	}
}

// A cache that grew shrinks when the bound is lowered.
func TestSetMaxDataCacheShrinks(t *testing.T) {
	c := newBlockCache()
	c.dataCap, c.dataCapMax = 200, 200
	for i := range 200 {
		c.putData(dataKey{off: int64(i)}, []byte{1})
	}
	fs := &FS{sb: &Superblock{BlockSize: 1 << 17}, cache: c}
	fs.SetMaxDataCache(8 << 20)
	if c.dataCap != 64 || len(c.data) != 64 {
		t.Fatalf("cap %d, held %d, want 64, 64", c.dataCap, len(c.data))
	}
	if _, ok := c.getData(dataKey{off: 199}); !ok {
		t.Error("shrinking evicted the newest block, want the oldest")
	}
}

// Growth under concurrent readers is race-free and stays within the bound.
func TestDataCacheGrowthConcurrent(t *testing.T) {
	c := newBlockCache()
	c.dataCap, c.dataCapMax = 4, 50
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 2000 {
				k := dataKey{off: int64((i*13 + g) % 120)}
				if _, ok := c.getData(k); !ok {
					c.putData(k, []byte{1})
				}
			}
		})
	}
	wg.Wait()
	if len(c.data) > 50 || c.dataCap > 50 {
		t.Fatalf("held %d, cap %d, bound 50", len(c.data), c.dataCap)
	}
}
