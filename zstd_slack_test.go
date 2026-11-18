package squashfs

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func slackSample(n int) []byte {
	r := rand.New(rand.NewPCG(1, 2))
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString("word")
		b.WriteByte(byte('a' + r.IntN(40)))
		b.WriteByte(' ')
	}
	return b.Bytes()[:n]
}

// The destination carries slack for the decoder's fast loop; the result must
// not expose it, and a block larger than maxOut by up to the slack is refused.
func TestZstdDecompress_SlackNeverLeaks(t *testing.T) {
	enc, _ := zstd.NewWriter(nil)
	plain := slackSample(128 << 10)
	src := enc.EncodeAll(plain, nil)
	got, err := (zstdDecompressor{}).decompress(src, len(plain))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip: %v", err)
	}
	if cap(got) != len(got) {
		t.Fatalf("cap %d exposes slack past %d", cap(got), len(got))
	}
	for d := 1; d <= zstdOverAlloc+1; d++ {
		if _, err := (zstdDecompressor{}).decompress(src, len(plain)-d); err == nil {
			t.Fatalf("block %d bytes over maxOut accepted", d)
		} else if errors.Is(err, nil) {
			t.Fatal("unreachable")
		}
	}
}

func BenchmarkZstdBlock128K(b *testing.B) {
	enc, _ := zstd.NewWriter(nil)
	plain := slackSample(128 << 10)
	src := enc.EncodeAll(plain, nil)
	b.SetBytes(int64(len(plain)))
	for i := 0; i < b.N; i++ {
		if _, err := (zstdDecompressor{}).decompress(src, len(plain)); err != nil {
			b.Fatal(err)
		}
	}
}
