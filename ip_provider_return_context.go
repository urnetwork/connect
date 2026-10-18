// Dedicated TCP admission belongs to both its source verdict and originating
// flow generation. A temporary link keeps either cancellation effective and
// joins its own cancellation callback before the pooled return item is reused.
package connect

import "context"

// One synchronous callback owns this link. The cancellation callback touches
// only this object and its child context, never a pooled packet or return item.
type providerReturnContextLink struct {
	ctx                  context.Context
	cancel               context.CancelFunc
	stopFlowCancellation func() bool
	flowCancellationDone chan struct{}
	// A cancellation-only seam proves close joins the fired callback. Set it
	// before canceling the flow; nil adds no work to ordinary admissions.
	afterFlowCancellationForTest func()
}

// Source/provider cancellation remains the child context's parent. The flow
// contributes another cancellation edge without canceling that shared parent.
func newProviderReturnContextLink(sourceCtx, flowCtx context.Context) *providerReturnContextLink {
	ctx, cancel := context.WithCancel(sourceCtx)
	link := &providerReturnContextLink{
		ctx:                  ctx,
		cancel:               cancel,
		flowCancellationDone: make(chan struct{}),
	}
	link.stopFlowCancellation = context.AfterFunc(flowCtx, func() {
		defer close(link.flowCancellationDone)
		cancel()
		if link.afterFlowCancellationForTest != nil {
			link.afterFlowCancellationForTest()
		}
	})
	// AfterFunc schedules an already-canceled parent asynchronously. Reject
	// admission synchronously when cancellation preceded registration's return.
	if flowCtx.Err() != nil {
		cancel()
	}
	return link
}

// A stopped callback cannot run; a fired one must finish before return. The
// link's cancellation never reaches either parent and owns no persistent worker.
func (self *providerReturnContextLink) close() {
	if !self.stopFlowCancellation() {
		<-self.flowCancellationDone
	}
	self.cancel()
}
