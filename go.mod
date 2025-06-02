module github.com/urnetwork/connect

go 1.24.0

require (
	github.com/docopt/docopt-go v0.0.0-20180111231733-ee0de3bc6815
	github.com/go-playground/assert/v2 v2.2.0
	github.com/golang-jwt/jwt/v5 v5.2.2
	github.com/golang/glog v1.2.5
	github.com/google/gopacket v1.1.19
	github.com/gorilla/websocket v1.5.3
	github.com/oklog/ulid/v2 v2.1.1
	github.com/quic-go/quic-go v0.52.0
	golang.org/x/crypto v0.38.0
	golang.org/x/exp v0.0.0-20250531010427-b6e5de432a8b
	golang.org/x/net v0.40.0
	golang.org/x/term v0.32.0
	google.golang.org/protobuf v1.36.6
	src.agwa.name/tlshacks v0.0.0-20231008131857-90d701ba3225
)

require (
	github.com/go-task/slim-sprig/v3 v3.0.0 // indirect
	github.com/google/pprof v0.0.0-20250602020802-c6617b811d0e // indirect
	github.com/onsi/ginkgo/v2 v2.23.4 // indirect
	go.uber.org/automaxprocs v1.6.0 // indirect
	go.uber.org/mock v0.5.2 // indirect
	golang.org/x/mod v0.24.0 // indirect
	golang.org/x/sync v0.14.0 // indirect
	golang.org/x/sys v0.33.0 // indirect
	golang.org/x/text v0.25.0 // indirect
	golang.org/x/tools v0.33.0 // indirect

)

retract [v0.0.1, v1.0.0]

retract v0.2.0 // retract self
