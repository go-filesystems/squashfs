# Performance parity — go-filesystems/squashfs vs kernel squashfs / unsquashfs  (2026-06-22)

## Methodology

- **Where**: a Tart VM (linux/arm64) on an Apple-silicon (M4) host. Our pure-Go
  driver and the reference C tools run in the same VM, same kernel, same
  hardware. Reads are **cold** (caches dropped before every iteration). The
  before→after rows below were measured **on identical hardware, in the same VM
  run, against the same generated image** (`cb-tpm-ubuntu`, Linux 6.17), so the
  speedup is a true A/B and not a cross-machine artefact.
- **CPU / kernel**: 4 vCPU aarch64, Linux 6.17 (Ubuntu 24.04). The original
  baseline row was first taken on a separate VM (Linux 6.12, Debian 13) whose
  kernel read at 3313 MB/s; re-running both old and new code on the same machine
  is what the before→after table reports.
- **Go**: 1.26.4 linux/arm64, CGO disabled.
- **Reference tools**: squashfs-tools 4.6.1 (`mksquashfs`, `unsquashfs`),
  in-tree kernel squashfs.
- **Image set**: 2008 files — 2000 small (1–4 KiB) + 8 large (4 MiB) ≈ 38 MB of
  file data. `mksquashfs` is the image creator (default gzip compression,
  128 KiB blocks); the resulting image is 36.8 MB.
- **Sampling**: best-of-5; read cold; throughput on the ~38 MB *uncompressed*
  payload.
- **SquashFS is read-only**, so there is no Format benchmark.
- **Read**: image created by `mksquashfs <fileset> img`, then read three ways —
  ours (`OpenFile` + recursive walk), the kernel (`mount -t squashfs -o loop` +
  `tar -cf /dev/null`), and the userspace peer `unsquashfs -d <dir>`.
- **Correctness gate (verified)**: our extraction returns exactly 2008 files and
  the same ~38 MB of decompressed payload as the source tree.

## Results

### Before → after (same VM, same image, best-of-5 cold), ~38.6 MB payload

| build | ours (MB/s, wall) | vs kernel | vs unsquashfs |
|-------|-------------------|-----------|---------------|
| **before** (no cache, stdlib `compress/zlib`) | 57.8 MB/s, 667.0 ms | 30.95× slower | 30.97× slower |
| **after**  (metadata+data cache, `klauspost/compress/zlib`) | **730.8 MB/s, 52.8 ms** | **2.45× slower** | **2.45× slower** |

Reference on the same run: kernel loop-mount **1790 MB/s** (21.6 ms),
`unsquashfs` **1792 MB/s** (21.5 ms).

**Net: 12.6× faster read, cutting the kernel gap from ~31× to 2.45×.** (The
original cross-machine table reported "50× vs kernel" because that VM's kernel
read at 3313 MB/s; on identical hardware the old code was 31× slower and the new
code is 2.45× slower.)

### Which lever helped most (isolated on the same hardware)

| variant | MB/s | factor vs before |
|---------|------|------------------|
| before (no cache, stdlib flate) | ~58 | 1.0× |
| **+ metadata/data block cache** (stdlib flate) | **629** | **~10.8×** |
| + cache **and** `klauspost/compress/zlib` | 731 | ~12.6× |

The **decompressed-block cache is overwhelmingly the dominant lever** (~10.8× of
the 12.6×). The walk's per-file `Stat` re-resolves every path from the root, so
without a cache the same inode-table and directory-table metadata blocks were
re-decompressed thousands of times; caching them by on-disk offset (the image is
read-only, so no invalidation is ever needed) turns those into map lookups.
Swapping stdlib `compress/zlib` for the faster pure-Go `klauspost/compress/zlib`
adds a further ~16% on the now decompression-light path.

## Summary

- **Read is now 2.45× of the kernel and on par with `unsquashfs`** — down from a
  ~31× kernel gap. SquashFS read is decompression-bound, and the cache removes
  the redundant decompression that dominated the cold walk.

### Fixed

1. **Decompressed metadata-block cache** (`cache.go`), keyed by absolute on-disk
   offset, shared by every metadata reader (inode table, directory table,
   fragment / xattr-id index, xattr key/value region). Read-only image ⇒ a full
   cache is always consistent and never invalidated. **Dominant win.**
2. **Decompressed data/fragment-block cache.** Many small files pack their tails
   into one shared fragment block; that block is now decompressed once instead of
   once per file.
3. **Faster pure-Go DEFLATE.** Swapped stdlib `compress/zlib` →
   `github.com/klauspost/compress/zlib` (already a dep via zstd), CGO still 0.

### Remaining gap / further ideas

The residual 2.45× vs the kernel is the expected cost of userspace + a pure-Go
single-threaded decompressor against C + page cache. Possible follow-ups:

- [ ] Parallel extract: decompress independent data/fragment blocks on a worker
      pool across cores (`unsquashfs` does this).
- [ ] Pool decompression scratch buffers / reuse zlib reader instances via
      `Reset` to shave the remaining per-block allocation.
- [ ] Investigate SIMD-assisted decompression for the lz4/zstd block compressors
      via go-asmgen where applicable.

## Reproduce

```sh
sudo ./benchmarks/run.sh squashfs <repo_dir> <work_dir> 5
```

`benchmarks/run.sh` is shared across the go-filesystems drivers;
`benchmarks/bench.go` is the squashfs (read-only) harness. Standalone `main`
package, excluded from the coverage gate.
