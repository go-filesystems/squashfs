module github.com/go-filesystems/squashfs

go 1.26.0

require (
	github.com/anchore/go-lzo v0.1.0
	github.com/go-filesystems/interface v0.0.0
	github.com/klauspost/compress v1.17.9
	github.com/ulikunitz/xz v0.5.12
)

require github.com/pierrec/lz4/v4 v4.1.22

replace github.com/go-filesystems/interface => ../interface
