package connect

import (
	"io"
	"net"
	"strings"
	"testing"
)

func syntheticRequest(t *testing.T, request string) (header string, bodyByteCount int) {
	c := newSyntheticSpeedConn()
	defer c.Close()
	if _, err := c.Write([]byte(request)); err != nil {
		t.Fatalf("write: %s", err)
	}
	var response []byte
	buf := make([]byte, 32*1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			response = append(response, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %s", err)
		}
	}
	i := strings.Index(string(response), "\r\n\r\n")
	if i < 0 {
		t.Fatalf("no header terminator in response")
	}
	return string(response[:i]), len(response) - (i + 4)
}

func TestSyntheticSpeedPing(t *testing.T) {
	header, bodyByteCount := syntheticRequest(t, "GET /ping HTTP/1.1\r\nHost: 198.18.0.1\r\n\r\n")
	if !strings.HasPrefix(header, "HTTP/1.1 200 OK") {
		t.Fatalf("unexpected status: %s", header)
	}
	if bodyByteCount != 1 {
		t.Fatalf("expected 1-byte ping body, got %d", bodyByteCount)
	}
}

func TestSyntheticSpeedDownload(t *testing.T) {
	header, bodyByteCount := syntheticRequest(t, "GET /download/1000000 HTTP/1.1\r\nHost: 198.18.0.1\r\n\r\n")
	if !strings.HasPrefix(header, "HTTP/1.1 200 OK") {
		t.Fatalf("unexpected status: %s", header)
	}
	if !strings.Contains(header, "Content-Length: 1000000") {
		t.Fatalf("missing content length: %s", header)
	}
	if bodyByteCount != 1000000 {
		t.Fatalf("expected 1000000 body bytes, got %d", bodyByteCount)
	}
}

func TestSyntheticSpeedUpload(t *testing.T) {
	body := strings.Repeat("x", 5000)
	header, _ := syntheticRequest(t, "POST /upload HTTP/1.1\r\nHost: 198.18.0.1\r\nContent-Length: 5000\r\n\r\n"+body)
	if !strings.HasPrefix(header, "HTTP/1.1 200 OK") {
		t.Fatalf("unexpected status: %s", header)
	}
}

func TestSyntheticSpeedIpRange(t *testing.T) {
	for ip, expect := range map[string]bool{
		"198.18.0.1":   true,
		"198.19.255.1": true,
		"198.17.0.1":   false,
		"198.20.0.1":   false,
		"1.1.1.1":      false,
	} {
		if got := isSyntheticSpeedIp(net.ParseIP(ip)); got != expect {
			t.Fatalf("%s: expected %t got %t", ip, expect, got)
		}
	}
}
