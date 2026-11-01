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

## Limitations

- Reading: gzip, xz, zstd, LZO, lz4 and legacy standalone lzma blocks are all
  decoded; xz blocks may carry the Delta and BCJ filters.
- Writing (`BuildFromDir`): produces gzip or uncompressed images; files are
  stored as full data blocks (no tail-end fragment packing yet), all owned by
  uid/gid 0, no xattrs. Once written, an image is immutable (no in-place edits).
- Extended attributes (xattr table) are not surfaced.
- Intended for tooling and testing.
