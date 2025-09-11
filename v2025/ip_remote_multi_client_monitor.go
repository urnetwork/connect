package connect

import (
	"sync"
	// "time"

	"golang.org/x/exp/maps"
)

// events surfaced to the end user

// the callback only received the `providerEvent` diffs. To get the full list of provider events, use `Events`
type MonitorEventFunction = func(windowExpandEvent *WindowExpandEvent, providerEvents map[Id]*ProviderEvent, reset bool)

type WindowExpandEvent struct {
	// EventTime   time.Time
	// CurrentSize int
	TargetSize   int
	MinSatisfied bool
}

// provider state machine is:
// ProviderStateInEvaluation
//
//	-> ProviderStateEvaluationFailed (terminal)
//	-> ProviderStateNotAdded (terminal)
//	-> ProviderStateAdded
//	  -> ProviderStateRemoved (terminal)
type ProviderState string

const (
	ProviderStateInEvaluation     ProviderState = "InEvaluation"
	ProviderStateEvaluationFailed ProviderState = "EvaluationFailed"
	ProviderStateNotAdded         ProviderState = "NotAdded"
	ProviderStateAdded            ProviderState = "Added"
	ProviderStateRemoved          ProviderState = "Removed"
)

func (self ProviderState) IsTerminal() bool {
	switch self {
	case ProviderStateEvaluationFailed, ProviderStateNotAdded, ProviderStateRemoved:
		return true
	default:
		return false
	}
}

func (self ProviderState) IsActive() bool {
	switch self {
	case ProviderStateAdded:
		return true
	default:
		return false
	}
}

type ProviderEvent struct {
	// EventTime time.Time
	ClientId Id
	State    ProviderState
}

func DefaultRemoteUserNatMultiClientMonitorSettings() *RemoteUserNatMultiClientMonitorSettings {
	return &RemoteUserNatMultiClientMonitorSettings{
		// EventWindowDuration: 120 * time.Second,
	}
}

type RemoteUserNatMultiClientMonitorSettings struct {
	// EventWindowDuration time.Duration
}

type MultiClientMonitor interface {
	AddMonitorEventCallback(monitorEventCallback MonitorEventFunction) func()
	Events() (*WindowExpandEvent, map[Id]*ProviderEvent)
	WindowExpandEvent() *WindowExpandEvent
	ProviderEvents() map[Id]*ProviderEvent
}

// conforms to `MultiClientMonitor`
type RemoteUserNatMultiClientMonitor struct {
	settings *RemoteUserNatMultiClientMonitorSettings

	stateLock sync.Mutex

	windowExpandEvent      WindowExpandEvent
	clientIdProviderEvents map[Id]*ProviderEvent

	monitorEventCallbacks *CallbackList[MonitorEventFunction]
}

func NewRemoteUserNatMultiClientMonitorWithDefaults() *RemoteUserNatMultiClientMonitor {
	return NewRemoteUserNatMultiClientMonitor(DefaultRemoteUserNatMultiClientMonitorSettings())
}

func NewRemoteUserNatMultiClientMonitor(settings *RemoteUserNatMultiClientMonitorSettings) *RemoteUserNatMultiClientMonitor {
	return &RemoteUserNatMultiClientMonitor{
		settings: settings,
		windowExpandEvent: WindowExpandEvent{
			// EventTime:   time.Now(),
			// CurrentSize: 0,
			TargetSize:   0,
			MinSatisfied: false,
		},
		clientIdProviderEvents: map[Id]*ProviderEvent{},
		monitorEventCallbacks:  NewCallbackList[MonitorEventFunction](),
	}
}

func (self *RemoteUserNatMultiClientMonitor) AddMonitorEventCallback(monitorEventCallback MonitorEventFunction) func() {
	callbackId := self.monitorEventCallbacks.Add(monitorEventCallback)
	return func() {
		self.monitorEventCallbacks.Remove(callbackId)
	}
}

/*
func (self *RemoteUserNatMultiClientMonitor) event() {
	callbacks := self.monitorEventCallbacks.Get()
	if len(callbacks) == 0 {
		return
	}

	var windowExpandEvent *WindowExpandEvent
	clientIdProviderEvents := map[Id]*ProviderEvent{}

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		self.coalesceProviderEvents()

		windowExpandEvent = self.windowExpandEvent
		for _, providerEvent := range self.providerEvents {
			clientIdProviderEvents[providerEvent.ClientId] = providerEvent
		}
	}()

	for _, callback := range callbacks {
		callback(windowExpandEvent, clientIdProviderEvents)
	}
}
*/

// must be called with `stateLock`
// func (self *RemoteUserNatMultiClientMonitor) coalesceProviderEvents() {
// 	windowStartTime := time.Now().Add(-self.settings.EventWindowDuration)

// 	i := 0
// 	for ; i < len(self.providerEvents) && self.providerEvents[i].EventTime.Before(windowStartTime); i += 1 {
// 		self.providerEvents[i] = nil
// 	}
// 	if 0 < i {
// 		self.providerEvents = self.providerEvents[i:]
// 	}
// }

func (self *RemoteUserNatMultiClientMonitor) Events() (*WindowExpandEvent, map[Id]*ProviderEvent) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// make a copy
	windowExpandEvent := self.windowExpandEvent
	return &windowExpandEvent, maps.Clone(self.clientIdProviderEvents)
}

func (self *RemoteUserNatMultiClientMonitor) WindowExpandEvent() *WindowExpandEvent {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// make a copy
	windowExpandEvent := self.windowExpandEvent
	return &windowExpandEvent

}

func (self *RemoteUserNatMultiClientMonitor) ProviderEvents() map[Id]*ProviderEvent {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// make a copy
	return maps.Clone(self.clientIdProviderEvents)
}

func (self *RemoteUserNatMultiClientMonitor) AddWindowExpandEvent(minSatisfied bool, targetSize int) {
	var windowExpandEvent WindowExpandEvent
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		windowExpandEvent = WindowExpandEvent{
			// EventTime:   time.Now(),
			// CurrentSize: currentSize,
			TargetSize:   targetSize,
			MinSatisfied: minSatisfied,
		}

		if self.windowExpandEvent != windowExpandEvent {
			self.windowExpandEvent = windowExpandEvent
			changed = true
		}
	}()

	if changed {
		if callbacks := self.monitorEventCallbacks.Get(); 0 < len(callbacks) {
			for _, callback := range callbacks {
				callback(&windowExpandEvent, map[Id]*ProviderEvent{}, false)
			}
		}
	}
}

// provider events are serialized per `clientId`
func (self *RemoteUserNatMultiClientMonitor) AddProviderEvent(clientId Id, state ProviderState) {
	var windowExpandEvent WindowExpandEvent
	clientIdProviderEvents := map[Id]*ProviderEvent{}

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		providerEvent := &ProviderEvent{
			// EventTime: time.Now(),
			ClientId: clientId,
			State:    state,
		}

		// self.providerEvents = append(self.providerEvents, providerEvent)
		// self.coalesceProviderEvents()
		if state.IsTerminal() {
			delete(self.clientIdProviderEvents, clientId)
		} else {
			self.clientIdProviderEvents[clientId] = providerEvent
		}

		windowExpandEvent = self.windowExpandEvent
		clientIdProviderEvents[providerEvent.ClientId] = providerEvent
	}()

	if callbacks := self.monitorEventCallbacks.Get(); 0 < len(callbacks) {
		for _, callback := range callbacks {
			callback(&windowExpandEvent, clientIdProviderEvents, false)
		}
	}
}

type MergedMultiClientMonitor struct {
	monitors []MultiClientMonitor
}

func NewMergedMultiClientMonitor(monitors []MultiClientMonitor) *MergedMultiClientMonitor {
	return &MergedMultiClientMonitor{
		monitors: monitors,
	}
}

func (self *MergedMultiClientMonitor) AddMonitorEventCallback(monitorEventCallback MonitorEventFunction) func() {
	c := func(_ *WindowExpandEvent, providerEvents map[Id]*ProviderEvent, reset bool) {
		windowExpandEvent := self.WindowExpandEvent()
		monitorEventCallback(windowExpandEvent, providerEvents, reset)
	}

	subs := []func(){}
	for _, monitor := range self.monitors {
		sub := monitor.AddMonitorEventCallback(c)
		subs = append(subs, sub)
	}
	return func() {
		for _, sub := range subs {
			sub()
		}
	}
}

func (self *MergedMultiClientMonitor) Events() (*WindowExpandEvent, map[Id]*ProviderEvent) {
	return self.WindowExpandEvent(), self.ProviderEvents()
}

func (self *MergedMultiClientMonitor) WindowExpandEvent() *WindowExpandEvent {
	netWindowExpandEvent := WindowExpandEvent{
		TargetSize:   0,
		MinSatisfied: true,
	}
	for _, monitor := range self.monitors {
		windowExpandEvent := monitor.WindowExpandEvent()
		netWindowExpandEvent.TargetSize += windowExpandEvent.TargetSize
		netWindowExpandEvent.MinSatisfied = netWindowExpandEvent.MinSatisfied && windowExpandEvent.MinSatisfied
	}
	return &netWindowExpandEvent
}

func (self *MergedMultiClientMonitor) ProviderEvents() map[Id]*ProviderEvent {
	netProviderEvents := map[Id]*ProviderEvent{}
	for _, monitor := range self.monitors {
		providerEvents := monitor.ProviderEvents()
		maps.Copy(netProviderEvents, providerEvents)
	}
	return netProviderEvents
}
