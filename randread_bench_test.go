// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package squashfs

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Random 4 KiB reads over a file much larger than the data cache (squashfs#61):
// 64 MiB of text-like data is 512 blocks of 128 KiB, read at 5000 fixed
// pseudo-random offsets, the same ratio of reads to blocks as the kernel
// judge's 20000 reads over 256 MiB.
const (
	randFileBytes = 64 << 20
	randPasses    = 5000
)

var (
	randImgOnce sync.Once
	randImgPath string
	randImgErr  error
)

func randImage(b *testing.B) string {
	b.Helper()
	randImgOnce.Do(func() {
		dir, err := os.MkdirTemp("", "squashfs-randread")
		if err != nil {
			randImgErr = err
			return
		}
		src := filepath.Join(dir, "src")
		os.MkdirAll(src, 0o755)
		r := rand.New(rand.NewPCG(42, 43))
		vocab := make([][]byte, 6000)
		for i := range vocab {
			w := make([]byte, 3+r.IntN(8))
			for j := range w {
				w[j] = byte('a' + r.IntN(26))
			}
			vocab[i] = w
		}
		buf := make([]byte, 0, randFileBytes)
		for len(buf) < randFileBytes-16 {
			k := int(float64(len(vocab)) * r.Float64() * r.Float64() * r.Float64())
			buf = append(buf, vocab[k]...)
			buf = append(buf, ' ')
		}
		for len(buf) < randFileBytes {
			buf = append(buf, ' ')
		}
		if randImgErr = os.WriteFile(filepath.Join(src, "large.bin"), buf, 0o644); randImgErr != nil {
			return
		}
		randImgPath = filepath.Join(dir, "r.img")
		randImgErr = BuildFromDir(randImgPath, src, BuildOptions{})
	})
	if randImgErr != nil {
		b.Fatal(randImgErr)
	}
	return randImgPath
}

func randPass(b *testing.B, fs *FS) {
	b.Helper()
	f, err := fs.OpenFile("/large.bin")
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	rng := rand.New(rand.NewPCG(9, 9))
	buf := make([]byte, 4096)
	for range randPasses {
		off := int64(rng.IntN(randFileBytes/4096)) * 4096
		if _, err := f.ReadAt(buf, off); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRandRead4K: cold is a fresh FS (empty cache) per pass; warm is the
// second pass of the same FS.
func BenchmarkRandRead4K(b *testing.B) {
	img := randImage(b)
	b.Run("cold", func(b *testing.B) {
		for b.Loop() {
			fs, err := OpenFile(img)
			if err != nil {
				b.Fatal(err)
			}
			randPass(b, fs)
			fs.Close()
		}
		b.ReportMetric(float64(b.N)*randPasses/b.Elapsed().Seconds(), "reads/s")
	})
	b.Run("warm", func(b *testing.B) {
		fs, err := OpenFile(img)
		if err != nil {
			b.Fatal(err)
		}
		defer fs.Close()
		randPass(b, fs)
		b.ResetTimer()
		for b.Loop() {
			randPass(b, fs)
		}
		b.ReportMetric(float64(b.N)*randPasses/b.Elapsed().Seconds(), "reads/s")
	})
}
