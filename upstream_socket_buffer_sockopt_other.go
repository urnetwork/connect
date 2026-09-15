//go:build !unix && !windows

package connect

// Browser WASM and other platforms without socket options keep their default
// buffers. Their unknown buffer policy never requests this control hook.
func setSocketBufferByteCounts(fd uintptr, send bool, receive bool, byteCount int) {}
