module github.com/urnetwork/connect

go 1.24.0

require (
	github.com/docopt/docopt-go v0.0.0-20180111231733-ee0de3bc6815
	github.com/go-playground/assert/v2 v2.2.0
	github.com/golang-jwt/jwt/v5 v5.2.2
	github.com/golang/glog v1.2.5
	github.com/google/gopacket v1.1.19
	github.com/gorilla/websocket v1.5.3
	github.com/oklog/ulid/v2 v2.1.0
	golang.org/x/crypto v0.38.0
	golang.org/x/exp v0.0.0-20250506013437-ce4c2cf36ca6
	golang.org/x/net v0.40.0
	golang.org/x/term v0.32.0
	google.golang.org/protobuf v1.36.6
	src.agwa.name/tlshacks v0.0.0-20231008131857-90d701ba3225
)

require (
	golang.org/x/sys v0.33.0 // indirect
	golang.org/x/text v0.25.0 // indirect
)

retract [v0.0.1, v1.0.0]

retract v0.2.0 // retract self
