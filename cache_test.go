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
	if _, ok := c.getData(5); ok {
		t.Fatal("expected miss")
	}
	c.putData(5, []byte("five"))
	if b, ok := c.getData(5); !ok || !bytes.Equal(b, []byte("five")) {
		t.Fatalf("data hit mismatch: %v %q", ok, b)
	}
	c.putData(5, []byte("FIVE")) // overwrite, no growth
	if len(c.dataKeys) != 1 {
		t.Fatalf("data re-put grew key list to %d", len(c.dataKeys))
	}
	c.putData(6, []byte("six"))
	c.putData(7, []byte("seven")) // evicts offset 5
	if _, ok := c.getData(5); ok {
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
	if _, ok := c.getData(1); ok {
		t.Fatal("nil getData should miss")
	}
	c.putData(1, nil) // no panic, no store
}
