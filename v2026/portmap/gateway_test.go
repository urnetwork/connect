package portmap

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// The production udp exchange and ssdp search, on loopback: the fake gateway is a
// socket in this process.

func testLoopbackUdp(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func testAddrPort(conn *net.UDPConn) netip.AddrPort {
	return conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

type testExchangeResult struct {
	response []byte
	err      error
}

func TestUdpExchangeTakesTheGatewaysAnswerAndDropsAnotherSource(t *testing.T) {
	server := testLoopbackUdp(t)
	stranger := testLoopbackUdp(t)
	request := []byte{natPmpVersion, natPmpOpcodeExternalAddress}

	results := make(chan testExchangeResult, 1)
	go func() {
		response, err := UdpExchange(
			context.Background(),
			testAddrPort(server),
			netip.MustParseAddr("127.0.0.1"),
			request,
			func(response []byte) bool {
				return 0 < len(response) && response[0] == 0xaa
			},
			10*time.Second,
			1,
		)
		results <- testExchangeResult{response: response, err: err}
	}()

	buffer := make([]byte, 64)
	n, client, err := server.ReadFromUDPAddrPort(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buffer[:n], request) {
		t.Fatalf("the gateway read %x, expected %x", buffer[:n], request)
	}
	// an answer from another source is dropped, even one accept would take
	if _, err := stranger.WriteToUDPAddrPort([]byte{0xaa, 0x01}, client); err != nil {
		t.Fatal(err)
	}
	// and an answer accept refuses is dropped too
	if _, err := server.WriteToUDPAddrPort([]byte{0xbb}, client); err != nil {
		t.Fatal(err)
	}
	if _, err := server.WriteToUDPAddrPort([]byte{0xaa, 0x02}, client); err != nil {
		t.Fatal(err)
	}
	result := <-results
	if result.err != nil {
		t.Fatal(result.err)
	}
	if !bytes.Equal(result.response, []byte{0xaa, 0x02}) {
		t.Fatalf("response = %x, expected the gateway's", result.response)
	}
}

func TestUdpExchangeSendsAgainWhenUnanswered(t *testing.T) {
	server := testLoopbackUdp(t)
	request := []byte{natPmpVersion, natPmpOpcodeExternalAddress}

	results := make(chan testExchangeResult, 1)
	go func() {
		response, err := UdpExchange(
			context.Background(),
			testAddrPort(server),
			netip.MustParseAddr("127.0.0.1"),
			request,
			func(response []byte) bool { return true },
			50*time.Millisecond,
			3,
		)
		results <- testExchangeResult{response: response, err: err}
	}()

	buffer := make([]byte, 64)
	// the first send goes unanswered
	if _, _, err := server.ReadFromUDPAddrPort(buffer); err != nil {
		t.Fatal(err)
	}
	_, client, err := server.ReadFromUDPAddrPort(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.WriteToUDPAddrPort([]byte{0x01}, client); err != nil {
		t.Fatal(err)
	}
	if result := <-results; result.err != nil || !bytes.Equal(result.response, []byte{0x01}) {
		t.Fatalf("result = %x %v, expected the answer to the second send", result.response, result.err)
	}
}

func TestUdpExchangeEndsWithItsContext(t *testing.T) {
	server := testLoopbackUdp(t)
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan testExchangeResult, 1)
	go func() {
		response, err := UdpExchange(
			ctx,
			testAddrPort(server),
			netip.MustParseAddr("127.0.0.1"),
			[]byte{0},
			func(response []byte) bool { return true },
			time.Hour,
			1,
		)
		results <- testExchangeResult{response: response, err: err}
	}()
	buffer := make([]byte, 64)
	if _, _, err := server.ReadFromUDPAddrPort(buffer); err != nil {
		t.Fatal(err)
	}
	// the request is in flight and nothing will answer it
	cancel()
	if result := <-results; result.err != context.Canceled {
		t.Fatalf("err = %v, expected the cancel", result.err)
	}
}

func TestUpnpSearchReadsTheGatewaysAnswer(t *testing.T) {
	gateway := testLoopbackUdp(t)
	location := "http://127.0.0.1:5000/rootDesc.xml"
	results := make(chan []string, 1)
	errs := make(chan error, 1)
	go func() {
		locations, err := UpnpSearch(
			context.Background(),
			netip.MustParseAddr("127.0.0.1"),
			netip.MustParseAddr("127.0.0.1"),
			10*time.Second,
			int(testAddrPort(gateway).Port()),
			"",
		)
		results <- locations
		errs <- err
	}()

	buffer := make([]byte, 2048)
	n, client, err := gateway.ReadFromUDPAddrPort(buffer)
	if err != nil {
		t.Fatal(err)
	}
	search := string(buffer[:n])
	for _, line := range []string{"M-SEARCH * HTTP/1.1\r\n", "ST: " + upnpSearchTarget + "\r\n", "MAN: \"ssdp:discover\"\r\n"} {
		if !bytes.Contains(buffer[:n], []byte(line)) {
			t.Fatalf("search = %q, expected %q", search, line)
		}
	}
	answer := fmt.Sprintf("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\nST: %s\r\nUSN: uuid:1::%s\r\nLOCATION: %s\r\n\r\n",
		upnpSearchTarget, upnpSearchTarget, location)
	if _, err := gateway.WriteToUDPAddrPort([]byte(answer), client); err != nil {
		t.Fatal(err)
	}
	if locations := <-results; !slices.Equal(locations, []string{location}) {
		t.Fatalf("locations = %v, expected %s", locations, location)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}

func TestParseSsdpLocation(t *testing.T) {
	for _, c := range []struct {
		datagram string
		location string
		ok       bool
	}{
		{datagram: "HTTP/1.1 200 OK\r\nLOCATION: http://192.0.2.1:5000/rootDesc.xml\r\n\r\n", location: "http://192.0.2.1:5000/rootDesc.xml", ok: true},
		{datagram: "HTTP/1.1 200 OK\r\nlocation:  http://192.0.2.1/d.xml \r\n\r\n", location: "http://192.0.2.1/d.xml", ok: true},
		{datagram: "HTTP/1.1 200 OK\r\nST: x\r\n\r\n", ok: false},
		{datagram: "HTTP/1.1 404 Not Found\r\nLOCATION: http://192.0.2.1/\r\n\r\n", ok: false},
		{datagram: "NOTIFY * HTTP/1.1\r\nLOCATION: http://192.0.2.1/\r\n\r\n", ok: false},
		{datagram: "garbage", ok: false},
	} {
		location, ok := parseSsdpLocation([]byte(c.datagram))
		if ok != c.ok || location != c.location {
			t.Errorf("%q = %q %t, expected %q %t", c.datagram, location, ok, c.location, c.ok)
		}
	}
}
