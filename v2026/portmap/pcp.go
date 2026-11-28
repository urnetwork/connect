package portmap

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// pcp.go — the two protocols of the gateway's udp 5351: PCP (RFC 6887) and its
// predecessor NAT-PMP (RFC 6886).
//
// The method asks in PCP first. A NAT-PMP server answers a request in a version
// it does not speak with its own version and "unsupported version" (RFC 6886
// §3.5), and that answer moves the method to NAT-PMP for the rest of its life
// (RFC 6887 §9). Only the MAP opcode is used, for the requests the mapper was
// built with, to this host's own address: no PEER, no THIRD_PARTY option, and
// no address but this host's in the client address field.

const (
	pcpVersion    = 2
	natPmpVersion = 0

	// the response bit of a pcp opcode, and a nat-pmp answer's opcode offset
	pcpResponseBit = 0x80

	pcpOpcodeMap = 1

	pcpHeaderByteCount   = 24
	pcpMapByteCount      = 36
	pcpRequestByteCount  = pcpHeaderByteCount + pcpMapByteCount
	pcpResponseByteCount = pcpHeaderByteCount + pcpMapByteCount

	// the iana protocol numbers a pcp map names
	pcpProtocolTcp = 6
	pcpProtocolUdp = 17

	pcpResultSuccess = 0

	natPmpOpcodeExternalAddress = 0
	natPmpOpcodeMapUdp          = 1
	natPmpOpcodeMapTcp          = 2

	natPmpResultSuccess            = 0
	natPmpResultUnsupportedVersion = 1

	natPmpMapRequestByteCount  = 12
	natPmpMapResponseByteCount = 16
	natPmpAddressByteCount     = 12
	// version, opcode, result and epoch: what every nat-pmp answer starts with
	natPmpAnswerHeaderByteCount = 8
)

// The words of a pcp result code (RFC 6887 §7.4).
func pcpResultText(resultCode byte) string {
	switch resultCode {
	case 1:
		return "unsupported version"
	case 2:
		return "not authorized: the router does not allow this mapping"
	case 3:
		return "malformed request"
	case 4:
		return "unsupported opcode"
	case 5:
		return "unsupported option"
	case 6:
		return "malformed option"
	case 7:
		return "network failure: the router has no external address yet"
	case 8:
		return "no resources: the router has no mapping to spare"
	case 9:
		return "unsupported protocol"
	case 10:
		return "this host is over the router's mapping quota"
	case 11:
		return "cannot provide the external port"
	case 12:
		return "address mismatch: another NAT is between this host and the router"
	case 13:
		return "excessive remote peers"
	default:
		return fmt.Sprintf("result code %d", resultCode)
	}
}

// The words of a nat-pmp result code (RFC 6886 §3.5).
func natPmpResultText(resultCode uint16) string {
	switch resultCode {
	case 1:
		return "unsupported version"
	case 2:
		return "not authorized: the router does not allow this mapping"
	case 3:
		return "network failure: the router has no external address yet"
	case 4:
		return "out of resources: the router has no mapping to spare"
	case 5:
		return "unsupported opcode"
	default:
		return fmt.Sprintf("result code %d", resultCode)
	}
}

type pcpMethod struct {
	mapper  *PortMapper
	gateway netip.Addr
	localIp netip.Addr
	// the server answered as a nat-pmp server, or the method was built for
	// nat-pmp alone
	natPmp bool
}

func (self *PortMapper) newPcpMethod(gateway netip.Addr, localIp netip.Addr, natPmp bool) *pcpMethod {
	return &pcpMethod{
		mapper:  self,
		gateway: gateway,
		localIp: localIp,
		natPmp:  natPmp,
	}
}

func (self *pcpMethod) name() string {
	if self.natPmp {
		return MethodNatPmp
	}
	return MethodPcp
}

func (self *pcpMethod) server() netip.AddrPort {
	return netip.AddrPortFrom(self.gateway, uint16(self.mapper.settings.ServerPort))
}

func (self *pcpMethod) mapPorts(
	ctx context.Context,
	requests []PortMapRequest,
	previous []*methodMapping,
	lifetime time.Duration,
) (externalIp netip.Addr, mappings []*methodMapping, errs []error, err error) {
	mappings = make([]*methodMapping, len(requests))
	errs = make([]error, len(requests))
	for i, request := range requests {
		var previousMapping *methodMapping
		if i < len(previous) {
			previousMapping = previous[i]
		}
		mapping, mappedIp, mapErr := self.mapPort(ctx, request, previousMapping, lifetime)
		if mapErr != nil {
			if i == 0 && previousMapping == nil && errors.Is(mapErr, errNoAnswer) {
				// the first request is what says whether the router speaks
				// either protocol at all
				return netip.Addr{}, mappings, errs, errMethodNotSupported
			}
			if ctx.Err() != nil {
				return netip.Addr{}, mappings, errs, ctx.Err()
			}
			errs[i] = mapErr
			continue
		}
		mappings[i] = mapping
		if mappedIp.IsValid() {
			externalIp = mappedIp
		}
	}
	if self.natPmp && !externalIp.IsValid() && mappings[0] != nil {
		// a nat-pmp map answer names no address, which a request of its own
		// does (RFC 6886 §3.2). Only for the status, so a failure is no error
		if addressIp, addressErr := self.natPmpExternalAddress(ctx); addressErr == nil {
			externalIp = addressIp
		}
	}
	return externalIp, mappings, errs, nil
}

func (self *pcpMethod) mapPort(
	ctx context.Context,
	request PortMapRequest,
	previous *methodMapping,
	lifetime time.Duration,
) (*methodMapping, netip.Addr, error) {
	if self.natPmp {
		return self.natPmpMapPort(ctx, request, previous, lifetime)
	}
	protocolNumber, err := pcpProtocolNumber(request.Protocol)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	var nonce [12]byte
	suggestedExternalPort := request.InternalPort
	suggestedExternalIp := netip.Addr{}
	if previous != nil {
		// a renewal names the mapping by its nonce (RFC 6887 §11.2)
		nonce = previous.pcpNonce
		suggestedExternalPort = previous.ExternalPort
	} else if _, err := rand.Read(nonce[:]); err != nil {
		return nil, netip.Addr{}, err
	}
	requestBytes := encodePcpMapRequest(
		self.mapper.lifetimeSeconds(lifetime),
		self.localIp,
		nonce,
		protocolNumber,
		request.InternalPort,
		suggestedExternalPort,
		suggestedExternalIp,
	)
	response, err := self.mapper.exchange()(ctx, self.server(), self.localIp, requestBytes, func(response []byte) bool {
		if isNatPmpUnsupportedVersion(response, pcpOpcodeMap) {
			return true
		}
		answer, ok := decodePcpMapResponse(response)
		return ok && answer.nonce == nonce && answer.protocol == protocolNumber && answer.internalPort == request.InternalPort
	})
	if err != nil {
		return nil, netip.Addr{}, err
	}
	if isNatPmpUnsupportedVersion(response, pcpOpcodeMap) {
		// a nat-pmp server: the rest of this method's life speaks nat-pmp
		self.natPmp = true
		return self.natPmpMapPort(ctx, request, previous, lifetime)
	}
	answer, _ := decodePcpMapResponse(response)
	if answer.resultCode != pcpResultSuccess {
		return nil, netip.Addr{}, errors.New(pcpResultText(answer.resultCode))
	}
	if answer.lifetimeSeconds == 0 || answer.externalPort == 0 {
		return nil, netip.Addr{}, errors.New("the router granted no mapping")
	}
	granted := time.Duration(answer.lifetimeSeconds) * time.Second
	return &methodMapping{
		PortMapping: PortMapping{
			Protocol:     request.Protocol,
			InternalPort: request.InternalPort,
			ExternalPort: answer.externalPort,
			Lifetime:     granted,
			ExpireTime:   self.mapper.now().Add(granted),
		},
		pcpNonce: nonce,
	}, answer.externalIp, nil
}

func (self *pcpMethod) natPmpMapPort(
	ctx context.Context,
	request PortMapRequest,
	previous *methodMapping,
	lifetime time.Duration,
) (*methodMapping, netip.Addr, error) {
	opcode, err := natPmpMapOpcode(request.Protocol)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	suggestedExternalPort := request.InternalPort
	if previous != nil {
		suggestedExternalPort = previous.ExternalPort
	}
	requestBytes := encodeNatPmpMapRequest(opcode, request.InternalPort, suggestedExternalPort, self.mapper.lifetimeSeconds(lifetime))
	response, err := self.mapper.exchange()(ctx, self.server(), self.localIp, requestBytes, func(response []byte) bool {
		answer, ok := decodeNatPmpMapResponse(response, opcode)
		return ok && (answer.resultCode != natPmpResultSuccess || answer.internalPort == request.InternalPort)
	})
	if err != nil {
		return nil, netip.Addr{}, err
	}
	answer, _ := decodeNatPmpMapResponse(response, opcode)
	if answer.resultCode != natPmpResultSuccess {
		return nil, netip.Addr{}, errors.New(natPmpResultText(answer.resultCode))
	}
	if !answer.complete || answer.lifetimeSeconds == 0 || answer.externalPort == 0 {
		return nil, netip.Addr{}, errors.New("the router granted no mapping")
	}
	granted := time.Duration(answer.lifetimeSeconds) * time.Second
	return &methodMapping{
		PortMapping: PortMapping{
			Protocol:     request.Protocol,
			InternalPort: request.InternalPort,
			ExternalPort: answer.externalPort,
			Lifetime:     granted,
			ExpireTime:   self.mapper.now().Add(granted),
		},
	}, netip.Addr{}, nil
}

func (self *pcpMethod) natPmpExternalAddress(ctx context.Context) (netip.Addr, error) {
	requestBytes := []byte{natPmpVersion, natPmpOpcodeExternalAddress}
	response, err := self.mapper.exchange()(ctx, self.server(), self.localIp, requestBytes, func(response []byte) bool {
		return natPmpAnswerHeaderByteCount <= len(response) &&
			response[0] == natPmpVersion &&
			response[1] == pcpResponseBit|natPmpOpcodeExternalAddress
	})
	if err != nil {
		return netip.Addr{}, err
	}
	if resultCode := binary.BigEndian.Uint16(response[2:4]); resultCode != natPmpResultSuccess {
		return netip.Addr{}, errors.New(natPmpResultText(resultCode))
	}
	if len(response) < natPmpAddressByteCount {
		return netip.Addr{}, errors.New("the router's address answer is short")
	}
	return netip.AddrFrom4([4]byte(response[8:12])), nil
}

// Removes each mapping: a request for it with lifetime zero (RFC 6887 §15, RFC
// 6886 §3.4).
func (self *pcpMethod) removePorts(ctx context.Context, mappings []*methodMapping) error {
	errs := []error{}
	for _, mapping := range mappings {
		var err error
		if self.natPmp {
			err = self.natPmpRemovePort(ctx, mapping)
		} else {
			err = self.pcpRemovePort(ctx, mapping)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %d: %w", mapping.Protocol, mapping.InternalPort, err))
		}
	}
	return errors.Join(errs...)
}

func (self *pcpMethod) pcpRemovePort(ctx context.Context, mapping *methodMapping) error {
	protocolNumber, err := pcpProtocolNumber(mapping.Protocol)
	if err != nil {
		return err
	}
	requestBytes := encodePcpMapRequest(
		0,
		self.localIp,
		mapping.pcpNonce,
		protocolNumber,
		mapping.InternalPort,
		0,
		netip.Addr{},
	)
	response, err := self.mapper.exchange()(ctx, self.server(), self.localIp, requestBytes, func(response []byte) bool {
		answer, ok := decodePcpMapResponse(response)
		return ok && answer.nonce == mapping.pcpNonce && answer.protocol == protocolNumber && answer.internalPort == mapping.InternalPort
	})
	if err != nil {
		return err
	}
	if answer, _ := decodePcpMapResponse(response); answer.resultCode != pcpResultSuccess {
		return errors.New(pcpResultText(answer.resultCode))
	}
	return nil
}

func (self *pcpMethod) natPmpRemovePort(ctx context.Context, mapping *methodMapping) error {
	opcode, err := natPmpMapOpcode(mapping.Protocol)
	if err != nil {
		return err
	}
	// the suggested external port and the lifetime are both zero (RFC 6886 §3.4)
	requestBytes := encodeNatPmpMapRequest(opcode, mapping.InternalPort, 0, 0)
	response, err := self.mapper.exchange()(ctx, self.server(), self.localIp, requestBytes, func(response []byte) bool {
		answer, ok := decodeNatPmpMapResponse(response, opcode)
		return ok && (answer.resultCode != natPmpResultSuccess || answer.internalPort == mapping.InternalPort)
	})
	if err != nil {
		return err
	}
	if answer, _ := decodeNatPmpMapResponse(response, opcode); answer.resultCode != natPmpResultSuccess {
		return errors.New(natPmpResultText(answer.resultCode))
	}
	return nil
}

func pcpProtocolNumber(protocol string) (byte, error) {
	switch protocol {
	case ProtocolTcp:
		return pcpProtocolTcp, nil
	case ProtocolUdp:
		return pcpProtocolUdp, nil
	default:
		return 0, fmt.Errorf("unknown protocol %q", protocol)
	}
}

func natPmpMapOpcode(protocol string) (byte, error) {
	switch protocol {
	case ProtocolTcp:
		return natPmpOpcodeMapTcp, nil
	case ProtocolUdp:
		return natPmpOpcodeMapUdp, nil
	default:
		return 0, fmt.Errorf("unknown protocol %q", protocol)
	}
}

// The 16 byte address field of pcp: an ipv4 address in its ipv4-mapped form, and
// the ipv4 all-zeros address, ::ffff:0.0.0.0, for no address (RFC 6887 §5, §11.1).
func pcpAddressBytes(ip netip.Addr) [16]byte {
	if !ip.IsValid() {
		ip = netip.IPv4Unspecified()
	}
	if ip.Is4() {
		ip = netip.AddrFrom16(ip.As16())
	}
	return ip.As16()
}

// A MAP request (RFC 6887 §7.1, §11.1).
func encodePcpMapRequest(
	lifetimeSeconds uint32,
	clientIp netip.Addr,
	nonce [12]byte,
	protocolNumber byte,
	internalPort int,
	suggestedExternalPort int,
	suggestedExternalIp netip.Addr,
) []byte {
	request := make([]byte, pcpRequestByteCount)
	request[0] = pcpVersion
	request[1] = pcpOpcodeMap
	binary.BigEndian.PutUint32(request[4:8], lifetimeSeconds)
	clientAddress := pcpAddressBytes(clientIp)
	copy(request[8:24], clientAddress[:])
	body := request[pcpHeaderByteCount:]
	copy(body[0:12], nonce[:])
	body[12] = protocolNumber
	binary.BigEndian.PutUint16(body[16:18], uint16(internalPort))
	binary.BigEndian.PutUint16(body[18:20], uint16(suggestedExternalPort))
	externalAddress := pcpAddressBytes(suggestedExternalIp)
	copy(body[20:36], externalAddress[:])
	return request
}

type pcpMapResponse struct {
	resultCode      byte
	lifetimeSeconds uint32
	nonce           [12]byte
	protocol        byte
	internalPort    int
	externalPort    int
	externalIp      netip.Addr
}

// A MAP response (RFC 6887 §7.2, §11.1), or false when `response` is not one.
func decodePcpMapResponse(response []byte) (pcpMapResponse, bool) {
	if len(response) < pcpResponseByteCount ||
		response[0] != pcpVersion ||
		response[1] != pcpResponseBit|pcpOpcodeMap {
		return pcpMapResponse{}, false
	}
	body := response[pcpHeaderByteCount:]
	answer := pcpMapResponse{
		resultCode:      response[3],
		lifetimeSeconds: binary.BigEndian.Uint32(response[4:8]),
		protocol:        body[12],
		internalPort:    int(binary.BigEndian.Uint16(body[16:18])),
		externalPort:    int(binary.BigEndian.Uint16(body[18:20])),
		externalIp:      netip.AddrFrom16([16]byte(body[20:36])).Unmap(),
	}
	copy(answer.nonce[:], body[0:12])
	return answer, true
}

// Whether `response` is a nat-pmp server's answer to a request in a version it
// does not speak, whose opcode was `opcode` (RFC 6886 §3.5).
func isNatPmpUnsupportedVersion(response []byte, opcode byte) bool {
	return natPmpAnswerHeaderByteCount <= len(response) &&
		response[0] == natPmpVersion &&
		response[1] == pcpResponseBit|opcode &&
		binary.BigEndian.Uint16(response[2:4]) == natPmpResultUnsupportedVersion
}

// A nat-pmp mapping request (RFC 6886 §3.3).
func encodeNatPmpMapRequest(opcode byte, internalPort int, suggestedExternalPort int, lifetimeSeconds uint32) []byte {
	request := make([]byte, natPmpMapRequestByteCount)
	request[0] = natPmpVersion
	request[1] = opcode
	binary.BigEndian.PutUint16(request[4:6], uint16(internalPort))
	binary.BigEndian.PutUint16(request[6:8], uint16(suggestedExternalPort))
	binary.BigEndian.PutUint32(request[8:12], lifetimeSeconds)
	return request
}

type natPmpMapResponse struct {
	resultCode uint16
	// the answer carries the mapping fields, which an error answer may omit
	complete        bool
	internalPort    int
	externalPort    int
	lifetimeSeconds uint32
}

// A nat-pmp mapping answer to `opcode` (RFC 6886 §3.3), or false when `response`
// is not one.
func decodeNatPmpMapResponse(response []byte, opcode byte) (natPmpMapResponse, bool) {
	if len(response) < natPmpAnswerHeaderByteCount ||
		response[0] != natPmpVersion ||
		response[1] != pcpResponseBit|opcode {
		return natPmpMapResponse{}, false
	}
	answer := natPmpMapResponse{
		resultCode: binary.BigEndian.Uint16(response[2:4]),
	}
	if natPmpMapResponseByteCount <= len(response) {
		answer.complete = true
		answer.internalPort = int(binary.BigEndian.Uint16(response[8:10]))
		answer.externalPort = int(binary.BigEndian.Uint16(response[10:12]))
		answer.lifetimeSeconds = binary.BigEndian.Uint32(response[12:16])
	}
	return answer, true
}
