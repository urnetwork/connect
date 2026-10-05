// Upgrade policy consumes the same bounded original cause graph as the HTTP
// operation owner. Foreign Is/As methods never grant protocol permission.
package connect

// Preserve every actual refusal and its response transport causes. A nil,
// hard, canceled or incomplete graph prevents a fresh negotiation.
func httpUpgradeFallbackCauses(err error) []*HTTPUpgradeError {
	causes := flattenHttpRequestCauses(err)
	if len(causes) == 0 {
		return nil
	}
	upgradeTs := make([]*HTTPUpgradeError, 0, len(causes))
	responseIo, transport := false, false
	for _, cause := range causes {
		upgrade, ok := cause.err.(*HTTPUpgradeError)
		if !ok {
			if cause.kind < 2 {
				return nil
			}
			transport = true
			continue
		}
		if upgrade == nil || upgrade.Terminal {
			return nil
		}
		responseIo = responseIo || upgrade.Reason == "response-io"
		upgradeTs = append(upgradeTs, upgrade)
	}
	if transport && !responseIo {
		return nil
	}
	return upgradeTs
}

// Any genuine authorization refusal stops the logical negotiation. Only a
// complete graph of those refusals can exempt an attempted route from failure.
func httpUpgradeTerminalCauses(err error) (terminal, refusalOnly bool) {
	causes := flattenHttpRequestCauses(err)
	refusalOnly = len(causes) != 0
	for _, cause := range causes {
		if upgrade, ok := cause.err.(*HTTPUpgradeError); ok && upgrade != nil && upgrade.Terminal {
			terminal = true
		} else {
			refusalOnly = false
		}
	}
	return
}
