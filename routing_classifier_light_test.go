package connect

import (
	"net"
	"net/netip"
	"testing"
)

// fakeServerNameResolver adapts a plain ip-string -> hostname map to
// ServerNameResolver for tests.
func fakeServerNameResolver(names map[string]string) ServerNameResolver {
	return func(ip netip.Addr) (string, bool) {
		name, ok := names[ip.String()]
		return name, ok
	}
}

func testLightIpPath(protocol IpProtocol, destIp string, destPort int) *IpPath {
	return &IpPath{
		Protocol:        protocol,
		SourceIp:        net.ParseIP("192.168.1.2"),
		SourcePort:      51000,
		DestinationIp:   net.ParseIP(destIp),
		DestinationPort: destPort,
	}
}

func TestLightClassifier(t *testing.T) {
	resolver := fakeServerNameResolver(map[string]string{
		"93.184.216.1": "netflix.com",
	})
	classifier := NewLightClassifier(resolver)

	tests := []struct {
		name   string
		ipPath *IpPath
		appId  string
		want   TrafficClass
	}{
		{
			name:   "server name beats port default: streaming",
			ipPath: testLightIpPath(IpProtocolTcp, "93.184.216.1", 443),
			want:   ClassStreaming,
		},
		{
			name:   "no resolvable name falls through to port default: browsing",
			ipPath: testLightIpPath(IpProtocolTcp, "93.184.216.2", 443),
			want:   ClassBrowsing,
		},
		{
			name:   "stun udp port: latency",
			ipPath: testLightIpPath(IpProtocolUdp, "8.8.8.8", 3478),
			want:   ClassLatency,
		},
		{
			name:   "bittorrent default port: bulk",
			ipPath: testLightIpPath(IpProtocolTcp, "10.0.0.5", 51413),
			want:   ClassBulk,
		},
		{
			name:   "unmatched high port with no name and no app never guesses: unknown",
			ipPath: testLightIpPath(IpProtocolTcp, "10.0.0.6", 54321),
			want:   ClassUnknown,
		},
		{
			name:   "app default beats server name",
			ipPath: testLightIpPath(IpProtocolTcp, "93.184.216.1", 443),
			appId:  "steam.exe",
			want:   ClassBulk,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifier.Classify(tt.ipPath, tt.appId)
			if got.Class != tt.want {
				t.Fatalf("Classify() class = %v, want %v", got.Class, tt.want)
			}
			if got.AppId != tt.appId {
				t.Fatalf("Classify() dropped appId: got %q want %q", got.AppId, tt.appId)
			}
		})
	}
}

// TestLightClassifierNilResolver proves the classifier is safe to construct
// with a nil ServerNameResolver (the install site may not always have one)
// and that it does not panic and still falls through to the port tier.
func TestLightClassifierNilResolver(t *testing.T) {
	classifier := NewLightClassifier(nil)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Classify panicked with nil resolver: %v", r)
		}
	}()

	got := classifier.Classify(testLightIpPath(IpProtocolTcp, "93.184.216.1", 443), "")
	if got.Class != ClassBrowsing {
		t.Fatalf("nil resolver: class = %v, want ClassBrowsing (falls through to port)", got.Class)
	}
}

// TestLightClassifierNilIpPath proves Classify never panics on a nil
// *IpPath (defensive: it is a table lookup on the placement hot path, where a
// panic would be worse than an unknown classification).
func TestLightClassifierNilIpPath(t *testing.T) {
	classifier := NewLightClassifier(nil)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Classify panicked with nil IpPath: %v", r)
		}
	}()

	got := classifier.Classify(nil, "chrome.exe")
	if got.Class != ClassUnknown {
		t.Fatalf("nil IpPath: class = %v, want ClassUnknown", got.Class)
	}
}
