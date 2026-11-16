package connect

// Root-cause tests for the camouflage clock: the sealed time is corrected only
// from an authenticated source, and an unauthenticated failure never sets the
// clock or disables an identity check (EXTENDER-UPDATE2 §3.2, EXTENDER.md P8).

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"testing"
	"time"
)

func httpHeaderWithDate(date time.Time) http.Header {
	header := http.Header{}
	header.Set("Date", date.UTC().Format(http.TimeFormat))
	return header
}

// A verified extender response's Date corrects the seal time for that extender,
// bringing a skewed client's sealed time back toward the server's (P8).
func TestExtenderCamouflageClockCorrectsFromVerifiedExtender(t *testing.T) {
	// the client's clock is ten minutes slow
	trueNow := time.Unix(1_800_000_000, 0)
	skew := -10 * time.Minute
	restore := Testing_SetExtenderCamouflageLocalClock(func() time.Time { return trueNow.Add(skew) })
	defer restore()

	key := make([]byte, ed25519.PublicKeySize)
	source := newExtenderCamouflageTimeSource(&ExtenderConfig{PublicKey: key}, true)
	if source == nil {
		t.Fatal("a verified config produced no time source")
	}
	// the extender's Date is the true time; the request bracket is the skewed
	// local clock, which is what the client reads
	sendTime := trueNow.Add(skew)
	source.observe(httpHeaderWithDate(trueNow), sendTime, sendTime)

	offset := Testing_ExtenderCamouflageClockOffset(key)
	// the offset should roughly cancel the skew (bring +10m forward)
	if offset < 9*time.Minute || 11*time.Minute < offset {
		t.Fatalf("offset = %s, expected about +10m to cancel the skew", offset)
	}
	// the sealed time is now within a window of the true server time
	sealed := extenderCamouflageSealTime(key)
	if d := sealed.Sub(trueNow); d < -time.Minute || time.Minute < d {
		t.Fatalf("sealed time off true by %s, expected within a minute", d)
	}
}

// A config whose outer leaf is not verified against a record key is no time
// source: an unauthenticated response must not set the clock (§3.2).
func TestExtenderCamouflageClockIgnoresUnverifiedSource(t *testing.T) {
	trueNow := time.Unix(1_800_000_000, 0)
	restore := Testing_SetExtenderCamouflageLocalClock(func() time.Time { return trueNow })
	defer restore()

	key := make([]byte, ed25519.PublicKeySize)
	// leafVerified false: not an authenticated source
	if source := newExtenderCamouflageTimeSource(&ExtenderConfig{PublicKey: key}, false); source != nil {
		t.Fatal("an unverified config produced a time source")
	}
	// a config with no record key is not a source either
	if source := newExtenderCamouflageTimeSource(&ExtenderConfig{}, true); source != nil {
		t.Fatal("a keyless config produced a time source")
	}
	// a nil source's observe is a no-op and leaves the offset at zero
	var nilSource *extenderCamouflageTimeSource
	nilSource.observe(httpHeaderWithDate(trueNow.Add(time.Hour)), trueNow, trueNow)
	if offset := Testing_ExtenderCamouflageClockOffset(key); offset != 0 {
		t.Fatalf("offset = %s after an unauthenticated observation, expected zero", offset)
	}
}

// The platform fallback counts only a response whose TLS chain verified: an
// unverified platform response must not move the clock (§3.2).
func TestExtenderCamouflageClockPlatformRequiresVerifiedChain(t *testing.T) {
	trueNow := time.Unix(1_800_000_000, 0)
	restore := Testing_SetExtenderCamouflageLocalClock(func() time.Time { return trueNow })
	defer restore()

	key := make([]byte, ed25519.PublicKeySize)
	date := trueNow.Add(5 * time.Minute)

	// no TLS state: refused
	noTls := &http.Response{Header: httpHeaderWithDate(date)}
	if ObserveExtenderCamouflagePlatformTime(noTls, trueNow, trueNow) {
		t.Fatal("a response with no TLS state set the platform clock")
	}
	// TLS present but no verified chain: refused
	unverified := &http.Response{Header: httpHeaderWithDate(date), TLS: &tls.ConnectionState{}}
	if ObserveExtenderCamouflagePlatformTime(unverified, trueNow, trueNow) {
		t.Fatal("a response with no verified chain set the platform clock")
	}
	if offset := Testing_ExtenderCamouflageClockOffset(key); offset != 0 {
		t.Fatalf("offset = %s after unverified platform responses, expected zero", offset)
	}

	// a verified chain is accepted and becomes the fallback for an extender
	// with no observation of its own
	verified := &http.Response{
		Header: httpHeaderWithDate(date),
		TLS:    &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{}}},
	}
	if !ObserveExtenderCamouflagePlatformTime(verified, trueNow, trueNow) {
		t.Fatal("a verified platform response was not accepted")
	}
	if offset := Testing_ExtenderCamouflageClockOffset(key); offset < 4*time.Minute || 6*time.Minute < offset {
		t.Fatalf("platform offset = %s, expected about +5m", offset)
	}
}

// An extender's own observation takes precedence over the platform fallback,
// and an expired observation is not used (P8 bounds).
func TestExtenderCamouflageClockExpiryAndPrecedence(t *testing.T) {
	trueNow := time.Unix(1_800_000_000, 0)
	nowValue := trueNow
	restore := Testing_SetExtenderCamouflageLocalClock(func() time.Time { return nowValue })
	defer restore()

	key := make([]byte, ed25519.PublicKeySize)
	// platform says +5m, the extender itself says +2m
	ObserveExtenderCamouflagePlatformTime(&http.Response{
		Header: httpHeaderWithDate(trueNow.Add(5 * time.Minute)),
		TLS:    &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{}}},
	}, trueNow, trueNow)
	source := newExtenderCamouflageTimeSource(&ExtenderConfig{PublicKey: key}, true)
	source.observe(httpHeaderWithDate(trueNow.Add(2*time.Minute)), trueNow, trueNow)

	// the extender's own offset wins
	if offset := Testing_ExtenderCamouflageClockOffset(key); offset < time.Minute || 3*time.Minute < offset {
		t.Fatalf("offset = %s, expected the extender's own ~+2m to win over the platform", offset)
	}
	// past the maximum age the monotonic clock advances, so both observations
	// expire and the offset falls back to zero
	nowValue = trueNow.Add(extenderCamouflageClockMaxAge + time.Hour)
	if offset := Testing_ExtenderCamouflageClockOffset(key); offset != 0 {
		t.Fatalf("offset = %s past the maximum age, expected zero", offset)
	}
}
