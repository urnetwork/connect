module github.com/urnetwork/connect

go 1.24.4

toolchain go1.24.5

require (
	github.com/docopt/docopt-go v0.0.0-20180111231733-ee0de3bc6815
	github.com/go-playground/assert/v2 v2.2.0
	github.com/golang-jwt/jwt/v5 v5.3.0
	github.com/urnetwork/glog v0.0.0
	github.com/google/gopacket v1.1.19
	github.com/gorilla/websocket v1.5.3
	github.com/oklog/ulid/v2 v2.1.1
	github.com/quic-go/quic-go v0.55.0
	golang.org/x/crypto v0.43.0
	golang.org/x/exp v0.0.0-20251009144603-d2f985daa21b
	golang.org/x/net v0.46.0
	golang.org/x/term v0.36.0
	google.golang.org/protobuf v1.36.10
	src.agwa.name/tlshacks v0.0.0-20250628001001-c92050511ef4
)

require (
	go.uber.org/mock v0.6.0 // indirect
	golang.org/x/mod v0.29.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.37.0 // indirect
	golang.org/x/text v0.30.0 // indirect
	golang.org/x/tools v0.38.0 // indirect
)

retract [v0.0.1, v1.0.0]

retract v0.2.0 // retract self

replace github.com/urnetwork/glog => ../glog
