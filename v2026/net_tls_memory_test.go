package connect

import "testing"

func TestDefaultTlsConfigSharesRootsButNotSessionCaches(t *testing.T) {
	first, err := DefaultTlsConfig()
	if err != nil {
		t.Fatalf("first DefaultTlsConfig: %v", err)
	}
	second, err := DefaultTlsConfig()
	if err != nil {
		t.Fatalf("second DefaultTlsConfig: %v", err)
	}
	if first.RootCAs == nil || first.RootCAs != second.RootCAs {
		t.Fatal("default TLS configs did not reuse the immutable pinned roots")
	}
	if first.ClientSessionCache == nil || first.ClientSessionCache == second.ClientSessionCache {
		t.Fatal("default TLS configs must retain independent session caches")
	}

	// Preserve PinnedCertPool's public fresh/mutable-pool contract.
	freshFirst, err := PinnedCertPool()
	if err != nil {
		t.Fatalf("first PinnedCertPool: %v", err)
	}
	freshSecond, err := PinnedCertPool()
	if err != nil {
		t.Fatalf("second PinnedCertPool: %v", err)
	}
	if freshFirst == freshSecond || freshFirst == first.RootCAs || freshSecond == first.RootCAs {
		t.Fatal("PinnedCertPool returned a shared default pool")
	}
}
