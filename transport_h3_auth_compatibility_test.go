// Exercises historical auth reconstruction through the current h3 handshake,
// including strict modern echoes, raw fallback, refusal, and joined ownership.
package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Reconstructs Auth from 847460bba8aa31fb2e86ce007e9d5443d884544b, whose
// fields ended at provide_intent (7), before metadata (8) and lease version (9).
func legacyH3AuthDescriptor(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	legacy := protodesc.ToDescriptorProto((&protocol.Auth{}).ProtoReflect().Descriptor())
	legacyFields := make([]*descriptorpb.FieldDescriptorProto, 0, 7)
	for _, field := range legacy.Field {
		if field.GetNumber() <= 7 {
			legacyFields = append(legacyFields, field)
		}
	}
	if len(legacyFields) != 7 {
		t.Fatalf("legacy auth field count = %d, want 7", len(legacyFields))
	}
	legacy.Field = legacyFields
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:        proto.String("h3_legacy_auth.proto"),
		Package:     proto.String("h3_legacy"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{legacy},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return file.Messages().Get(0)
}

// Borrows the current framed request and decodes it with the old descriptor.
// The current Go container below exposes only those old known fields to the
// historical helper; appended fields remain unknown exactly as on the old peer.
func decodeLegacyH3Auth(descriptor protoreflect.MessageDescriptor, frameBytes []byte) (*protocol.Auth, error) {
	frame := &protocol.Frame{}
	if err := proto.Unmarshal(frameBytes, frame); err != nil {
		return nil, err
	}
	if frame.MessageType != protocol.MessageType_TransferAuth {
		return nil, fmt.Errorf("legacy auth frame type = %s", frame.MessageType)
	}
	legacy := dynamicpb.NewMessage(descriptor)
	if err := proto.Unmarshal(frame.MessageBytes, legacy); err != nil {
		return nil, err
	}
	if len(legacy.GetUnknown()) == 0 {
		return nil, fmt.Errorf("current auth did not carry unknown session fields")
	}
	fields := descriptor.Fields()
	request := &protocol.Auth{
		ByJwt:                     legacy.Get(fields.ByName("by_jwt")).String(),
		AppVersion:                legacy.Get(fields.ByName("app_version")).String(),
		InstanceId:                bytes.Clone(legacy.Get(fields.ByName("instance_id")).Bytes()),
		H3DatagramVersion:         uint32(legacy.Get(fields.ByName("h3_datagram_version")).Uint()),
		H3DatagramAcceptedVersion: uint32(legacy.Get(fields.ByName("h3_datagram_accepted_version")).Uint()),
		IpFamily:                  int32(legacy.Get(fields.ByName("ip_family")).Int()),
		ProvideIntent:             legacy.Get(fields.ByName("provide_intent")).Bool(),
	}
	request.ProtoReflect().SetUnknown(bytes.Clone(legacy.GetUnknown()))
	return request, nil
}

// The actual response reconstruction from Connect
// 847460bba8aa31fb2e86ce007e9d5443d884544b:transport_h3_datagram.go:151.
// Only the helper name and the historical constant's spelling differ; pinning
// version 2 prevents a future production version change from rewriting history.
func acceptLegacyH3DatagramAuthOffer(
	auth *protocol.Auth,
	enabled bool,
	localQuicSupport bool,
	remoteQuicSupport bool,
) (*protocol.Auth, bool) {
	const legacyH3DatagramProtocolVersion uint32 = 2
	accepted := enabled && localQuicSupport && remoteQuicSupport &&
		auth.H3DatagramVersion == legacyH3DatagramProtocolVersion
	response := &protocol.Auth{
		ByJwt:             auth.ByJwt,
		AppVersion:        auth.AppVersion,
		InstanceId:        bytes.Clone(auth.InstanceId),
		H3DatagramVersion: auth.H3DatagramVersion,
	}
	if accepted {
		response.H3DatagramAcceptedVersion = legacyH3DatagramProtocolVersion
	}
	return response, accepted
}

// A wire-decoded old response negotiates the legacy carrier without mutating
// either auth message or changing the exported strict validator's contract.
func TestH3AuthCompatibilityAcceptsOnlyCompleteLegacyOmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	request := &protocol.Auth{
		ByJwt: "synthetic-h3-credential", AppVersion: "synthetic-version",
		InstanceId: NewId().Bytes(), ClientInfo: `{"v":1,"device_type":"synthetic"}`,
		StreamLeaseVersion: 1,
	}
	SetH3DatagramAuthOffer(request, true)
	requestBytes, err := EncodeFrame(request, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(requestBytes)
	legacyRequest, err := decodeLegacyH3Auth(legacyH3AuthDescriptor(t), requestBytes)
	if err != nil {
		t.Fatal(err)
	}
	legacyResponse, accepted := acceptLegacyH3DatagramAuthOffer(legacyRequest, true, true, true)
	if !accepted || legacyResponse.ClientInfo != "" || legacyResponse.StreamLeaseVersion != 0 ||
		len(legacyResponse.ProtoReflect().GetUnknown()) != 0 {
		t.Fatal("historical helper did not reconstruct its accepted response")
	}
	responseBytes, err := EncodeFrame(legacyResponse, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(responseBytes)
	decoded, err := DecodeFrame(responseBytes)
	if err != nil {
		t.Fatal(err)
	}
	response := decoded.(*protocol.Auth)
	requestBefore, responseBefore := proto.Clone(request), proto.Clone(response)
	if accepted, err := ValidateH3DatagramAuthResponse(request, response, true, true, true); accepted || err == nil {
		t.Fatalf("strict validator accepted missing echoes: (%t, %v)", accepted, err)
	}
	if accepted, err := negotiateH3DatagramAuthResponse(request, response, true, true, true); !accepted || err != nil {
		t.Fatalf("historical accepted response failed negotiation: (%t, %v)", accepted, err)
	}
	if !proto.Equal(request, requestBefore) || !proto.Equal(response, responseBefore) {
		t.Fatal("negotiation mutated borrowed auth messages")
	}
	modernResponse, _ := AcceptH3DatagramAuthOffer(request, true, true, true)
	if accepted, err := negotiateH3DatagramAuthResponse(request, modernResponse, true, true, true); !accepted || err != nil {
		t.Fatalf("modern response failed negotiation: (%t, %v)", accepted, err)
	}
	if accepted, err := negotiateH3DatagramAuthResponse(request, request, true, true, false); accepted || err != nil {
		t.Fatalf("raw echo failed reliable fallback: (%t, %v)", accepted, err)
	}
}

// Both response generations retain identity, offer, version and transport
// capability checks. Partial or contradictory session echoes cannot downgrade.
func TestH3AuthCompatibilityRejectsIdentityAndCapabilityChanges(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, c := range []struct {
			name   string
			change func(*protocol.Auth, *protocol.Auth)
		}{
			{name: "credential", change: func(_, response *protocol.Auth) { response.ByJwt += "-other" }},
			{name: "app version", change: func(_, response *protocol.Auth) { response.AppVersion += "-other" }},
			{name: "instance", change: func(_, response *protocol.Auth) { response.InstanceId[0] ^= 1 }},
			{name: "offer", change: func(_, response *protocol.Auth) { response.H3DatagramVersion++ }},
			{name: "unoffered", change: func(request, _ *protocol.Auth) { request.H3DatagramVersion = 0 }},
			{name: "unknown accepted", change: func(_, response *protocol.Auth) { response.H3DatagramAcceptedVersion++ }},
			{name: "contradictory metadata", change: func(_, response *protocol.Auth) { response.ClientInfo = `{"v":1,"device_type":"other"}` }},
			{name: "contradictory lease", change: func(_, response *protocol.Auth) { response.StreamLeaseVersion = 2 }},
			{name: "metadata only", change: func(request, response *protocol.Auth) {
				response.ClientInfo = request.ClientInfo
				response.StreamLeaseVersion = 0
			}},
			{name: "lease only", change: func(_, response *protocol.Auth) { response.ClientInfo = ""; response.StreamLeaseVersion = 1 }},
			{name: "future lease offer", change: func(request, _ *protocol.Auth) { request.StreamLeaseVersion = 2 }},
		} {
			request := &protocol.Auth{
				ByJwt: "synthetic-h3-credential", AppVersion: "synthetic-version",
				InstanceId: NewId().Bytes(), ClientInfo: `{"v":1}`, StreamLeaseVersion: 1,
			}
			SetH3DatagramAuthOffer(request, true)
			response, _ := AcceptH3DatagramAuthOffer(request, true, true, true)
			if legacy {
				response, _ = acceptLegacyH3DatagramAuthOffer(request, true, true, true)
			}
			c.change(request, response)
			if accepted, err := negotiateH3DatagramAuthResponse(request, response, true, true, true); accepted || err == nil {
				t.Fatalf("legacy=%t %s accepted: (%t, %v)", legacy, c.name, accepted, err)
			}
		}
	}
	request := &protocol.Auth{
		ByJwt: "synthetic-h3-credential", AppVersion: "synthetic-version",
		InstanceId: NewId().Bytes(), ClientInfo: `{"v":1}`, StreamLeaseVersion: 1,
	}
	SetH3DatagramAuthOffer(request, true)
	for _, legacy := range []bool{false, true} {
		response, _ := AcceptH3DatagramAuthOffer(request, true, true, true)
		if legacy {
			response, _ = acceptLegacyH3DatagramAuthOffer(request, true, true, true)
		}
		for _, capabilities := range [][3]bool{{false, true, true}, {true, false, true}, {true, true, false}} {
			if accepted, err := negotiateH3DatagramAuthResponse(request, response, capabilities[0], capabilities[1], capabilities[2]); accepted || err == nil {
				t.Fatalf("legacy=%t capabilities=%v accepted: (%t, %v)", legacy, capabilities, accepted, err)
			}
		}
	}
	legacyResponse, _ := acceptLegacyH3DatagramAuthOffer(request, true, true, true)
	legacyResponse.H3DatagramAcceptedVersion = 0
	if accepted, err := negotiateH3DatagramAuthResponse(request, legacyResponse, true, true, true); accepted || err == nil {
		t.Fatalf("omitted echoes without acceptance selected fallback: (%t, %v)", accepted, err)
	}
	if accepted, err := negotiateH3DatagramAuthResponse(request, nil, true, true, true); accepted || err == nil {
		t.Fatalf("nil response accepted: (%t, %v)", accepted, err)
	}
}

// Describes the server's response at the real stream handshake boundary.
type h3AuthCompatibilityCase struct {
	legacy          bool
	acceptDatagrams bool
	reject          bool
	malformed       bool
	changeResponse  func(*protocol.Auth)
}

// A single real peer forces auth success or refusal before teardown.
// Route callbacks and peer closure are the barriers; timeouts only
// bound a broken test. Every owned connection, worker, route and buffer is joined.
func testPlatformH3AuthCompatibility(t *testing.T, ipVersion int, c h3AuthCompatibilityCase) {
	t.Helper()
	descriptor := legacyH3AuthDescriptor(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	certPem, keyPem, err := selfSign([]string{"h3-auth.example"}, "h3-auth.example", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	const nextProto = "synthetic-h3-auth"
	listener, err := quic.ListenAddr(
		testLoopbackHostPort(ipVersion, 0),
		&tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{nextProto}},
		&quic.Config{EnableDatagrams: true, MaxIdleTimeout: time.Minute},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(64))
	settings.Log = NewNoopLogger()
	settings.ReconnectTimeout = time.Hour
	settings.PingTimeout = time.Minute
	settings.ReadTimeout = time.Minute
	settings.AuthTimeout = 15 * time.Second
	settings.H3Port = listener.Addr().(*net.UDPAddr).Port
	settings.resolveH3AddrsForTest = func(context.Context, string, int) ([]*net.UDPAddr, error) {
		return []*net.UDPAddr{listener.Addr().(*net.UDPAddr)}, nil
	}
	settings.QuicTlsConfig = &tls.Config{
		InsecureSkipVerify: true, // synthetic in-process certificate
		NextProtos:         []string{nextProto},
	}
	routeAdded := make(chan struct{}, 1)
	var addedCount, removedCount atomic.Int32
	settings.SendRouteObserver = func(_ Transport, _ Route, connected bool) {
		if connected {
			addedCount.Add(1)
			select {
			case routeAdded <- struct{}{}:
			default:
			}
		} else {
			removedCount.Add(1)
		}
	}
	peerDone := make(chan struct{})
	peerResult := make(chan error, 1)
	messageRead := make(chan struct{})
	payload := []byte("synthetic-h3-auth-routed-message")
	go func() {
		defer close(peerDone)
		peerResult <- func() error {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return err
			}
			defer conn.CloseWithError(0, "synthetic peer complete")
			stream, err := conn.AcceptStream(ctx)
			if err != nil {
				return err
			}
			deadline, _ := ctx.Deadline()
			stream.SetDeadline(deadline)
			framer := NewFramer(settings.FramerSettings)
			authBytes, err := framer.Read(stream)
			if err != nil {
				return err
			}
			defer MessagePoolReturn(authBytes)
			decoded, err := DecodeFrame(authBytes)
			if err != nil {
				return err
			}
			request, ok := decoded.(*protocol.Auth)
			if !ok || request.ClientInfo == "" || request.StreamLeaseVersion != 1 {
				return fmt.Errorf("current handshake did not send metadata and lease offer")
			}
			state := conn.ConnectionState()
			response, accepted := AcceptH3DatagramAuthOffer(request, c.acceptDatagrams, state.SupportsDatagrams.Local, state.SupportsDatagrams.Remote)
			if c.legacy {
				legacyRequest, err := decodeLegacyH3Auth(descriptor, authBytes)
				if err != nil {
					return err
				}
				response, accepted = acceptLegacyH3DatagramAuthOffer(legacyRequest, c.acceptDatagrams, state.SupportsDatagrams.Local, state.SupportsDatagrams.Remote)
			}
			if accepted != c.acceptDatagrams {
				return fmt.Errorf("peer DATAGRAM acceptance = %t, want %t", accepted, c.acceptDatagrams)
			}
			if c.changeResponse != nil {
				c.changeResponse(response)
			}
			if c.malformed {
				err = framer.Write(stream, []byte{0xff})
			} else if c.legacy && !accepted {
				// Server 8ebedeace218a6602edc39b46d4642cba797739d used the
				// original raw frame in its non-accepted branch, preserving unknowns.
				err = framer.Write(stream, authBytes)
			} else {
				responseBytes, encodeErr := EncodeFrame(response, DefaultProtocolVersion)
				if encodeErr != nil {
					return encodeErr
				}
				err = framer.Write(stream, responseBytes)
				MessagePoolReturn(responseBytes)
			}
			if err != nil {
				return err
			}
			if c.reject {
				select {
				case <-conn.Context().Done():
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			var message []byte
			if accepted {
				budget := NewH3DatagramReassemblyBudget(settings.H3DatagramSettings.ProcessReassemblyByteCount)
				reassembler, err := NewH3DatagramReassembler(settings.H3DatagramSettings, budget, &H3DatagramStats{})
				if err != nil {
					return err
				}
				defer reassembler.Close()
				for message == nil {
					datagram, err := conn.ReceiveDatagram(ctx)
					if err != nil {
						return fmt.Errorf("accepted peer lost connection before routed DATAGRAM: %w", err)
					}
					message = reassembler.Accept(datagram, time.Now())
				}
			} else {
				message, err = framer.Read(stream)
				if err != nil {
					return fmt.Errorf("raw fallback lost connection before routed stream: %w", err)
				}
			}
			matches := bytes.Equal(message, payload)
			MessagePoolReturn(message)
			if !matches {
				return fmt.Errorf("routed payload changed")
			}
			close(messageRead)
			select {
			case <-conn.Context().Done():
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}()
	}()
	strategySettings := DefaultClientStrategySettings()
	strategySettings.Log = NewNoopLogger()
	strategySettings.EnableResilient = false
	strategySettings.ExtenderConfigs = nil
	strategySettings.ExpandExtenderProfileCount = 0
	strategySettings.InternalDohDomains = nil
	strategy := NewClientStrategy(ctx, strategySettings)
	routes := NewRouteManagerWithLogger(ctx, "synthetic-h3-auth", NewNoopLogger())
	transport := NewPlatformTransportWithTargetMode(
		ctx, strategy, routes, "https://h3-auth.example",
		&ClientAuth{ByJwt: "synthetic-h3-credential", InstanceId: NewId(), AppVersion: "synthetic-version"},
		TransportModeH3, settings,
	)
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := transport.CloseAndWait(closeCtx); err != nil {
			t.Errorf("v%d transport cleanup: %v", ipVersion, err)
		}
		strategy.Close()
		cancel()
		listener.Close()
		select {
		case <-peerDone:
		case <-closeCtx.Done():
			t.Errorf("v%d peer cleanup: %v", ipVersion, closeCtx.Err())
		}
		if transport.IsConnected() || addedCount.Load() != removedCount.Load() {
			t.Errorf("v%d routes survived cleanup: added=%d removed=%d", ipVersion, addedCount.Load(), removedCount.Load())
		}
		if routes.HasActiveTransport() {
			t.Errorf("v%d route manager retained a send or receive transport after cleanup", ipVersion)
		}
		if used := transport.h3DatagramReassemblyBudget.Used(); used != 0 {
			t.Errorf("v%d reassembly retained %d bytes", ipVersion, used)
		}
		if stats := settings.PlatformTransportBudget.Stats(); stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 {
			t.Errorf("v%d transport admission survived cleanup: %+v", ipVersion, stats)
		}
	}()
	if !c.reject {
		select {
		case <-routeAdded:
		case err := <-peerResult:
			t.Fatalf("v%d handshake failed before route publication: %v", ipVersion, err)
		case <-ctx.Done():
			t.Fatalf("v%d wait for authenticated route: %v", ipVersion, ctx.Err())
		}
		writer := routes.OpenMultiRouteWriter(DestinationId(NewId()))
		defer routes.CloseMultiRouteWriter(writer)
		policy := writer.(transferFlightPolicyProvider).transferFlightPolicy()
		if policy.limited != c.acceptDatagrams {
			t.Fatalf("v%d carrier unreliable=%t, want %t", ipVersion, policy.limited, c.acceptDatagrams)
		}
		message := MessagePoolCopy(payload)
		sent, disposition, writeErr := writer.(transferCarrierMultiRouteWriter).writeDetailedWithCarrier(ctx, message, 5*time.Second)
		if !sent {
			MessagePoolReturn(message)
		}
		if !sent || writeErr != nil {
			t.Fatalf("v%d route send: sent=%t err=%v", ipVersion, sent, writeErr)
		}
		if disposition.transportType != TransportTypeH3 || disposition.unreliable != c.acceptDatagrams {
			t.Fatalf("v%d selected carrier = %+v, want H3 unreliable=%t", ipVersion, disposition, c.acceptDatagrams)
		}
		select {
		case <-messageRead:
		case err := <-peerResult:
			t.Fatalf("v%d routed receive: %v", ipVersion, err)
		case <-ctx.Done():
			t.Fatalf("v%d wait for routed payload: %v", ipVersion, ctx.Err())
		}
		if err := transport.CloseAndWait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-peerResult:
		if err != nil {
			t.Fatalf("v%d peer completion: %v", ipVersion, err)
		}
	case <-routeAdded:
		t.Fatalf("v%d rejected auth published a route", ipVersion)
	case <-ctx.Done():
		t.Fatalf("v%d wait for peer close: %v", ipVersion, ctx.Err())
	}
	if c.reject && addedCount.Load() != 0 {
		t.Fatalf("v%d rejected auth published %d routes", ipVersion, addedCount.Load())
	}
}

// The actual old helper must establish the current client and carry a datagram.
func TestPlatformTransportH3AuthLegacyAcceptedCompatibility(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, ipVersion := range testIpVersions {
		testPlatformH3AuthCompatibility(t, ipVersion, h3AuthCompatibilityCase{legacy: true, acceptDatagrams: true})
	}
}

// The historical server's rejected offer echoes the original frame intact.
func TestPlatformTransportH3AuthLegacyRawFallbackCompatibility(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, ipVersion := range testIpVersions {
		testPlatformH3AuthCompatibility(t, ipVersion, h3AuthCompatibilityCase{legacy: true})
	}
}

// A modern response still negotiates its complete auth echo and hybrid carrier.
func TestPlatformTransportH3AuthModernCompatibility(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, ipVersion := range testIpVersions {
		testPlatformH3AuthCompatibility(t, ipVersion, h3AuthCompatibilityCase{acceptDatagrams: true})
	}
}

// Invalid versions, identity, partial echoes and malformed frames close the
// real connection before any route is published, including the legacy branch.
func TestPlatformTransportH3AuthRejectsInvalidCompatibility(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, c := range []h3AuthCompatibilityCase{
		{legacy: true, acceptDatagrams: true, reject: true, changeResponse: func(response *protocol.Auth) { response.H3DatagramAcceptedVersion = 3 }},
		{legacy: true, acceptDatagrams: true, reject: true, changeResponse: func(response *protocol.Auth) { response.ByJwt += "-other" }},
		{acceptDatagrams: true, reject: true, changeResponse: func(response *protocol.Auth) { response.ClientInfo = "" }},
		{acceptDatagrams: true, reject: true, malformed: true},
	} {
		for _, ipVersion := range testIpVersions {
			testPlatformH3AuthCompatibility(t, ipVersion, c)
		}
	}
}
