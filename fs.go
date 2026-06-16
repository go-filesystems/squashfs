// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"fmt"
	"io"
	"os"

	filesystem "github.com/go-filesystems/interface"
)

// FS is an opened, read-only SquashFS 4.0 filesystem.
//
// The struct holds an io.ReaderAt, the decoded superblock and the block
// decompressor. All traversal is recomputed per call from the on-disk tables —
// no caches, no locks — which keeps the reader trivially safe for concurrent
// use against a frozen image.
type FS struct {
	rs     io.ReaderAt
	size   int64
	sb     *Superblock
	d      decompressor
	closer io.Closer
}

// Verify the package satisfies the common filesystem interface.
var _ filesystem.Filesystem = (*FS)(nil)

// Open parses the SquashFS superblock at offset 0 and returns a read-only
// handle. The caller retains ownership of rs unless it also implements
// io.Closer, in which case Close is forwarded. Pass size = -1 if unknown.
func Open(rs io.ReaderAt, size int64) (*FS, error) {
	sb, err := readSuperblock(rs)
	if err != nil {
		return nil, err
	}
	d, err := newDecompressor(sb.Compression)
	if err != nil {
		return nil, err
	}
	fs := &FS{rs: rs, size: size, sb: sb, d: d}
	if c, ok := rs.(io.Closer); ok {
		fs.closer = c
	}
	return fs, nil
}

// OpenFile opens path read-only and wires it into Open. The returned FS owns
// the file handle and closes it on Close.
func OpenFile(path string) (*FS, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("squashfs: open %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("squashfs: stat %s: %w", path, err)
	}
	fs, err := Open(f, st.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	return fs, nil
}

// Superblock returns the decoded superblock. The pointer is owned by FS.
func (fs *FS) Superblock() *Superblock { return fs.sb }

// Close releases the backing file handle if FS opened one.
func (fs *FS) Close() error {
	if fs.closer != nil {
		return fs.closer.Close()
	}
	return nil
}

// ReadFile returns the full contents of the regular file at path. Symlinks,
// including a final-component symlink, are followed.
func (fs *FS) ReadFile(path string) ([]byte, error) {
	in, err := resolve(fs, path, true)
	if err != nil {
		return nil, err
	}
	if !in.isRegular() {
		return nil, fmt.Errorf("%w: %s", ErrNotRegular, path)
	}
	return readFile(fs, in)
}

// ListDir enumerates the entries of the directory at path. SquashFS does not
// store "." or ".." entries, so they are absent from the result.
func (fs *FS) ListDir(path string) ([]filesystem.DirEntry, error) {
	in, err := resolve(fs, path, true)
	if err != nil {
		return nil, err
	}
	if !in.isDir() {
		return nil, fmt.Errorf("%w: %s", ErrNotDirectory, path)
	}
	entries, err := readDir(fs, in)
	if err != nil {
		return nil, err
	}
	out := make([]filesystem.DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, filesystem.NewDirEntry(uint64(e.Number), e.Name, uint8(e.Type)))
	}
	return out, nil
}

// Stat resolves path (following symlinks) and returns mode, size and inode.
func (fs *FS) Stat(path string) (filesystem.Stat, error) {
	in, err := resolve(fs, path, true)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStat(in.Mode, in.Size, uint64(in.Number)), nil
}

// Xattrs resolves path (following symlinks) and returns its extended
// attributes, keyed by fully-qualified name (with the namespace prefix, e.g.
// "user.comment") and mapped to the raw value bytes.
//
// A nil map (and nil error) is returned when the node has no attributes or the
// image carries no xattr table. Only the extended inode variants store an
// xattr index, so basic inodes always report none.
func (fs *FS) Xattrs(path string) (map[string][]byte, error) {
	in, err := resolve(fs, path, true)
	if err != nil {
		return nil, err
	}
	return fs.inodeXattrs(in)
}

// inodeXattrs fetches the extended attributes recorded for inode in.
func (fs *FS) inodeXattrs(in *inode) (map[string][]byte, error) {
	if in.xattrIdx == noXattr || !fs.hasXattrTable() {
		return nil, nil
	}
	t, err := fs.readXattrTable()
	if err != nil {
		return nil, err
	}
	e, err := fs.readXattrIDEntry(t, in.xattrIdx)
	if err != nil {
		return nil, err
	}
	return fs.readXattrPairs(t, e)
}

// ReadLink returns the target of the symbolic link at path; the final
// component is not followed.
func (fs *FS) ReadLink(path string) (string, error) {
	in, err := resolve(fs, path, false)
	if err != nil {
		return "", err
	}
	if !in.isSymlink() {
		return "", fmt.Errorf("%w: %s", ErrNotSymlink, path)
	}
	return in.symlinkTarget, nil
}

// --- Mutating methods: SquashFS is a read-only archive format. ---

func (fs *FS) WriteFile(string, []byte, os.FileMode) error { return ErrReadOnly }
func (fs *FS) MkDir(string, os.FileMode) error             { return ErrReadOnly }
func (fs *FS) DeleteFile(string) error                     { return ErrReadOnly }
func (fs *FS) DeleteDir(string) error                      { return ErrReadOnly }
func (fs *FS) Rename(string, string) error                 { return ErrReadOnly }
