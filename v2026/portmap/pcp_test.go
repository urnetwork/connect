package portmap

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

// The pcp and nat-pmp wire formats, byte for byte against the RFCs.

func TestEncodePcpMapRequest(t *testing.T) {
	nonce := [12]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	request := encodePcpMapRequest(3600, testLocalIp, nonce, pcpProtocolTcp, 443, 443, netip.Addr{})
	if len(request) != 60 {
		t.Fatalf("request is %d bytes, expected 60 (RFC 6887 §7.1, §11.1)", len(request))
	}
	if request[0] != 2 || request[1] != 1 || request[2] != 0 || request[3] != 0 {
		t.Fatalf("header = %x, expected version 2 and the MAP opcode", request[:4])
	}
	if lifetime := binary.BigEndian.Uint32(request[4:8]); lifetime != 3600 {
		t.Fatalf("lifetime = %d", lifetime)
	}
	// the client address in its ipv4-mapped form
	if clientIp := netip.AddrFrom16([16]byte(request[8:24])); clientIp != netip.AddrFrom16(testLocalIp.As16()) {
		t.Fatalf("client address = %s", clientIp)
	}
	body := request[24:]
	if [12]byte(body[0:12]) != nonce || body[12] != 6 || body[13] != 0 || body[14] != 0 || body[15] != 0 {
		t.Fatalf("map body = %x", body[:16])
	}
	if binary.BigEndian.Uint16(body[16:18]) != 443 || binary.BigEndian.Uint16(body[18:20]) != 443 {
		t.Fatalf("ports = %x", body[16:20])
	}
	// no preference is the ipv4 all-zeros address, ::ffff:0.0.0.0 (RFC 6887 §11.1)
	expectedNoPreference := [16]byte{10: 0xff, 11: 0xff}
	if [16]byte(body[20:36]) != expectedNoPreference {
		t.Fatalf("suggested external address = %x, expected ::ffff:0.0.0.0", body[20:36])
	}
}

func TestDecodePcpMapResponse(t *testing.T) {
	response := make([]byte, 60)
	response[0] = 2
	response[1] = 0x81
	response[3] = 0
	binary.BigEndian.PutUint32(response[4:8], 1800)
	body := response[24:]
	copy(body[0:12], []byte("abcdefghijkl"))
	body[12] = 17
	binary.BigEndian.PutUint16(body[16:18], 4053)
	binary.BigEndian.PutUint16(body[18:20], 40053)
	externalAddress := pcpAddressBytes(testExternalIp)
	copy(body[20:36], externalAddress[:])

	answer, ok := decodePcpMapResponse(response)
	if !ok {
		t.Fatal("a MAP response was not read")
	}
	if answer.resultCode != 0 || answer.lifetimeSeconds != 1800 || answer.protocol != 17 ||
		answer.internalPort != 4053 || answer.externalPort != 40053 || answer.externalIp != testExternalIp ||
		string(answer.nonce[:]) != "abcdefghijkl" {
		t.Fatalf("answer = %+v", answer)
	}
	// a request, a short message, and another version are not responses
	request := encodePcpMapRequest(1, testLocalIp, [12]byte{}, 6, 1, 1, netip.Addr{})
	for _, notResponse := range [][]byte{request, response[:59], append([]byte{1}, response[1:]...)} {
		if _, ok := decodePcpMapResponse(notResponse); ok {
			t.Fatalf("%x was read as a MAP response", notResponse[:4])
		}
	}
}

func TestNatPmpUnsupportedVersionAnswersAPcpRequest(t *testing.T) {
	// a nat-pmp server's answer to a pcp MAP request (RFC 6886 §3.5, RFC 6887 §9)
	answer := []byte{0, 0x81, 0, 1, 0, 0, 0, 9}
	if !isNatPmpUnsupportedVersion(answer, pcpOpcodeMap) {
		t.Fatal("the unsupported version answer was not recognized")
	}
	for _, other := range [][]byte{
		{0, 0x81, 0, 0, 0, 0, 0, 9},
		{0, 0x82, 0, 1, 0, 0, 0, 9},
		{2, 0x81, 0, 1, 0, 0, 0, 9},
		{0, 0x81, 0, 1},
	} {
		if isNatPmpUnsupportedVersion(other, pcpOpcodeMap) {
			t.Fatalf("%x was taken for an unsupported version answer", other)
		}
	}
}

func TestEncodeNatPmpMapRequest(t *testing.T) {
	request := encodeNatPmpMapRequest(natPmpOpcodeMapTcp, 443, 443, 3600)
	expected := []byte{0, 2, 0, 0, 0x01, 0xbb, 0x01, 0xbb, 0, 0, 0x0e, 0x10}
	if string(request) != string(expected) {
		t.Fatalf("request = %x, expected %x (RFC 6886 §3.3)", request, expected)
	}
	// a removal: the suggested external port and the lifetime are zero (§3.4)
	removal := encodeNatPmpMapRequest(natPmpOpcodeMapUdp, 4053, 0, 0)
	expectedRemoval := []byte{0, 1, 0, 0, 0x0f, 0xd5, 0, 0, 0, 0, 0, 0}
	if string(removal) != string(expectedRemoval) {
		t.Fatalf("removal = %x, expected %x", removal, expectedRemoval)
	}
}

func TestDecodeNatPmpMapResponse(t *testing.T) {
	response := []byte{0, 0x82, 0, 0, 0, 0, 0, 7, 0x01, 0xbb, 0x9d, 0xfb, 0, 0, 0x0e, 0x10}
	answer, ok := decodeNatPmpMapResponse(response, natPmpOpcodeMapTcp)
	if !ok || !answer.complete || answer.resultCode != 0 || answer.internalPort != 443 ||
		answer.externalPort != 40443 || answer.lifetimeSeconds != 3600 {
		t.Fatalf("answer = %+v %t", answer, ok)
	}
	// an error answer may stop after its header
	refusal, ok := decodeNatPmpMapResponse([]byte{0, 0x82, 0, 2, 0, 0, 0, 7}, natPmpOpcodeMapTcp)
	if !ok || refusal.complete || refusal.resultCode != 2 {
		t.Fatalf("refusal = %+v %t", refusal, ok)
	}
	if _, ok := decodeNatPmpMapResponse(response, natPmpOpcodeMapUdp); ok {
		t.Fatal("a tcp answer was read as the answer to a udp request")
	}
}
