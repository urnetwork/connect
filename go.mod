module github.com/urnetwork/connect

go 1.24.0

require (
	github.com/docopt/docopt-go v0.0.0-20180111231733-ee0de3bc6815
	github.com/go-playground/assert/v2 v2.2.0
	github.com/golang-jwt/jwt/v5 v5.2.1
	github.com/golang/glog v1.2.4
	github.com/google/gopacket v1.1.19
	github.com/gorilla/websocket v1.5.3
	github.com/oklog/ulid/v2 v2.1.0
	github.com/urnetwork/connect/protocol v0.0.0
	golang.org/x/crypto v0.35.0
	golang.org/x/exp v0.0.0-20250228200357-dead58393ab7
	golang.org/x/net v0.35.0
	golang.org/x/term v0.29.0
	google.golang.org/protobuf v1.36.5
	src.agwa.name/tlshacks v0.0.0-20231008131857-90d701ba3225
)

require (
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/text v0.22.0 // indirect
)

replace github.com/urnetwork/connect/protocol => ./protocol
