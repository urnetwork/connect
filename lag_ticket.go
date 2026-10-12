package connect

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// lag.dev tickets and reports (lag.dev/PLAN.md M0). A ticket is the
// operator's admission of one test on one extender (M3, A6), signed by the
// root key that signs records. A report is what the extender measured in that
// test (M8, A7), signed by its identity key.
//
// Both are signed as records are (extender_record.go): ed25519 over a domain
// string concatenated with the already serialized body, which travels as the
// exact bytes that were signed and is decoded only after verification. Each
// has a domain of its own, so neither signature can stand in for a record's,
// a revocation's or the other's.
//
// The functions here are safe for concurrent use.

const (
	// signature domain of a lag ticket body
	LagTicketSignatureDomain = "ur-lag-ticket-v1"
	// signature domain of a lag report body
	LagReportSignatureDomain = "ur-lag-report-v1"
)

// Serializes and signs a ticket body with a root key (A6).
func SignLagTicket(
	body *protocol.LagTicketBody,
	rootPrivateKey ed25519.PrivateKey,
) (*protocol.LagTicket, error) {
	if body == nil {
		return nil, fmt.Errorf("lag ticket body is missing")
	}
	if len(rootPrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("lag ticket root key must be %d bytes, got %d", ed25519.PrivateKeySize, len(rootPrivateKey))
	}
	bodyBytes, err := proto.Marshal(body)
	if err != nil {
		return nil, err
	}
	rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)
	return &protocol.LagTicket{
		Body:          bodyBytes,
		RootSignature: ed25519.Sign(rootPrivateKey, extenderSigningBytes(LagTicketSignatureDomain, bodyBytes)),
		RootKeyId:     ExtenderKeyId(rootPublicKey),
	}, nil
}

// Verifies a ticket under any of the root keys and decodes the body that was
// signed (M3). The key id is a hint, as for records. A ticket is valid while
// now is before its ExpireTimeMs, so one without an expiry never is. The
// expiry bounds only when a session may open, so a ticket is verified once,
// at admission. A caller that tolerates clock skew passes now less its
// tolerance. Whether the ticket names this extender, its network host and an
// unused id are the caller's to check.
func VerifyLagTicket(
	ticket *protocol.LagTicket,
	rootPublicKeys []ed25519.PublicKey,
	now time.Time,
) (*protocol.LagTicketBody, error) {
	if ticket == nil {
		return nil, fmt.Errorf("lag ticket is missing")
	}
	if _, err := NewExtenderRootKeySet(rootPublicKeys...).Verify(
		LagTicketSignatureDomain,
		ticket.Body,
		ticket.RootSignature,
		ticket.RootKeyId,
	); err != nil {
		return nil, err
	}
	body := &protocol.LagTicketBody{}
	if err := proto.Unmarshal(ticket.Body, body); err != nil {
		return nil, err
	}
	if body.ExpireTimeMs <= uint64(now.UnixMilli()) {
		return nil, fmt.Errorf("lag ticket expired at %d ms", body.ExpireTimeMs)
	}
	return body, nil
}

// Serializes and signs a report body with the extender's identity key (M8).
func SignLagReport(
	body *protocol.LagReportBody,
	identityPrivateKey ed25519.PrivateKey,
) (*protocol.LagReport, error) {
	if body == nil {
		return nil, fmt.Errorf("lag report body is missing")
	}
	if len(identityPrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("lag report identity key must be %d bytes, got %d", ed25519.PrivateKeySize, len(identityPrivateKey))
	}
	bodyBytes, err := proto.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &protocol.LagReport{
		Body:      bodyBytes,
		Signature: ed25519.Sign(identityPrivateKey, extenderSigningBytes(LagReportSignatureDomain, bodyBytes)),
	}, nil
}

// Verifies a report under the identity key of the extender that sent it and
// decodes the body that was signed (A7). The body must name that same
// extender, so a report never vouches for another's measurement.
func VerifyLagReport(
	report *protocol.LagReport,
	extenderPublicKey ed25519.PublicKey,
) (*protocol.LagReportBody, error) {
	if report == nil {
		return nil, fmt.Errorf("lag report is missing")
	}
	if len(extenderPublicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("lag report extender key must be %d bytes, got %d", ed25519.PublicKeySize, len(extenderPublicKey))
	}
	if !ed25519.Verify(extenderPublicKey, extenderSigningBytes(LagReportSignatureDomain, report.Body), report.Signature) {
		return nil, fmt.Errorf("lag report signature does not verify under the extender key")
	}
	body := &protocol.LagReportBody{}
	if err := proto.Unmarshal(report.Body, body); err != nil {
		return nil, err
	}
	if !bytes.Equal(body.ExtenderPublicKey, extenderPublicKey) {
		return nil, fmt.Errorf("lag report names another extender than the one that signed it")
	}
	return body, nil
}

// The text form of a ticket in urls and api answers (M4, A6): base64url
// without padding of the serialized ticket.
func EncodeLagTicket(ticket *protocol.LagTicket) (string, error) {
	if ticket == nil {
		return "", fmt.Errorf("lag ticket is missing")
	}
	ticketBytes, err := proto.Marshal(ticket)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ticketBytes), nil
}

// Parses the text form of a ticket. Padding and the standard base64 alphabet
// are refused. The ticket is not verified.
func DecodeLagTicket(text string) (*protocol.LagTicket, error) {
	if text == "" {
		return nil, fmt.Errorf("lag ticket is empty")
	}
	ticketBytes, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		return nil, err
	}
	ticket := &protocol.LagTicket{}
	if err := proto.Unmarshal(ticketBytes, ticket); err != nil {
		return nil, err
	}
	return ticket, nil
}
