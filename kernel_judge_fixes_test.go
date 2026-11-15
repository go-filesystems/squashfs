package squashfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// Pins for the defects go-filesystems/kerneljudge found against the Linux
// kernel and squashfs-tools (#42, #43, #44, #45, #46).

func buildJudgeImage(t *testing.T, opts BuildOptions) (img string, when time.Time) {
	t.Helper()
	src := t.TempDir()
	when = time.Date(2021, 3, 4, 9, 6, 8, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(src, "f"), when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", filepath.Join(src, "ln")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing/nowhere", filepath.Join(src, "dangling")); err != nil {
		t.Fatal(err)
	}
	img = filepath.Join(t.TempDir(), "a.sqsh")
	if err := BuildFromDir(img, src, opts); err != nil {
		t.Fatal(err)
	}
	return img, when
}

func TestBuiltImageIsPaddedTo4096(t *testing.T) {
	for _, o := range []BuildOptions{{}, {Uncompressed: true}, {Compressor: CompressXZ}, {Compressor: CompressZSTD}, {Compressor: CompressLZ4}} {
		img, _ := buildJudgeImage(t, o)
		fi, _ := os.Stat(img)
		if fi.Size()%4096 != 0 {
			t.Errorf("%+v: image is %d bytes, not a multiple of 4096", o, fi.Size())
		}
		fs, err := OpenFile(img)
		if err != nil {
			t.Fatal(err)
		}
		if bu := fs.Superblock().BytesUsed; bu > uint64(fi.Size()) || uint64(fi.Size())-bu >= 4096 {
			t.Errorf("bytes_used %d, file %d: it must stay the unpadded end", bu, fi.Size())
		}
		fs.Close()
	}
}

func TestBuildKeepsModificationTimes(t *testing.T) {
	img, when := buildJudgeImage(t, BuildOptions{})
	fs, err := OpenFile(img)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	ft, err := fs.Times("/f")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := ft.Mtime.Get(); !ok || !got.Equal(when) {
		t.Errorf("mtime %v (recorded %v), want %v", got, ok, when)
	}
	// mod_time is the newest inode (the source directory itself is newer).
	if fs.Superblock().ModTime < uint32(when.Unix()) {
		t.Errorf("superblock mod_time %d is older than an inode (%d)", fs.Superblock().ModTime, when.Unix())
	}
}

func TestStatDoesNotFollowTheFinalSymlink(t *testing.T) {
	img, _ := buildJudgeImage(t, BuildOptions{})
	fs, err := OpenFile(img)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	st, err := fs.Stat("/ln")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0xF000 != 0xA000 || st.Size() != 1 {
		t.Errorf("Stat(/ln): mode %#o size %d, want a symlink of length 1", st.Mode(), st.Size())
	}
	if st, err := fs.Stat("/dangling"); err != nil || st.Mode()&0xF000 != 0xA000 {
		t.Errorf("a dangling link must be statable as a link: %v", err)
	}
	if target, err := fs.ReadLink("/dangling"); err != nil || target != "missing/nowhere" {
		t.Errorf("ReadLink: %q, %v", target, err)
	}
	var _ filesystem.TimesReader = fs
}
