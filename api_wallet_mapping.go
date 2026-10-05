// The coldkey signer receives the exact authenticated mapping challenge. The
// SDK never owns a coldkey and does not reconstruct or normalize signed bytes.
package connect

import "fmt"

// The wallet explicitly approves an inclusive earning interval for one client.
type SnWalletMappingChallengeArgs struct {
	ClientId     *Id    `json:"client_id,omitempty"`
	ColdkeySs58  string `json:"coldkey_ss58"`
	FromEpoch    uint64 `json:"from_epoch"`
	ThroughEpoch uint64 `json:"through_epoch"`
}

// Submit this exact message and its signature through SnSetWallet after approval.
type SnWalletMappingChallengeResult struct {
	Message string `json:"message"`
}

// Completion carries the original message through the SDK's ordinary owner.
type SnWalletMappingChallengeCallback ApiCallback[*SnWalletMappingChallengeResult]

// Challenge issuance is authenticated but never changes the wallet projection.
func (self *BringYourApi) SnWalletMappingChallenge(args *SnWalletMappingChallengeArgs, callback SnWalletMappingChallengeCallback) {
	go HandleError(func() {
		HttpPostWithStrategy(self.ctx, self.clientStrategy, fmt.Sprintf("%s/sn/wallet/consent", self.apiUrl), args, self.ByJwt(), &SnWalletMappingChallengeResult{}, callback)
	})
}

// The synchronous owner observes the actual completed authenticated request.
func (self *BringYourApi) SnWalletMappingChallengeSync(args *SnWalletMappingChallengeArgs) (*SnWalletMappingChallengeResult, error) {
	return HttpPostWithStrategy(self.ctx, self.clientStrategy, fmt.Sprintf("%s/sn/wallet/consent", self.apiUrl), args, self.ByJwt(), &SnWalletMappingChallengeResult{}, NewNoopApiCallback[*SnWalletMappingChallengeResult]())
}
