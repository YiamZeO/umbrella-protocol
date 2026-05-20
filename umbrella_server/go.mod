module umbrella_server

go 1.25.6

require (
	github.com/anacrolix/utp v0.2.0
	github.com/apernet/hysteria/core/v2 v2.8.2
	github.com/apernet/hysteria/extras/v2 v2.8.2
	github.com/hashicorp/yamux v0.1.2
	github.com/xtls/reality v0.0.0-20251116175510-cd53f7d50237
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/anacrolix/missinggo v1.3.0 // indirect
	github.com/anacrolix/missinggo/perf v1.0.0 // indirect
	github.com/anacrolix/missinggo/v2 v2.5.1 // indirect
	github.com/anacrolix/sync v0.4.0 // indirect
	github.com/andybalholm/brotli v1.1.0 // indirect
	github.com/apernet/quic-go v0.59.1-0.20260425001925-6c6cc9bcb716 // indirect
	github.com/cloudflare/circl v1.6.1 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/huandu/xstrings v1.3.1 // indirect
	github.com/juju/ratelimit v1.0.2 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/pires/go-proxyproto v0.8.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/refraction-networking/utls v1.8.1 // indirect
	github.com/stretchr/objx v0.5.2 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	golang.org/x/crypto v0.47.0 // indirect
	golang.org/x/exp v0.0.0-20240506185415-9bf2ced13842 // indirect
	golang.org/x/net v0.49.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/text v0.34.0 // indirect
)

replace github.com/anacrolix/utp => github.com/Haonixao/utp v0.0.0-20260512111648-6d2d2fc87c8c

replace github.com/apernet/hysteria/core/v2 => github.com/Haonixao/hysteria/core/v2 v2.0.0-20260519181509-74e6ca42097b

replace github.com/apernet/hysteria/extras/v2 => github.com/Haonixao/hysteria/extras/v2 v2.0.0-20260519181509-74e6ca42097b
