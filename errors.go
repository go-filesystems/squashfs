// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// Package squashfs is a pure-Go, read-only driver for the SquashFS 4.0
// on-disk format produced by mksquashfs. It parses the superblock,
// metadata-backed inode and directory tables, file data blocks and tail-end
// fragments, and decompresses gzip/zlib-compressed (and uncompressed)
// metadata and data. SquashFS is an inherently read-only archive format;
// every mutating method of filesystem.Filesystem returns ErrReadOnly.
package squashfs

import "errors"

// Sentinel errors returned by the SquashFS driver. Compare with errors.Is so
// wrapped errors continue to match.
var (
	// ErrReadOnly is returned by every mutating method (WriteFile, MkDir,
	// DeleteFile, DeleteDir, Rename). SquashFS is a read-only archive.
	ErrReadOnly = errors.New("squashfs: filesystem is read-only")

	// ErrBadMagic is returned when the superblock magic is not "hsqs".
	ErrBadMagic = errors.New("squashfs: bad superblock magic")

	// ErrUnsupportedVersion is returned for any on-disk version other than 4.0.
	ErrUnsupportedVersion = errors.New("squashfs: unsupported version (only 4.0)")

	// ErrUnsupportedCompression is returned when the image uses a compressor
	// this driver cannot yet decode.
	ErrUnsupportedCompression = errors.New("squashfs: unsupported compression")

	// ErrNotFound is returned when a path component cannot be located.
	ErrNotFound = errors.New("squashfs: path not found")

	// ErrNotDirectory is returned when ListDir targets a non-directory.
	ErrNotDirectory = errors.New("squashfs: not a directory")

	// ErrNotRegular is returned when ReadFile targets a non-regular file.
	ErrNotRegular = errors.New("squashfs: not a regular file")

	// ErrNotSymlink is returned when ReadLink targets a non-symlink.
	ErrNotSymlink = errors.New("squashfs: not a symbolic link")

	// ErrTooManyLinks is returned when path resolution exceeds the symlink hop
	// limit, indicating a loop.
	ErrTooManyLinks = errors.New("squashfs: too many symbolic link traversals")

	// ErrCorrupt is returned when an on-disk structure fails a sanity check.
	ErrCorrupt = errors.New("squashfs: corrupt image")
)
