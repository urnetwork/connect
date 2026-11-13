package connect

// Explicit fixed single-hop ownership is required; ordinary discovery and
// general consumer windows retain their existing provider qualification.
func dataOnlyProviderProbeEnabled(settings *MultiClientSettings, generator MultiClientGenerator, args *multiClientChannelArgs) bool {
	if settings == nil || !settings.DataOnlyProviderProbe || settings.ProviderProbe ||
		generator == nil || args == nil || !args.FixedDestination || args.Destination.Len() != 1 {
		return false
	}
	size, fixed := generator.FixedDestinationSize()
	return fixed && size == 1
}
