module github.com/go-filesystems/squashfs

go 1.25.0

require (
	github.com/anchore/go-lzo v0.1.0
	github.com/go-filesystems/interface v0.0.0
	github.com/klauspost/compress v1.17.9
	github.com/ulikunitz/xz v0.5.12
)

replace github.com/go-filesystems/interface => ../interface
