// Cross-window selection reads exact-service history without changing the
// existing filter's sole-candidate or all-failed fallback behavior.
package connect

import "time"

// Reports whether every otherwise eligible candidate has a current failure
// for this exact service. Empty or malformed fields provide no such evidence.
func (self *RemoteUserNatMultiClient) destinationServiceFailuresExhausted(
	candidates []*multiClientChannel,
	ipPath *IpPath,
) bool {
	if len(candidates) == 0 || ipPath == nil {
		return false
	}
	now := time.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, client := range candidates {
		key, ok := destinationServiceFailureKeyFor(ipPath, client)
		if !ok {
			return false
		}
		entry, found := self.destinationServiceFailures[key]
		if !found {
			return false
		}
		if destinationServiceFailureTtl <= now.Sub(entry.time) {
			delete(self.destinationServiceFailures, key)
			return false
		}
	}
	return true
}

// A later window may replace a globally healthy but service-failed first field
// only with its ordinary healthy offer. Keep the first field's last-resort
// availability when later windows have only benched or ended owners.
func healthyCrossWindowCandidates(candidates []*multiClientChannel) []*multiClientChannel {
	count := 0
	for _, client := range candidates {
		if _, err := client.WindowStats(); err == nil && !client.isWarning() {
			candidates[count] = client
			count++
		}
	}
	return candidates[:count]
}
