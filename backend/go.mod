module github.com/video-site/backend

go 1.23.0

toolchain go1.23.4

require (
	github.com/OpenListTeam/wopan-sdk-go v0.2.0
	github.com/SheltonZhu/115driver v1.3.2
	github.com/aliyun/aliyun-oss-go-sdk v3.0.2+incompatible
	github.com/go-chi/chi/v5 v5.1.0
	github.com/go-resty/resty/v2 v2.14.0
	golang.org/x/net v0.27.0
	golang.org/x/sys v0.30.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.33.1
)

require (
	github.com/aead/ecdh v0.2.0 // indirect
	github.com/andreburgaud/crypt2go v1.1.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/pierrec/lz4/v4 v4.1.17 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e // indirect
	golang.org/x/crypto v0.25.0 // indirect
	golang.org/x/time v0.8.0 // indirect
	modernc.org/gc/v3 v3.0.0-20240107210532-573471604cb6 // indirect
	modernc.org/libc v1.55.3 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
	modernc.org/strutil v1.2.0 // indirect
	modernc.org/token v1.1.0 // indirect
)

// 依赖已通过 go mod vendor 打包进 backend/vendor/ 并入库，支持离线构建。
// 升级 SDK 请使用标准流程：
//   go get github.com/SheltonZhu/115driver@<版本>
//   go mod tidy
//   go mod vendor
