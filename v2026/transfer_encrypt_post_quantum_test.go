package connect

import (
	"context"
	"crypto/tls"
	"testing"
)

// The receiver-role TLS config has to offer the X25519MLKEM768 hybrid for a
// session to be post-quantum. A caller-supplied config that leaves it out
// serves classical sessions only, and does not advertise.
func TestServesPostQuantumEncryptionNeedsTheHybridGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cert, err := generateSequenceTlsCertificate()
	AssertEqual(t, err, nil)
	classical := &tls.Config{
		Certificates:     []tls.Certificate{cert},
		ClientAuth:       tls.RequireAnyClientCert,
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519},
	}
	client := testingProvideClient(t, ctx, EncryptionModeOpportunistic, classical)
	AssertEqual(t, client.EncryptionSessionManager().ServesPostQuantumEncryption(), false)
	frame, err := client.ContractManager().provideFrame()
	AssertEqual(t, err, nil)
	advertised, _ := testingProvidePostQuantumEncryption(t, frame)
	AssertEqual(t, advertised, false)

	// the crypto/tls default list leads with the hybrid
	hybridDefault := classical.Clone()
	hybridDefault.CurvePreferences = nil
	client = testingProvideClient(t, ctx, EncryptionModeOpportunistic, hybridDefault)
	AssertEqual(t, client.EncryptionSessionManager().ServesPostQuantumEncryption(), true)
}
