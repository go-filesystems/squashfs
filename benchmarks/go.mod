// Nested module: isolates the benchmark harness (a standalone main package)
// from the library's go.mod so it is NOT part of `go list ./...` and never
// affects the coverage floor. See BENCHMARKS.md.
module github.com/go-filesystems/squashfs/benchmarks

go 1.27.4

require github.com/go-filesystems/squashfs v0.9.2

require (
	github.com/go-compressions/lz4 v0.5.1 // indirect
	github.com/go-compressions/lzo v0.4.0 // indirect
	github.com/go-compressions/xz v0.1.0 // indirect
	github.com/go-filesystems/inflight v0.5.0 // indirect
	github.com/go-filesystems/interface v0.6.0 // indirect
	github.com/go-simd/matchlen v0.5.0 // indirect
	github.com/go-volumes/safeio v0.0.0-20261011024942-5635ea21db3b // indirect
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/ulikunitz/xz v0.5.17 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/go-filesystems/squashfs => ..
