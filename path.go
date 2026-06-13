// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"fmt"
	"strings"
)

// maxSymlinkHops bounds symlink resolution per path lookup.
const maxSymlinkHops = 40

// resolve walks an absolute path from the root inode. When followFinal is
// true a symlink at the final component is followed; otherwise the symlink
// inode itself is returned (lstat semantics). Intermediate symlinks are
// always followed.
func resolve(fs *FS, path string, followFinal bool) (*inode, error) {
	root, err := readInode(fs, fs.sb.RootInode)
	if err != nil {
		return nil, err
	}
	return resolveFrom(fs, root, path, followFinal, 0)
}

func resolveFrom(fs *FS, start *inode, path string, followFinal bool, hops int) (*inode, error) {
	if hops > maxSymlinkHops {
		return nil, ErrTooManyLinks
	}
	parts := splitPath(path)
	cur := start
	for i, name := range parts {
		if !cur.isDir() {
			return nil, fmt.Errorf("%w: %q", ErrNotDirectory, name)
		}
		entries, err := readDir(fs, cur)
		if err != nil {
			return nil, err
		}
		var child *inode
		for _, e := range entries {
			if e.Name == name {
				child, err = readInode(fs, e.InodeRef)
				if err != nil {
					return nil, err
				}
				break
			}
		}
		if child == nil {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
		}
		last := i == len(parts)-1
		if child.isSymlink() && (!last || followFinal) {
			target := child.symlinkTarget
			base := cur // relative links resolve against the containing dir
			if strings.HasPrefix(target, "/") {
				base, err = readInode(fs, fs.sb.RootInode)
				if err != nil {
					return nil, err
				}
			}
			child, err = resolveFrom(fs, base, target, true, hops+1)
			if err != nil {
				return nil, err
			}
		}
		cur = child
	}
	return cur, nil
}

// splitPath normalises an absolute path into its non-empty components,
// dropping "." and empty segments. ".." is preserved as a literal component
// (SquashFS directories don't store ".." entries we can follow, so callers
// should pass already-clean paths; this keeps behaviour predictable).
func splitPath(p string) []string {
	raw := strings.Split(p, "/")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if s == "" || s == "." {
			continue
		}
		out = append(out, s)
	}
	return out
}
