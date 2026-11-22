<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-squashfs.png" alt="go-filesystems/squashfs" width="720"></p>

# squashfs

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/squashfs.svg)](https://pkg.go.dev/github.com/go-filesystems/squashfs)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/squashfs/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/squashfs/actions/workflows/ci.yml)

Pure-Go, read-only access to **SquashFS 4.0** filesystem images — no root, no external tools, no CGO.

SquashFS is a compressed, read-only archive format used widely for live media,
container/app images (snap, AppImage) and embedded/initramfs roots. This driver
parses an image produced by `mksquashfs` and exposes it through the shared
`github.com/go-filesystems/interface` `Filesystem` API.

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | SquashFS 4.0 superblock; validates magic + version |
| ReadFile | ✅ | Data blocks + tail-end fragments + sparse blocks |
| Read at an offset | ✅ | `(*FS).OpenFile(path)` → `io.ReaderAt` + `Size()` (`filesystem.Opener` / `filesystem.File`); decompresses only the blocks a range touches, through the shared block cache |
| ListDir | ✅ | Multi-header directories (no `.`/`..` — SquashFS omits them) |
| Stat | ✅ | mode (type + perms), size, inode number |
| ReadLink / Symlinks | ✅ | Targets read; followed during path resolution |
| Compression — gzip / xz / zstd / lzo / lz4 | ✅ | gzip (zlib), xz (LZMA2, Delta and BCJ filters), zstd, LZO1X, LZ4 |
| Compression — lzma (legacy) | ✅ | Standalone LZMA1 stream decoded |
| Create image (`BuildFromDir`) | ✅ | Build a SquashFS 4.0 image from a tree; gzip or uncompressed; `unsquashfs`-readable |
| In-place writes (`WriteFile`/`MkDir`/…) | ❌ | The archive is immutable once written; mutators return `ErrReadOnly` |

## References

- https://dr-emann.github.io/squashfs/ (SquashFS Binary Format)
- Linux `fs/squashfs/` and `squashfs-tools` (`mksquashfs` / `unsquashfs`)

## Module

```
github.com/go-filesystems/squashfs
```

## Usage

```go
fs, err := squashfs.OpenFile("image.squashfs")
if err != nil { /* ... */ }
defer fs.Close()

data, err := fs.ReadFile("/etc/hostname")
entries, err := fs.ListDir("/")

// Create an image from a directory tree (gzip by default).
err = squashfs.BuildFromDir("out.squashfs", "/path/to/tree", squashfs.BuildOptions{})
```

### Reading part of a file

`ReadFile` returns the whole file, which is no use to anything serving reads on
demand — a mount, an NFS or 9P export — where a 4 KiB request out of a
multi-gigabyte image must not decompress the lot. The driver implements the
optional [`filesystem.Opener`](https://github.com/go-filesystems/interface)
capability:

```go
var generic filesystem.Filesystem = fs
if o, ok := generic.(filesystem.Opener); ok {
    f, err := o.OpenFile("/usr/lib/big.so")
    if err != nil { /* ... */ }
    defer f.Close()

    buf := make([]byte, 4096)
    n, err := f.ReadAt(buf, 1<<30) // only the blocks that range touches
    _, _ = n, err
    _ = f.Size()                   // from the inode; decompresses nothing
}
```

Compression does not rule random access out. A SquashFS file is cut into
fixed-size **logical** blocks (`block_size`, 128 KiB by default) compressed
**independently**, and the inode carries each one's on-disk length. So the block
holding byte N is `N/block_size`, and its position on disk is the running sum of
the preceding lengths — a prefix sum computed once at `OpenFile` over metadata
already decoded. Serving a range costs the one or two blocks it touches.
Decompression goes through the same block cache `ReadFile` uses, which matters
because a fragment block is shared between many files' tails.

`ReadAt` follows `io.ReaderAt` exactly (`n < len(p)` only with a non-nil error,
`io.EOF` at the end) and is safe to call concurrently.

Note the method `(*FS).OpenFile(path) (filesystem.File, error)` is distinct from
the package-level `squashfs.OpenFile(path) (*FS, error)`, which opens an image
from the host filesystem.

## Untrusted images

An image is attacker-controlled input, and in SquashFS almost every size is a
claim: a 4-byte sparse block entry stands for a whole block of zeros, metadata
is compressed, and a fragment of a header can name gigabytes. Every
input-driven allocation is therefore bounded **before** it is made
(`bombs_test.go` measures each road with `runtime.MemStats`):

| Claim in the image | Bound |
|---|---|
| `block_size` / `block_log` | at most 1 MiB / 20, the reference limits (Linux `super.c`) |
| a data block's on-disk length | at most `block_size`, compressed or stored, and inside the image (Linux `block.c`) |
| a file's size | `ReadFile` refuses more than `DefaultMaxReadFile` (1 GiB) with `ErrTooLarge`; `ReadFileLimit(path, max)` picks the ceiling, `OpenFile` + `ReadAt` stream any size |
| a file's block list | read in chunks as the metadata supplies it, never sized from the claim |
| a directory | at most 4 Mi entries (`ErrTooLarge`) |
| an xattr list | the id index is read one pointer at a time; names and values are capped at 16 MiB per inode |
| zstd output | the shared decoder stops at 1 MiB (the format maximum), not 64 GiB |
| lzma dictionary | the dictionary the header declares (up to 4 GiB) is never allocated: the decoder ([`go-compressions/xz`](https://github.com/go-compressions/xz)) grows its window with the data it produces and refuses to grow it beyond `MaxDictionary`, set to the block's output limit; a block that outgrows it is `ErrCorrupt` |
| xz dictionary | the same: the block header's dictionary is a claim, not an allocation, and `MaxDictionary` is the block's output limit (a planted 1.5 GiB claim over a 44-byte stream costs about 23 KB; it cost 1.6 GB with `ulikunitz/xz`). The stream must also be one block of one stream |

Fuzz targets: `FuzzOpenRead`, `FuzzReadFile`, `FuzzReadMetaBlock`,
`FuzzReadDir`, `FuzzDecompress`, `FuzzReadInode`, `FuzzReadXattr`.

## Parallel reads

The data blocks of a file are compressed independently, so a sequential read
decodes several at once. `ReadAt` decodes the blocks a read spans, and a window
ahead of a sequential reader, on up to `min(GOMAXPROCS, 8)` goroutines through
[`inflight.Ordered`](https://github.com/go-filesystems/inflight) (bounded,
ordered; at most that many blocks in flight, results land in the existing
64-block cache); `ReadFile` assembles the blocks in file order the same way. A
random read of one block is unchanged. `(*FS).SetReadParallelism(n)` sets the
width; 1 is the serial path.

Sequential read of a 256 MiB text-like file, MiB/s, median of 3 interleaved
rounds, caches dropped, 4 vCPU aarch64 VM, Linux 7.0.0-43 (kernel squashfs
built with `CHOICE_DECOMP_BY_MOUNT`, `DECOMP_MULTI_PERCPU` and `DECOMP_SINGLE`
available; the kernel does not scale from 1 to 4 CPUs on this loop-mounted
read), host load below 7:

| compressor | kernel 1 CPU | ours 1 CPU | kernel 4 CPUs | ours 4 CPUs | ours 4 CPUs, serial |
|---|---:|---:|---:|---:|---:|
| xz | 48.3 | 39.5 | 47.7 | **146.0** | 39.1 |
| gzip | 283.6 | 231.1 | 261.2 | **718.8** | 171.4 |
| zstd | 492.1 | 457.4 | 514.6 | **1527.1** | 595.7 |
| lz4 | 1232.3 | 1909.3 | 977.4 | **3136.5** | 2060.4 |
| lzo | 685.5 | 623.3 | 588.0 | **1649.3** | 640.4 |

At one CPU xz, gzip, zstd and lzo remain behind the kernel (open defects, single
core kernels); at four, ours is 2.7 to 3.1 times the kernel on every
compressor. Random 4 KiB reads are not changed by the read-ahead.

### Random 4 KiB reads

The decoded-data cache starts at 64 blocks (8 MiB at 128 KiB) and grows, up to
`DefaultMaxDataCacheBytes` (256 MiB, `(*FS).SetMaxDataCache(bytes)`), only when a
block it evicted is requested again: a random read of a file larger than the
cache does, one sequential pass does not. Before, 20000 random 4 KiB reads over
a 256 MiB file decoded about 19400 blocks where the kernel's page cache decoded
2048. Reads per second over that file, median of 3 interleaved rounds, caches
dropped before the cold pass, the warm pass being the same offsets again, 4 vCPU
VM, host load below 8:

| compressor | cold: kernel | cold: v0.8.1 | cold: now | warm: kernel | warm: now |
|---|---:|---:|---:|---:|---:|
| xz | 3798 | 325 | 2951 | 1573429 | 3034088 |
| gzip | 21875 | 2030 | 16326 | 1513661 | 2853186 |
| zstd | 51286 | 6737 | 45869 | 1596586 | 2184032 |
| lzo | 60764 | 5242 | 52636 | 1676534 | 2978674 |
| lz4 | 84994 | 21881 | 109138 | 1566188 | 2675033 |

Memory: the cache can hold up to 256 MiB of decoded blocks once a workload has
shown it needs them (one 256 MiB file read at random); lower the bound with
`SetMaxDataCache` where that is too much. Cold, xz is at 78 % of the kernel,
gzip 75 %, lzo 87 %, zstd 89 % and lz4 128 %; a deflate or xz stream cannot stop
early, and the lzo and lz4 decoders that could do not need to (v0.8.1 xz figure
from the readpath-par run, the other columns from the same driver as the rest).

## Judged by

The Linux kernel (6.17) and squashfs-tools 4.6.1, run as root in a Tart VM by
[kerneljudge](https://github.com/go-filesystems/kerneljudge) (`kerneljudge run
--driver <clone>`; the test is `kerneljudge_test.go`, behind
`FS_KERNEL_JUDGE=1`). Each image is held to a fixed corpus (nested directories,
UTF-8 names, a sparse and a 32 MiB file, symlinks, xattrs, times) by
`unsquashfs -lls` and `-stat`, a read-only kernel mount and this reader. Reverse:
`mksquashfs` with gzip, lzo, lz4, xz, zstd and no compression writes the images
and this reader reads them. `BuildFromDir` is judged for gzip, none, zstd, xz
and lz4. Both directions are clean, with no known defect. It found, and this
release fixed,
[#42](https://github.com/go-filesystems/squashfs/issues/42) (the image is padded
to 4096 as `mksquashfs` does; the kernel refused it on a loop device),
[#43](https://github.com/go-filesystems/squashfs/issues/43) (source mtimes are
kept), [#44](https://github.com/go-filesystems/squashfs/issues/44) (`Stat` no
longer follows the final symlink), [#45](https://github.com/go-filesystems/squashfs/issues/45)
(`Times` implements `filesystem.TimesReader`) and
[#46](https://github.com/go-filesystems/squashfs/issues/46) (the xz encoder uses a
block-size dictionary and CRC32, which the kernel's decoder accepts). Against the kernel on one image (4000 small files, a
256 MiB file): metadata walk 35758 files/s against 54134 for the kernel; the
sequential read figure is not a storage comparison (host-cached VM disk), see
the kerneljudge README.

## Limitations

- Reading: gzip, xz, zstd, LZO, lz4 and legacy standalone lzma blocks are all
  decoded; xz blocks may carry the Delta and BCJ filters.
- Writing (`BuildFromDir`): gzip, zstd, xz, lz4 or uncompressed images, with
  fragment packing, modification times kept, all owned by uid/gid 0, no xattrs.
  The image is padded to 4096 bytes. Once written, an image is immutable (no
  in-place edits).
- Extended attributes are read (`(*FS).Xattrs`); the builder does not write them.
- `Stat` does not follow a final symlink (lstat); `ReadLink` gives the target.
- One path lookup follows at most 40 symlinks IN TOTAL (a `go-volumes/safeio`
  `SymlinkBudget`, like Linux's `MAXSYMLINKS`), not only 40 deep: a chain
  `l0 -> l1/l1 -> ...` would otherwise cost 2^n lookups. It is refused with
  `ErrTooManyLinks`, which also matches `safeio.ErrSymlinkLoop`.
- Intended for tooling and testing.
