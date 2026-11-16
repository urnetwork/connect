package connect

// SetWindowSizeLimit composes default public windows with their owner's carrier
// capacity without replacing an explicit performance profile or fixed peer set.
// The original settings remain immutable, so returning to zero restores them.
func (self *RemoteUserNatMultiClient) SetWindowSizeLimit(limit int) {
	limit = max(0, limit)
	for _, window := range self.windows {
		func() {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			if window.windowSizeLimit != limit {
				window.windowSizeLimit = limit
				window.resizeMonitor.NotifyAll()
			}
		}()
	}
}

// Must be called with stateLock. Keep every non-size policy value; each reader
// uses the same capped copy, including speed's fixed target and route selection.
func (self *multiClientWindow) defaultWindowSizeWithLock() WindowSizeSettings {
	size := self.settings.WindowSizes[self.windowType]
	limit := self.windowSizeLimit
	if limit <= 0 {
		return size
	}
	minimumLimit := limit
	if self.windowType == WindowTypeQuality {
		// Both classes share the owner. Leave room for speed to retain one
		// fewer than its minimum without stranding quality below its own.
		speed := self.settings.WindowSizes[WindowTypeSpeed]
		speedMinimum := speed.WindowSizeMin
		if speed.FixedWindowSize > 0 {
			speedMinimum = speed.FixedWindowSize
		}
		minimumLimit = max(1, limit-max(0, min(limit, speedMinimum)-1))
	}
	size.WindowSizeMin = min(size.WindowSizeMin, minimumLimit)
	size.WindowSizeMax = min(size.WindowSizeMax, limit)
	if size.WindowSizeHardMax <= 0 {
		size.WindowSizeHardMax = limit
	} else {
		size.WindowSizeHardMax = min(size.WindowSizeHardMax, limit)
	}
	size.FixedWindowSize = min(size.FixedWindowSize, minimumLimit)
	size.WindowSizeMinP2pOnly = min(size.WindowSizeMinP2pOnly, limit)
	size.WindowSizeMinIpv6Capable = min(size.WindowSizeMinIpv6Capable, limit)
	size.KeepHealthiestCount = min(size.KeepHealthiestCount, limit)
	return size
}
