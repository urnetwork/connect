package connect

import (
	"context"
	"fmt"
	"testing"
)

// testingRecordingGenerator records the client settings a window client is
// created with, and fails the creation so no real client spins up.
type testingRecordingGenerator struct {
	testingEmptyMultiClientGenerator
	clientSettings *ClientSettings
}

func (self *testingRecordingGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	self.clientSettings = clientSettings
	return nil, fmt.Errorf("no clients")
}

// TestMultiClientChannelPqe verifies the profile's post-quantum encryption
// setting enables the e2e encryption sessions on the window clients, and
// stays off otherwise.
func TestMultiClientChannelPqe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	newChannelClientSettings := func(performanceProfile *PerformanceProfile) *ClientSettings {
		generator := &testingRecordingGenerator{}
		_, err := newMultiClientChannel(
			ctx,
			&multiClientChannelArgs{},
			generator,
			nil,
			nil,
			nil,
			nil,
			func() {},
			performanceProfile,
			DefaultMultiClientSettings(),
		)
		// the recording generator fails creation after settings are applied
		AssertEqual(t, true, err != nil)
		AssertEqual(t, true, generator.clientSettings != nil)
		return generator.clientSettings
	}

	// no profile: encryption stays off
	clientSettings := newChannelClientSettings(nil)
	AssertEqual(t, false, clientSettings.EncryptionSettings.Encrypt)

	// profile without pqe: encryption stays off
	clientSettings = newChannelClientSettings(&PerformanceProfile{
		WindowType:  WindowTypeAuto,
		AllowDirect: true,
	})
	AssertEqual(t, false, clientSettings.EncryptionSettings.Encrypt)

	// pqe on an auto profile enables the e2e sessions
	clientSettings = newChannelClientSettings(&PerformanceProfile{
		WindowType:            WindowTypeAuto,
		PostQuantumEncryption: true,
	})
	AssertEqual(t, true, clientSettings.EncryptionSettings.Encrypt)

	// pqe on a fixed profile enables the e2e sessions
	clientSettings = newChannelClientSettings(&PerformanceProfile{
		WindowType:            WindowTypeSpeed,
		WindowSize:            DefaultWindowSizeSettings(),
		PostQuantumEncryption: true,
	})
	AssertEqual(t, true, clientSettings.EncryptionSettings.Encrypt)
}
