package connect

import "context"

// Each window inherits only its generator's immutable telemetry claim. Shared
// strategies, APIs and unrelated generators remain unmarked by default.
func (self *ApiMultiClientGenerator) newClientOob(ctx context.Context, byJwt string) *ApiOutOfBandControl {
	owner := newApiOutOfBandControl(ctx, self.clientStrategy, byJwt, self.apiUrl, self.controlTelemetryProbe)
	owner.localControl = self.clientControl
	return owner
}
