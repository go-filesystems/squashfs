package squashfs

// The Linux kernel and squashfs-tools as the judge of this driver, in a Tart
// VM. Skipped unless FS_KERNEL_JUDGE=1; github.com/go-filesystems/kerneljudge's
// cmd/kerneljudge sets it and runs this as root in the VM:
//
//	kerneljudge run --driver <clone of this repository> [--perf]

import (
	"testing"

	filesystem "github.com/go-filesystems/interface"
	kj "github.com/go-filesystems/kerneljudge"
)

func kernelJudgeDriver() kj.Driver {
	build := func(o BuildOptions) func(img, src string) error {
		return func(img, src string) error { return BuildFromDir(img, src, o) }
	}
	native := func(comp string) kj.Native {
		return kj.Native{Name: "mksquashfs-" + comp, Args: []string{"mksquashfs", "{src}", "{img}", "-comp", comp, "-quiet", "-no-progress", "-noappend"}}
	}
	// BuildFromDir documents "no xattrs"; everything else it is held to.
	builderCaps := kj.Caps{Symlinks: true, Sparse: true, Mtime: true}
	return kj.Driver{
		Name:   "squashfs",
		FSType: "squashfs",
		Open: func(img string) (filesystem.Filesystem, error) {
			f, err := OpenFile(img)
			if err != nil {
				return nil, err
			}
			return f, nil
		},
		Builders: []kj.Builder{
			{Name: "gzip", Build: build(BuildOptions{})},
			{Name: "none", Build: build(BuildOptions{Uncompressed: true})},
			{Name: "zstd", Build: build(BuildOptions{Compressor: CompressZSTD})},
			{Name: "xz", Build: build(BuildOptions{Compressor: CompressXZ})},
			{Name: "lz4", Build: build(BuildOptions{Compressor: CompressLZ4})},
		},
		BuilderCaps: &builderCaps,
		Natives: []kj.Native{native("gzip"), native("lzo"), native("lz4"), native("xz"), native("zstd"),
			{Name: "mksquashfs-none", Args: []string{"mksquashfs", "{src}", "{img}", "-noI", "-noD", "-noF", "-noX", "-quiet", "-no-progress", "-noappend"}}},
		Fsck: [][]string{
			{"unsquashfs", "-lls", "{img}"},
			{"unsquashfs", "-stat", "{img}"},
		},
		Compare: kj.Compare{Mtime: true, Xattrs: true, Perm: true},
	}
}

func TestKernelJudge(t *testing.T)     { kj.Run(t, kernelJudgeDriver()) }
func TestKernelJudgePerf(t *testing.T) { kj.RunPerf(t, kernelJudgeDriver(), kj.PerfOptions{}) }
