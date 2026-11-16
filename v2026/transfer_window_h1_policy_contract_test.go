// The physical fixture's policy gate accepts valid evidence states while
// rejecting incorrect actual arm configuration. No carrier or timer is used.
package connect

import "testing"

// Build exact constructor/sequence owners with explicit budgets and delivery
// instruments. The snapshot reads real fields, independently of diagnostics.
func newWindowPhysicalH1PolicyContract() (*Client, Id, *SendSequence) {
	settings := DefaultSendBufferSettings()
	settings.WindowSizing = WindowSizingFromDelivery
	settings.ReliableAdmissionBoundedByDelivery = false
	settings.ApplyWindowSizing()
	settings.ResendQueueBudget = NewTransferMemoryBudget(8 * 1024 * 1024)
	destination := NewId()
	sequenceSettings := *settings
	sequence := &SendSequence{
		destination: destination, sendBufferSettings: &sequenceSettings,
		deliveredBytes: make([]deliveredBytesSample, deliveredBytesRingSize),
		resendQueue:    newResendQueue(sequenceSettings.ResendQueueBudget, 0),
	}
	client := &Client{sendBuffer: &SendBuffer{
		sendBufferSettings: settings,
		sendSequences:      map[sendSequenceId]*SendSequence{{Destination: destination}: sequence},
	}}
	return client, destination, sequence
}

// Service can size a valid configured arm without a fresh cumulative interval.
func TestWindowPhysicalH1PolicyGateAcceptsServiceQualified(t *testing.T) {
	client, destination, _ := newWindowPhysicalH1PolicyContract()
	reading := windowPhysicalH1Reading{}
	reading.Policy[0] = windowPhysicalH1PolicySnapshot(client, destination)
	reading.Window[0] = SendWindowEstimate{Sized: false, ServiceSized: true, ServiceByteRate: 12500000, Window: 4 * 1024 * 1024, LearnedWindow: 4 * 1024 * 1024}
	if !windowPhysicalH1ArmPolicyMatches("delivery", reading, 0) {
		t.Fatal("configured delivery arm was rejected solely because its current evidence is service-qualified")
	}
}

// Retained byte capacity remains delivery policy when both current proofs age.
func TestWindowPhysicalH1PolicyGateAcceptsAgedRetainedWindow(t *testing.T) {
	client, destination, _ := newWindowPhysicalH1PolicyContract()
	reading := windowPhysicalH1Reading{}
	reading.Policy[0] = windowPhysicalH1PolicySnapshot(client, destination)
	reading.Window[0] = SendWindowEstimate{Initial: 512 * 1024, Window: 4 * 1024 * 1024, LearnedWindow: 4 * 1024 * 1024}
	if !windowPhysicalH1ArmPolicyMatches("delivery", reading, 0) {
		t.Fatal("aged evidence changed the fixture's identity of a configured retained delivery arm")
	}
}

// A configured constant must be rejected in the delivery arm even when a
// diagnostic happens to carry a previously qualified sample.
func TestWindowPhysicalH1PolicyGateRejectsWrongArmWithFreshEvidence(t *testing.T) {
	client, destination, sequence := newWindowPhysicalH1PolicyContract()
	client.sendBuffer.sendBufferSettings.WindowSizing = WindowSizingConstant
	client.sendBuffer.sendBufferSettings.ApplyWindowSizing()
	sequence.sendBufferSettings.WindowSizing = WindowSizingConstant
	sequence.sendBufferSettings.ApplyWindowSizing()
	reading := windowPhysicalH1Reading{}
	reading.Policy[0] = windowPhysicalH1PolicySnapshot(client, destination)
	reading.Window[0] = SendWindowEstimate{Sized: true, ServiceSized: true, LearnedWindow: 4 * 1024 * 1024}
	if windowPhysicalH1ArmPolicyMatches("delivery", reading, 0) {
		t.Fatal("fresh evidence hid an actual constant-policy constructor")
	}
	if !windowPhysicalH1ArmPolicyMatches("ceiling-before", reading, 0) || !windowPhysicalH1ArmPolicyMatches("ceiling-after", reading, 0) {
		t.Fatal("stale diagnostic evidence hid the correctly configured constant arms")
	}
}

// The enum alone cannot certify a rule disabled by scale, budget, or competing
// admission. These are configuration failures regardless of current evidence.
func TestWindowPhysicalH1PolicyGateRejectsDisabledConstructor(t *testing.T) {
	for _, disable := range []func(*SendBufferSettings){
		func(settings *SendBufferSettings) { settings.DeliverySizedWindowScale = 0 },
		func(settings *SendBufferSettings) { settings.ResendQueueBudget = nil },
		func(settings *SendBufferSettings) { settings.ReliableAdmissionBoundedByDelivery = true },
	} {
		client, destination, _ := newWindowPhysicalH1PolicyContract()
		disable(client.sendBuffer.sendBufferSettings)
		reading := windowPhysicalH1Reading{}
		reading.Policy[0] = windowPhysicalH1PolicySnapshot(client, destination)
		reading.Window[0] = SendWindowEstimate{Sized: true}
		if windowPhysicalH1ArmPolicyMatches("delivery", reading, 0) {
			t.Fatal("fresh evidence hid a disabled constructor rule")
		}
	}
}

// Real sequence state must agree with construction; a disabled, absent, or
// differently configured owner cannot be masked by a positive statistics field.
func TestWindowPhysicalH1PolicyGateRejectsWrongSequenceConfiguration(t *testing.T) {
	for _, change := range []func(*SendSequence){
		func(sequence *SendSequence) { sequence.sendBufferSettings.WindowSizing = WindowSizingConstant },
		func(sequence *SendSequence) { sequence.sendBufferSettings.DeliverySizedWindowScale = 0 },
		func(sequence *SendSequence) { sequence.sendBufferSettings.DeliverySizedWindowScale++ },
		func(sequence *SendSequence) { sequence.deliveredBytes = nil },
		func(sequence *SendSequence) { sequence.resendQueue = newResendQueue(nil, 0) },
		func(sequence *SendSequence) { sequence.sendBufferSettings.ReliableAdmissionBoundedByDelivery = true },
	} {
		client, destination, sequence := newWindowPhysicalH1PolicyContract()
		change(sequence)
		reading := windowPhysicalH1Reading{}
		reading.Policy[0] = windowPhysicalH1PolicySnapshot(client, destination)
		reading.Window[0] = SendWindowEstimate{Sized: true}
		if windowPhysicalH1ArmPolicyMatches("delivery", reading, 0) {
			t.Fatal("constructor and statistics hid a differently configured actual sequence")
		}
	}
}

// An unrelated destination cannot stand in for the actual arm's absent owner.
func TestWindowPhysicalH1PolicyGateScopesActualDestination(t *testing.T) {
	client, _, _ := newWindowPhysicalH1PolicyContract()
	reading := windowPhysicalH1Reading{}
	reading.Policy[0] = windowPhysicalH1PolicySnapshot(client, NewId())
	reading.Window[0] = SendWindowEstimate{Sized: true}
	if windowPhysicalH1ArmPolicyMatches("delivery", reading, 0) {
		t.Fatal("another destination certified a missing actual send sequence")
	}
}

// A complete cumulative proof remains a positive control in either direction.
func TestWindowPhysicalH1PolicyGateKeepsQualifiedPositiveControl(t *testing.T) {
	client, destination, _ := newWindowPhysicalH1PolicyContract()
	reading := windowPhysicalH1Reading{}
	for direction := range 2 {
		reading.Policy[direction] = windowPhysicalH1PolicySnapshot(client, destination)
		reading.Window[direction] = SendWindowEstimate{Sized: true}
		if !windowPhysicalH1ArmPolicyMatches("delivery", reading, direction) {
			t.Fatalf("qualified configured direction%d was rejected", direction)
		}
	}
	client.sendBuffer.sendBufferSettings.WindowSizing = WindowSizingConstant
	client.sendBuffer.sendBufferSettings.ApplyWindowSizing()
	for _, sequence := range client.sendBuffer.sendSequences {
		sequence.sendBufferSettings.WindowSizing = WindowSizingConstant
		sequence.sendBufferSettings.ApplyWindowSizing()
	}
	reading.Policy[0] = windowPhysicalH1PolicySnapshot(client, destination)
	reading.Window[0] = SendWindowEstimate{}
	if !windowPhysicalH1ArmPolicyMatches("ceiling-before", reading, 0) || !windowPhysicalH1ArmPolicyMatches("ceiling-after", reading, 0) {
		t.Fatal("configured constant positive controls were rejected")
	}
}

// An unrecognized arm cannot silently inherit the constant reference policy.
func TestWindowPhysicalH1PolicyGateRejectsUnknownArm(t *testing.T) {
	client, destination, _ := newWindowPhysicalH1PolicyContract()
	reading := windowPhysicalH1Reading{}
	reading.Policy[0] = windowPhysicalH1PolicySnapshot(client, destination)
	if windowPhysicalH1ArmPolicyMatches("undeclared-arm", reading, 0) {
		t.Fatal("unknown arm silently passed as constant")
	}
}
