//go:build !js

package connect

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// Tests are external owners of every manager they create. Close only requests
// teardown so callbacks can safely invoke it; a test cleanup must additionally
// join the admitted Pion, signaling, and peer workers before the next root is
// allowed to observe process state.
func closeTestWebRtcManager(t testing.TB, manager *WebRtcManager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := manager.closeAndWait(ctx); err != nil {
		t.Errorf("join test WebRTC manager: %v", err)
	}
}

// Registers the join at construction so an early Fatal, timeout, or partial
// setup cannot bypass the owner boundary. Individual tests may still exercise
// Close or closeAndWait directly; the final cleanup is idempotent.
//
// A test manager never offers the shipped STUN pool, whose servers are public
// hosts: clearing IceServerUrls leaves the pool in force, so a test that names
// no server gathers host candidates only, and one that wants STUN names its
// own servers (IceServerUrls) or pool.
func newTestWebRtcManager(
	t testing.TB,
	ctx context.Context,
	signalSender SignalSender,
	settings *WebRtcSettings,
) *WebRtcManager {
	t.Helper()
	if slices.Equal(settings.IceServerPoolUrls, defaultStunServerUrls) {
		settings.IceServerPoolUrls = nil
	}
	manager := NewWebRtcManager(ctx, signalSender, settings)
	t.Cleanup(func() {
		closeTestWebRtcManager(t, manager)
	})
	return manager
}

// newTestPionPeerConnection is a bare Pion PeerConnection with Pion's default
// codecs and interceptors but multicast DNS off, as the shipped factory sets
// it. With Pion's default (query only), gathering joins 224.0.0.251 and
// ff02::fb on every up interface, and the host sends IGMP and MLD reports.
func newTestPionPeerConnection() (*webrtc.PeerConnection, error) {
	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))
	return api.NewPeerConnection(webrtc.Configuration{})
}

// A test manager offers no server of the shipped STUN pool: one that clears
// IceServerUrls gathers host candidates only, and one that names its own pool
// keeps it.
func TestWebRtcTestManagersOfferNoPublicStunPool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleared := DefaultWebRtcSettings()
	cleared.Log = NewNoopLogger()
	cleared.IceServerUrls = nil
	newTestWebRtcManager(t, ctx, newSignalPipe(nil), cleared)
	if urls := cleared.selectIceServerUrls(); len(urls) != 0 {
		t.Fatalf("a test manager offers %v", urls)
	}
	named := DefaultWebRtcSettings()
	named.Log = NewNoopLogger()
	named.IceServerPoolUrls = []string{"stun:127.0.0.1:3478"}
	newTestWebRtcManager(t, ctx, newSignalPipe(nil), named)
	if urls := named.selectIceServerUrls(); !slices.Equal(urls, []string{"stun:127.0.0.1:3478"}) {
		t.Fatalf("a test manager dropped its own pool, offers %v", urls)
	}
}

// Every test and benchmark manager must be registered with the joining owner
// at construction. A deferred Close is insufficient because it only requests
// teardown and lets the next root race Pion cleanup.
func TestWebRtcTestManagersHaveJoiningOwners(t *testing.T) {
	_, helperPath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate WebRTC test owner helper")
	}
	directory := filepath.Dir(helperPath)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	rawConstructorCount := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			t.Errorf("parse %s: %v", entry.Name(), parseErr)
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			constructor, isIdentifier := call.Fun.(*ast.Ident)
			if !isIdentifier || constructor.Name != "NewWebRtcManager" {
				return true
			}
			rawConstructorCount++
			if path != helperPath {
				t.Errorf("%s constructs a test WebRTC manager without a joining owner", entry.Name())
			}
			return true
		})
	}
	if rawConstructorCount != 1 {
		t.Fatalf("raw test WebRTC manager constructors = %d, want only the joining helper", rawConstructorCount)
	}
}
