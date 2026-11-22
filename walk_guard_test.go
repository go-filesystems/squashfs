package squashfs

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-volumes/safeio"
)

// TestSymlinkFanoutIsRefused: a chain l0 -> "l1/l1", l1 -> "l2/l2", ...,
// l39 -> "." would cost 2^39 directory lookups for one ListDir("/l0") if only
// the depth were bounded. The TOTAL number of links followed in one lookup is
// bounded (as Linux does with MAXSYMLINKS), so the lookup is refused.
func TestSymlinkFanoutIsRefused(t *testing.T) {
	const depth = 40
	root := &node{name: "", mode: sIFDIR | 0o755}
	for k := 0; k < depth; k++ {
		target := "."
		if k < depth-1 {
			target = fmt.Sprintf("l%02d/l%02d", k+1, k+1)
		}
		root.children = append(root.children, &node{
			name: fmt.Sprintf("l%02d", k), mode: sIFLNK | 0o777, target: target,
		})
	}
	img, err := buildImage(root, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ListDir("/l39"); err != nil {
		t.Fatalf("control l39: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := fs.ListDir("/l00")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrTooManyLinks) || !errors.Is(err, safeio.ErrSymlinkLoop) {
			t.Fatalf("ListDir(/l00) = %v, want ErrTooManyLinks wrapping safeio.ErrSymlinkLoop", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("ListDir(/l00) still running after 20s: symlink fan-out is not bounded")
	}
}
