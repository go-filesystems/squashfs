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
| Compression — gzip / xz / zstd / lzo / lz4 | ✅ | gzip (zlib), xz (LZMA2, no BCJ), zstd, LZO1X, LZ4 |
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

## Limitations

- Reading: gzip, xz, zstd, LZO, lz4 and legacy standalone lzma blocks are all
  decoded. xz with BCJ filters is unsupported.
- Writing (`BuildFromDir`): produces gzip or uncompressed images; files are
  stored as full data blocks (no tail-end fragment packing yet), all owned by
  uid/gid 0, no xattrs. Once written, an image is immutable (no in-place edits).
- Extended attributes (xattr table) are not surfaced.
- Intended for tooling and testing.
