module github.com/beeper/babbleserv

go 1.24.3

require (
	github.com/apple/foundationdb/bindings/go v0.0.0-20230127234245-2d0427521d8f
	github.com/beeper/libserv v0.0.0-20231231202820-c7303abfc32c
	github.com/go-chi/chi/v5 v5.0.12
	github.com/jedib0t/go-pretty/v6 v6.7.5
	github.com/matrix-org/gomatrix v0.0.0-20210324163249-be2af5ef2e16
	github.com/matrix-org/gomatrixserverlib v0.0.0-20240328203753-c2391f7113a5
	github.com/minio/minio-go/v7 v7.0.74
	github.com/redis/go-redis/v9 v9.5.4
	github.com/rs/xid v1.6.0
	github.com/rs/zerolog v1.34.0
	github.com/samber/lo v1.52.0
	github.com/stretchr/testify v1.11.1
	github.com/tidwall/gjson v1.18.0
	github.com/tidwall/sjson v1.2.5
	github.com/vmihailenco/msgpack/v5 v5.4.1
	go.mau.fi/util v0.9.4-0.20251211121531-f6527b4882ae
	golang.org/x/crypto v0.46.0
	golang.org/x/exp v0.0.0-20251209150349-8475f28825e9
	gopkg.in/yaml.v3 v3.0.1
	maunium.net/go/mautrix v0.26.1-0.20251219122505-b4c9faa5e20e
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-ini/ini v1.67.0 // indirect
	github.com/goccy/go-json v0.10.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/klauspost/cpuid/v2 v2.2.8 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/matrix-org/util v0.0.0-20200807132607-55161520e1d4 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.16 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/petermattis/goid v0.0.0-20251121121749-a11dd1a45f9a // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/sirupsen/logrus v1.9.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/text v0.32.0 // indirect
	golang.org/x/xerrors v0.0.0-20231012003039-104605ab7028 // indirect
)

// replace maunium.net/go/mautrix => ./external/mautrix
