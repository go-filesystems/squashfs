# Performance parity — go-filesystems/squashfs vs kernel squashfs / unsquashfs  (2026-06-22)

## Methodology

- **Where**: the `debian` Tart VM (linux/arm64) on an Apple-silicon (M4) host.
  Our pure-Go driver and the reference C tools run in the same VM, same kernel,
  same hardware. Reads are **cold** (caches dropped before every iteration).
- **CPU / kernel**: 4 vCPU aarch64, Linux 6.12.74 (Debian 13).
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

| op | size | ours (MB/s, wall) | reference (MB/s, wall) | ratio | verdict |
|----|------|-------------------|------------------------|-------|---------|
| Read (cold) | 38 MB | 66 MB/s, 558.3 ms | kernel: 3313 MB/s, 11.1 ms | 50.3× | ours 50× slower |
| Read (cold) | 38 MB | 66 MB/s, 558.3 ms | unsquashfs: 801 MB/s, 45.9 ms | 12.2× | ours 12× slower |

## Summary

- **This is our worst read gap: 50× vs the kernel, 12× vs `unsquashfs`.** It is
  honest and expected — SquashFS read is decompression-bound, and that is
  exactly where a pure-Go userspace reader without batching or parallelism
  suffers most.

### Root causes

1. **Per-block decompression with no caching.** Each fragment/data block is
   decompressed independently, and the **metadata block cache is small/absent**,
   so shared metadata blocks (inode + directory tables) are decompressed
   repeatedly across the 2008-file walk.
2. **Pure-Go `compress/flate`.** The kernel and `unsquashfs` use zlib with
   tuned/assembly inner loops; our gzip path is the standard-library decoder.
3. **No parallelism.** `unsquashfs` decompresses with a worker pool across
   cores; we are single-threaded.
4. **Per-file / per-block allocation** → GC pressure.

### Action items

- [ ] **Cache decompressed metadata blocks** (inode + directory tables) keyed by
      on-disk offset — the single biggest win, since the walk re-touches the same
      metadata blocks for every file in a directory.
- [ ] Parallel extract: decompress data/fragment blocks on a worker pool.
- [ ] Pool decompression scratch buffers; reuse `flate.Reader` instances via
      `Reset`.
- [ ] Evaluate a faster DEFLATE (e.g. `klauspost/compress/flate`, already a
      transitive dep) for the gzip path; benchmark vs stdlib.
- [ ] Investigate SIMD-assisted decompression for the lz4/zstd block
      compressors via go-asmgen where applicable.

## Reproduce

```sh
sudo ./benchmarks/run.sh squashfs <repo_dir> <work_dir> 5
```

`benchmarks/run.sh` is shared across the go-filesystems drivers;
`benchmarks/bench.go` is the squashfs (read-only) harness. Standalone `main`
package, excluded from the coverage gate.
