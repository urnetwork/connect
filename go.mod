module github.com/urnetwork/connect

go 1.26.3

require (
	github.com/docopt/docopt-go v0.0.0-20180111231733-ee0de3bc6815
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/gopacket v1.1.19
	github.com/gorilla/websocket v1.5.3
	github.com/pion/datachannel v1.6.2
	github.com/pion/logging v0.2.4
	github.com/pion/transport/v4 v4.0.2
	github.com/pion/webrtc/v4 v4.2.15
	github.com/quic-go/quic-go v0.60.0
	github.com/urnetwork/glog v0.0.0
	github.com/wlynxg/anet v0.0.5
	golang.org/x/crypto v0.53.0
	golang.org/x/net v0.56.0
	golang.org/x/sys v0.46.0
	google.golang.org/protobuf v1.36.11
	gvisor.dev/gvisor v0.0.0-20260624000029-d10071d63566
	src.agwa.name/tlshacks v0.0.3
)

require (
	github.com/google/btree v1.1.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/pion/dtls/v3 v3.1.4 // indirect
	github.com/pion/ice/v4 v4.2.7 // indirect
	github.com/pion/interceptor v0.1.45 // indirect
	github.com/pion/mdns/v2 v2.1.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.16 // indirect
	github.com/pion/rtp v1.10.2 // indirect
	github.com/pion/sctp v1.10.0 // indirect
	github.com/pion/sdp/v3 v3.0.18 // indirect
	github.com/pion/srtp/v3 v3.0.11 // indirect
	github.com/pion/stun/v3 v3.1.5 // indirect
	github.com/pion/turn/v5 v5.0.9 // indirect
	golang.org/x/exp v0.0.0-20260611194520-c48552f49976 // indirect
	golang.org/x/text v0.38.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

retract [v0.0.1, v1.0.0]

retract v0.2.0 // retract self

replace github.com/urnetwork/glog => ../glog
