// Physical A/B/A policy identity comes from the resolved configuration and
// actual destination sequences. Fresh evidence is an independent diagnostic.
package connect

// An effective scale of zero records a disabled rule even when its enum or
// configured scale still asks for delivery sizing.
type windowPhysicalH1ResolvedPolicy struct {
	WindowSizing       WindowSizingPolicyKind
	Scale              int
	EffectiveScale     int
	HasMemoryBudget    bool
	HasDeliveryHistory bool
}

// Capture both construction and actual sequence owners, without treating a
// momentary missing service/cumulative sample as a different policy.
type windowPhysicalH1PolicyReading struct {
	Constructor windowPhysicalH1ResolvedPolicy
	Sequences   []windowPhysicalH1ResolvedPolicy
}

// Constructor settings and instrument allocation are immutable after owner
// creation. Copy the map's owners under its lock; inspect them after release.
func windowPhysicalH1PolicySnapshot(client *Client, destination Id) windowPhysicalH1PolicyReading {
	buffer := client.sendBuffer
	settings := buffer.sendBufferSettings
	reading := windowPhysicalH1PolicyReading{Constructor: windowPhysicalH1ResolvedPolicy{
		WindowSizing: settings.WindowSizing, Scale: settings.DeliverySizedWindowScale,
		HasMemoryBudget: settings.ResendQueueBudget != nil,
	}}
	if settings.WindowSizingActive() {
		reading.Constructor.EffectiveScale = settings.DeliverySizedWindowScale
	}
	var sequences []*SendSequence
	func() {
		buffer.mutex.Lock()
		defer buffer.mutex.Unlock()
		for id, sequence := range buffer.sendSequences {
			if id.Destination == destination {
				sequences = append(sequences, sequence)
			}
		}
	}()
	for _, sequence := range sequences {
		settings := sequence.sendBufferSettings
		resolved := windowPhysicalH1ResolvedPolicy{WindowSizing: settings.WindowSizing, Scale: settings.DeliverySizedWindowScale, HasDeliveryHistory: sequence.deliveredBytes != nil, HasMemoryBudget: sequence.resendQueue != nil && sequence.resendQueue.Budget() != nil}
		if 0 < resolved.Scale && resolved.HasDeliveryHistory && resolved.HasMemoryBudget && !settings.ReliableAdmissionBoundedByDelivery {
			resolved.EffectiveScale = resolved.Scale
		}
		reading.Sequences = append(reading.Sequences, resolved)
	}
	return reading
}

// Check the declared arm against resolved owners. Neither Sized nor
// ServiceSized is policy identity: either can age out on a valid retained arm.
func windowPhysicalH1ArmPolicyMatches(arm string, reading windowPhysicalH1Reading, direction int) bool {
	policy := reading.Policy[direction]
	if len(policy.Sequences) == 0 {
		return false
	}
	wanted := WindowSizingConstant
	switch arm {
	case "delivery":
		wanted = WindowSizingFromDelivery
	case "ceiling-before", "ceiling-after":
	default:
		return false
	}
	constructor := policy.Constructor
	if constructor.WindowSizing != wanted {
		return false
	}
	if wanted == WindowSizingFromDelivery {
		if constructor.Scale <= 0 || constructor.EffectiveScale != constructor.Scale {
			return false
		}
	} else if constructor.Scale != 0 || constructor.EffectiveScale != 0 {
		return false
	}
	for _, sequence := range policy.Sequences {
		if sequence.WindowSizing != wanted || sequence.Scale != constructor.Scale || sequence.EffectiveScale != constructor.EffectiveScale {
			return false
		}
	}
	return true
}
