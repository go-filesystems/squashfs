# squashfs

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
| ListDir | ✅ | Multi-header directories (no `.`/`..` — SquashFS omits them) |
| Stat | ✅ | mode (type + perms), size, inode number |
| ReadLink / Symlinks | ✅ | Targets read; followed during path resolution |
| Compression — gzip / xz / zstd / lzo / lz4 | ✅ | gzip (zlib), xz (LZMA2, no BCJ), zstd, LZO1X, LZ4 |
| Compression — lzma (legacy) | ⏳ | Returns `ErrUnsupportedCompression` |
| Write operations | ❌ | Read-only format; mutators return `ErrReadOnly` |

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
```

## Limitations

- Read-only (the on-disk format is read-only by design).
- gzip, xz, zstd, LZO and lz4 blocks are decoded; only legacy standalone lzma is
  not (returns `ErrUnsupportedCompression`). xz with BCJ filters is unsupported.
- Extended attributes (xattr table) are not surfaced yet.
- Intended for tooling and testing.
