//go:build !js

package connect

import (
	"net/http"
)

// Native builds dial through the strategy's own dialers (see HttpParallel /
// HttpSerial); there is no platform-direct path. The js build replaces both.
func (self *ClientStrategy) httpPlatformDirect(request *http.Request) (*httpResult, bool, error) {
	return nil, false, nil
}

func platformDirectHttpTransport() http.RoundTripper {
	return nil
}
