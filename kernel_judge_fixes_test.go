package squashfs

import (
	"bytes"
	"fmt"
	"io"
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
		fi, serr := os.Stat(img)
		if serr != nil {
			t.Fatal(serr)
		}
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

type countingReaderAt struct {
	r io.ReaderAt
	n int
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.n++
	return c.r.ReadAt(p, off)
}

// Small files share a fragment block; when that block is stored uncompressed
// (incompressible data, or an uncompressed image) it used to be read from the
// device again for every file (#49).
func TestFragmentBlockStoredRawIsReadOnce(t *testing.T) {
	src := t.TempDir()
	const files = 60
	for i := 0; i < files; i++ {
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("f%03d", i)), bytes.Repeat([]byte{byte(i + 1)}, 100+i), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img := filepath.Join(t.TempDir(), "a.sqsh")
	if err := BuildFromDir(img, src, BuildOptions{Uncompressed: true}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	cr := &countingReaderAt{r: bytes.NewReader(raw)}
	fs, err := Open(cr, int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if fs.Superblock().FragCount == 0 {
		t.Fatal("the files were not packed into a fragment")
	}
	for i := 0; i < files; i++ { // warm the metadata cache and the fragment
		if _, err := fs.ReadFile(fmt.Sprintf("/f%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	before := cr.n
	for i := 0; i < files; i++ {
		got, err := fs.ReadFile(fmt.Sprintf("/f%03d", i))
		if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{byte(i + 1)}, 100+i)) {
			t.Fatalf("f%03d: %v", i, err)
		}
	}
	if reads := cr.n - before; reads != 0 {
		t.Errorf("re-reading %d small files cost %d device reads: the raw fragment block or the fragment-table entry (the 8-byte pointer) is not cached", files, reads)
	}
}

// A crafted image can name one offset as a compressed block (through one inode)
// and as a raw block (through another, or a fragment entry). The data cache was
// keyed by offset alone and served the bytes of whichever was read first.
func TestDataCacheDistinguishesHowABlockIsRead(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f"), bytes.Repeat([]byte("a"), 140000), 0o644); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(t.TempDir(), "a.sqsh")
	if err := BuildFromDir(img, src, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	fs, err := OpenFile(img)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	in, err := resolve(fs, "/f", true)
	if err != nil {
		t.Fatal(err)
	}
	var off int64 = int64(in.blocksStart)
	var word uint32
	for _, sz := range in.blockSizes {
		if n, compressed := blockOnDiskSize(sz); n > 0 && compressed {
			word = sz
			break
		}
		n, _ := blockOnDiskSize(sz)
		off += n
	}
	if word == 0 {
		t.Skip("no compressed block in this image")
	}
	n, _ := blockOnDiskSize(word)
	plain, err := fs.readBlockOpt(off, word, true)
	if err != nil || int64(len(plain)) == n {
		t.Fatalf("the compressed block did not expand: %d bytes (on disk %d), %v", len(plain), n, err)
	}
	// The same offset and length, read as a RAW block.
	forged, err := fs.readBlockOpt(off, uint32(n)|dataUncompressedBit, true)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(forged)) != n {
		t.Fatalf("a raw read of the block returned %d bytes: the cached decompressed block of the same offset was served (want the %d raw bytes)", len(forged), n)
	}
}
