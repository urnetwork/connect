package connect

// Pins vless delivery evidence to its configuration through ranking, serial
// preference, replacement and late completion, without dialing a real endpoint.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Uses only documentation addresses and synthetic credentials.
func strategyVlessIdentityTestConfig(address string) *VlessConfig {
	return &VlessConfig{
		Address:  address,
		Port:     443,
		Id:       "synthetic-strategy-user",
		Network:  VlessNetworkTcp,
		Security: VlessSecurityNone,
	}
}

// These entry points need local state only; a fixed clock removes score decay
// from comparisons between a completed endpoint and an untried sibling.
func newStrategyVlessIdentityTestStrategy(t *testing.T, configs ...*VlessConfig) *ClientStrategy {
	t.Helper()
	scores := newNetworkStrategyScores()
	scores.now = func() time.Time { return time.Unix(1700000000, 0) }
	strategy := &ClientStrategy{
		settings: DefaultClientStrategySettings(),
		scores:   scores,
		dialers:  map[*clientDialer]bool{},
	}
	strategy.SetVlessConfigs(configs)
	if len(strategy.dialers) != len(configs) {
		t.Fatal("valid test configurations did not create their dialers")
	}
	t.Cleanup(func() { strategy.SetVlessConfigs(nil) })
	return strategy
}

// Finds an endpoint without relying on map iteration order.
func strategyVlessIdentityTestDialer(t *testing.T, strategy *ClientStrategy, address string) *clientDialer {
	t.Helper()
	for dialer := range strategy.dialers {
		if dialer.vlessConfig != nil && dialer.vlessConfig.Address == address {
			return dialer
		}
	}
	t.Fatalf("missing test endpoint %s", address)
	return nil
}

// A delivered endpoint must improve only its own shuffle and serial preference.
func TestStrategyVlessDeliveryBelongsToOneServer(t *testing.T) {
	strategy := newStrategyVlessIdentityTestStrategy(t,
		strategyVlessIdentityTestConfig("192.0.2.1"),
		strategyVlessIdentityTestConfig("192.0.2.2"))
	deliveredDialer := strategyVlessIdentityTestDialer(t, strategy, "192.0.2.1")
	untriedDialer := strategyVlessIdentityTestDialer(t, strategy, "192.0.2.2")
	base := strategy.dialerWeights(false)[untriedDialer]
	info := strategy.dialerInfo(deliveredDialer)
	strategy.RecordDeliveryOutcome(info, deliveryVerifiedByteCount, false)
	weights := strategy.dialerWeights(false)
	if weights[deliveredDialer] <= base || weights[untriedDialer] != base {
		t.Fatalf("delivery mixed endpoints: delivered=%g untried=%g base=%g", weights[deliveredDialer], weights[untriedDialer], base)
	}
	if !strategy.dialerDelivered(deliveredDialer) || strategy.dialerDelivered(untriedDialer) {
		t.Fatal("serial preference mixed endpoints")
	}
	if info.strategyDialerKey() != deliveredDialer.dialerKey() {
		t.Fatal("attempt and ranking use different endpoint identities")
	}
}

// Keeping the number of servers unchanged must still invalidate the old route.
func TestStrategyVlessReplacementInvalidatesDelivery(t *testing.T) {
	strategy := newStrategyVlessIdentityTestStrategy(t, strategyVlessIdentityTestConfig("192.0.2.1"))
	originalDialer := strategyVlessIdentityTestDialer(t, strategy, "192.0.2.1")
	strategy.RecordDeliveryOutcome(strategy.dialerInfo(originalDialer), deliveryVerifiedByteCount, false)
	originalHash := strategy.scores.configHash
	strategy.SetVlessConfigs([]*VlessConfig{strategyVlessIdentityTestConfig("192.0.2.2")})
	replacementDialer := strategyVlessIdentityTestDialer(t, strategy, "192.0.2.2")
	if strategy.scores.configHash == originalHash || strategy.dialerDelivered(replacementDialer) || len(strategy.scores.scores) != 0 {
		t.Fatal("new endpoint inherited the previous endpoint's configuration and delivery evidence")
	}
}

// Completion is explicitly ordered after replacement, so the old attempt can
// neither repopulate the store nor make the new endpoint a serial winner.
func TestStrategyVlessOldCompletionCannotCreditReplacement(t *testing.T) {
	strategy := newStrategyVlessIdentityTestStrategy(t, strategyVlessIdentityTestConfig("192.0.2.1"))
	oldInfo := strategy.dialerInfo(strategyVlessIdentityTestDialer(t, strategy, "192.0.2.1"))
	strategy.SetVlessConfigs([]*VlessConfig{strategyVlessIdentityTestConfig("192.0.2.2")})
	strategy.RecordDeliveryOutcome(oldInfo, deliveryVerifiedByteCount, false)
	if len(strategy.scores.scores) != 0 || strategy.dialerDelivered(strategyVlessIdentityTestDialer(t, strategy, "192.0.2.2")) {
		t.Fatal("late completion repopulated a replaced configuration")
	}
}

// A race may select an endpoint just before the configuration replaces its
// dialer object. Even a surviving key must not stamp that retired selection
// with the replacement's configuration generation.
func TestStrategyVlessRetiredSelectionCannotStampReplacement(t *testing.T) {
	first := strategyVlessIdentityTestConfig("192.0.2.1")
	strategy := newStrategyVlessIdentityTestStrategy(t, first, strategyVlessIdentityTestConfig("192.0.2.2"))
	selected := strategyVlessIdentityTestDialer(t, strategy, first.Address)
	strategy.SetVlessConfigs([]*VlessConfig{first, strategyVlessIdentityTestConfig("192.0.2.3")})
	info := strategy.dialerInfo(selected)
	strategy.RecordDeliveryOutcome(info, deliveryVerifiedByteCount, false)
	if info.delivery != nil || len(strategy.scores.scores) != 0 {
		t.Fatal("retired selection acquired replacement configuration ownership")
	}
}

// A configuration returning after an intervening replacement is a new
// generation, even though its stable identity is the same as the original.
func TestStrategyVlessOldCompletionCannotCreditReturningConfiguration(t *testing.T) {
	first := strategyVlessIdentityTestConfig("192.0.2.1")
	strategy := newStrategyVlessIdentityTestStrategy(t, first)
	oldInfo := strategy.dialerInfo(strategyVlessIdentityTestDialer(t, strategy, first.Address))
	strategy.SetVlessConfigs([]*VlessConfig{strategyVlessIdentityTestConfig("192.0.2.2")})
	strategy.SetVlessConfigs([]*VlessConfig{first})
	strategy.RecordDeliveryOutcome(oldInfo, deliveryVerifiedByteCount, false)
	if len(strategy.scores.scores) != 0 {
		t.Fatal("old completion acquired a returning configuration's new generation")
	}
}

// Copies, display labels and unused crawl hints do not change the scored route.
func TestStrategyVlessSameConfigurationRetainsIdentity(t *testing.T) {
	config := strategyVlessIdentityTestConfig("192.0.2.1")
	strategy := newStrategyVlessIdentityTestStrategy(t, config)
	originalDialer := strategyVlessIdentityTestDialer(t, strategy, config.Address)
	strategy.RecordDeliveryOutcome(strategy.dialerInfo(originalDialer), deliveryVerifiedByteCount, false)
	originalHash, originalKey := strategy.scores.configHash, originalDialer.dialerKey()
	copiedConfig := config.Copy()
	copiedConfig.Name = "synthetic renamed route"
	copiedConfig.SpiderX = "/unused-crawl-hint"
	copiedConfig.Network, copiedConfig.Security = "", ""
	strategy.SetVlessConfigs([]*VlessConfig{copiedConfig})
	replacementDialer := strategyVlessIdentityTestDialer(t, strategy, config.Address)
	if replacementDialer.dialerKey() != originalKey || strategy.scores.configHash != originalHash || !strategy.dialerDelivered(replacementDialer) {
		t.Fatal("an equivalent configuration lost its existing delivery identity")
	}
}

// Each behavior-bearing configuration field separates evidence and invalidates
// old attempts; changes here are valid configurations, not rejected inputs.
func TestStrategyVlessMeaningfulConfigurationChangesIdentity(t *testing.T) {
	base := strategyVlessIdentityTestConfig("192.0.2.1")
	base.Security = VlessSecurityTls
	base.ServerName = "relay.example"
	base.PublicKey = bytes.Repeat([]byte{1}, 32)
	base.ShortId = []byte{1}
	changes := []struct {
		name   string
		change func(*VlessConfig)
	}{
		{name: "address", change: func(config *VlessConfig) { config.Address = "192.0.2.2" }},
		{name: "port", change: func(config *VlessConfig) { config.Port = 8443 }},
		{name: "user", change: func(config *VlessConfig) { config.Id = "synthetic-other-user" }},
		{name: "flow", change: func(config *VlessConfig) { config.Flow = VlessFlowVision }},
		{name: "network", change: func(config *VlessConfig) { config.Network = VlessNetworkWs }},
		{name: "security", change: func(config *VlessConfig) { config.Security = VlessSecurityReality }},
		{name: "server name", change: func(config *VlessConfig) { config.ServerName = "other.example" }},
		{name: "fingerprint", change: func(config *VlessConfig) { config.Fingerprint = "chrome" }},
		{name: "protocols", change: func(config *VlessConfig) { config.Alpns = []string{"http/1.1"} }},
		{name: "verification", change: func(config *VlessConfig) { config.AllowInsecure = true }},
		{name: "public key", change: func(config *VlessConfig) { config.PublicKey[0] = 2 }},
		{name: "short id", change: func(config *VlessConfig) { config.ShortId[0] = 2 }},
		{name: "path", change: func(config *VlessConfig) { config.Path = "/other" }},
		{name: "host", change: func(config *VlessConfig) { config.Host = "other.example" }},
	}
	for _, change := range changes {
		strategy := newStrategyVlessIdentityTestStrategy(t, base)
		originalDialer := strategyVlessIdentityTestDialer(t, strategy, base.Address)
		originalKey, originalHash := originalDialer.dialerKey(), strategy.scores.configHash
		oldInfo := strategy.dialerInfo(originalDialer)
		changed := base.Copy()
		change.change(changed)
		strategy.SetVlessConfigs([]*VlessConfig{changed})
		replacementDialer := strategyVlessIdentityTestDialer(t, strategy, changed.Address)
		strategy.RecordDeliveryOutcome(oldInfo, deliveryVerifiedByteCount, false)
		if replacementDialer.dialerKey() == originalKey || strategy.scores.configHash == originalHash || len(strategy.scores.scores) != 0 {
			t.Errorf("%s change reused configuration evidence", change.name)
		}
	}
}

// Public attempt metadata and family telemetry must carry neither configuration
// material nor the private hash used to separate endpoints.
func TestStrategyVlessIdentityStaysPrivate(t *testing.T) {
	config := strategyVlessIdentityTestConfig("192.0.2.1")
	strategy := newStrategyVlessIdentityTestStrategy(t, config)
	dialer := strategyVlessIdentityTestDialer(t, strategy, config.Address)
	info := strategy.dialerInfo(dialer)
	strategy.RecordDeliveryOutcome(info, deliveryVerifiedByteCount, false)
	report := strategy.scores.report()
	if len(report.Families) != 1 || report.Families["vless"].Delivered != 1 || info.Description != "vless" {
		t.Fatal("private endpoint identity escaped into the public strategy family")
	}
	publicBytes, err := json.Marshal([]any{info, report})
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{config.Address, config.Id, strings.TrimPrefix(dialer.dialerKey(), "vless|")} {
		if strings.Contains(string(publicBytes), private) && private != "vless" {
			t.Fatal("private endpoint identity escaped into serialized metadata")
		}
	}
}
