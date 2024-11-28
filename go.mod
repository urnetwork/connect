module github.com/urnetwork/connect

go 1.23.1

require (
	github.com/docopt/docopt-go v0.0.0-20180111231733-ee0de3bc6815
	github.com/gin-gonic/gin v1.10.0
	github.com/go-playground/assert/v2 v2.2.0
	github.com/golang-jwt/jwt/v5 v5.2.0
	github.com/golang/glog v1.2.1
	github.com/google/gopacket v1.1.19
	github.com/gorilla/websocket v1.5.0
	github.com/mattn/go-shellwords v1.0.12
	github.com/oklog/ulid/v2 v2.1.0
	github.com/quic-go/quic-go v0.46.0
	github.com/urnetwork/protocol v0.0.0
	github.com/urnetwork/userwireguard v0.0.0
	golang.org/x/exp v0.0.0-20240506185415-9bf2ced13842
	golang.org/x/term v0.25.0
	golang.zx2c4.com/wireguard/wgctrl v0.0.0-20230429144221-925a1e7659e6
	google.golang.org/protobuf v1.34.2
	src.agwa.name/tlshacks v0.0.0-20231008131857-90d701ba3225
)

require (
	github.com/cespare/xxhash/v2 v2.2.0
	github.com/jellydator/ttlcache/v3 v3.3.0
	github.com/stretchr/testify v1.9.0
	golang.org/x/crypto v0.28.0
	golang.org/x/net v0.30.0
	golang.org/x/sync v0.8.0
	gvisor.dev/gvisor v0.0.0-20241127223613-65a7fdf8cf17
)

require (
	github.com/go-task/slim-sprig v0.0.0-20230315185526-52ccab3ef572 // indirect
	github.com/google/pprof v0.0.0-20210407192527-94a9f03dee38 // indirect
	github.com/onsi/ginkgo/v2 v2.9.5 // indirect
	go.uber.org/mock v0.4.0 // indirect
	golang.org/x/mod v0.21.0 // indirect
	golang.org/x/tools v0.26.0 // indirect
)

require (
	github.com/bytedance/sonic v1.11.6 // indirect
	github.com/bytedance/sonic/loader v0.1.1 // indirect
	github.com/cloudwego/base64x v0.1.4 // indirect
	github.com/cloudwego/iasm v0.2.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/gabriel-vasile/mimetype v1.4.3 // indirect
	github.com/gin-contrib/sse v0.1.0 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.20.0 // indirect
	github.com/goccy/go-json v0.10.2 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/cpuid/v2 v2.2.7 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/pelletier/go-toml/v2 v2.2.2 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/ugorji/go/codec v1.2.12 // indirect
	golang.org/x/arch v0.8.0 // indirect
	golang.org/x/sys v0.26.0 // indirect
	golang.org/x/text v0.19.0 // indirect
	golang.org/x/time v0.7.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/urnetwork/protocol v0.0.0 => ../protocol

replace github.com/urnetwork/userwireguard v0.0.0 => ../userwireguard
