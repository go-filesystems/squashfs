// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"context"
	"runtime"

	"github.com/go-filesystems/inflight"
)

// The data blocks of a file are compressed independently, so a sequential read
// can decode several at once. The scheduling is inflight.Ordered (bounded,
// ordered, no goroutine outlives the call); this file only decides WHICH blocks
// to hand it.
//
// Memory and goroutines are bounded twice: at most readParallelism blocks are in
// flight at once (inflight.Ordered's token), and every decoded block lands in
// the existing bounded block cache (defaultDataCacheBlocks), so a window never
// holds more than maxWindow blocks of the cache.

// maxReadParallelism caps the default worker count: past it the decoders are
// memory-bound on the shared cache and the extra goroutines buy nothing.
const maxReadParallelism = 8

// maxWindow is the most blocks one decode window covers: half the data cache,
// so a window cannot evict the blocks the same read is about to copy out. A
// single ReadAt larger than maxWindow blocks (4 MiB at the default block size)
// is decoded in parallel for its first maxWindow blocks only; the rest decode
// as the read reaches them. The bound is per call: G concurrent readers hold up
// to G x workers blocks in flight.
const maxWindow = defaultDataCacheBlocks / 2

// defaultReadParallelism is min(GOMAXPROCS, maxReadParallelism), at least 1.
func defaultReadParallelism() int {
	return max(1, min(runtime.GOMAXPROCS(0), maxReadParallelism))
}

// SetReadParallelism sets how many data blocks of one read are decompressed at
// once: n <= 1 decodes serially, as before; n is capped at maxWindow. The
// default is min(GOMAXPROCS, 8). It must be called before the FS is shared
// between goroutines.
func (fs *FS) SetReadParallelism(n int) {
	fs.workers = max(1, min(n, maxWindow))
}

// readWorkers is the worker count, defaulting for an FS built without Open.
func (fs *FS) readWorkers() int {
	if fs.workers < 1 {
		return 1
	}
	return fs.workers
}

// prefetch decodes the compressed, not yet cached blocks [first, first+count)
// of the file on up to fs.readWorkers() goroutines, leaving them in the block
// cache. It is best effort: a block that fails is skipped here and fails again,
// with its own error and in file order, when the caller reads it, so prefetch
// returns nothing.
//
// ahead is set for the speculative window of a sequential reader: it then
// waits until the block the reader needs next is missing or a full worker
// batch is missing, so the steady state decodes in batches of readWorkers()
// rather than one block at a time.
func (f *squashfsFile) prefetch(first, count int, ahead bool) {
	w := f.fs.readWorkers()
	if w < 2 {
		return
	}
	last := min(first+count, len(f.blockOffsets))
	var arr [maxWindow]int // on the stack: the common call (nothing to do) allocates nothing
	todo := arr[:0]
	for i := first; i < last && len(todo) < maxWindow; i++ {
		n, compressed := blockOnDiskSize(f.blockWords[i])
		if n == 0 || !compressed {
			continue // sparse, or raw: nothing to decode
		}
		if _, ok := f.fs.cache.getData(dataKey{f.blockOffsets[i], f.blockWords[i]}); ok {
			continue
		}
		todo = append(todo, i)
	}
	if len(todo) < 2 {
		return // one block is decoded by the caller's own read
	}
	if ahead && todo[0] != first && len(todo) < w {
		return // the reader's next block is ready; wait for a full batch
	}
	run := append([]int(nil), todo...) // the closure outlives arr's frame use
	_ = inflight.Ordered(context.Background(), len(run), w,
		func(_ context.Context, k int) (struct{}, error) {
			i := run[k]
			_, _ = f.fs.readBlock(f.blockOffsets[i], f.blockWords[i]) // error: reported by the read itself
			return struct{}{}, nil
		},
		func(int, struct{}) error { return nil })
}

// readAhead is called by ReadAt before it copies [off, off+n) out. It decodes,
// in parallel, the blocks the read spans, plus, when the read continues the
// previous one (a sequential reader), a window of the blocks after it, so that
// a reader asking 4 KiB at a time still finds its next blocks already decoded.
// A random read spanning one block does nothing: its cost is unchanged.
func (f *squashfsFile) readAhead(off int64, n int) {
	if f.fs.readWorkers() < 2 || n <= 0 {
		return
	}
	first := int(off / f.blockSize)
	end := off + int64(n)
	spanned := int((end-1)/f.blockSize) - first + 1
	sequential := f.lastEnd.Swap(end) == off && off > 0
	count := spanned
	if sequential {
		count = max(count, 2*f.fs.readWorkers())
	}
	if count < 2 {
		return
	}
	f.prefetch(first, count, sequential)
}
