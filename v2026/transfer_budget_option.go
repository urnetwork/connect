// Local protocol ownership is independent of application byte admission.
package connect

// The option carries no wire authority. Callers retain responsibility for
// classifying their own traffic, and ordinary receiver authentication applies.
type transferOptionsNoBudget struct{}

// Excludes a send from application memory budgets and data flight windows.
// Delivery, retries, ACKs, cancellation, message-size validation and ordinary
// channel ownership are unchanged. A receiver independently classifies bytes.
func NoBudget() transferOptionsNoBudget { return transferOptionsNoBudget{} }

// Every coalesced original must explicitly carry the same local policy.
// A group's immutable parent supplies it after its source Pack is returned.
func (self sendAckRecord) bypassesBudget() bool {
	return self.noBudget || self.group != nil && self.group.ack.noBudget
}

// Empty callback records still contribute policy, so an ordinary data member
// cannot inherit a neighboring protocol member's exemption during coalescing.
func (self *sendAckSet) bypassesBudget() bool {
	return self.budgetTracked && !self.hasBudgeted
}
