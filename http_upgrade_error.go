// Upgrade policy consumes the same bounded original cause graph as the HTTP
// operation owner. Foreign Is/As methods never grant protocol permission.
package connect

// Preserve every actual refusal and require a complete graph. A nil, hard,
// canceled or incompletely inspected cause prevents a fresh negotiation.
func httpUpgradeFallbackCauses(err error) []*HTTPUpgradeError {
	causes := flattenHttpRequestCauses(err)
	if len(causes) == 0 {
		return nil
	}
	upgradeTs := make([]*HTTPUpgradeError, 0, len(causes))
	for _, cause := range causes {
		upgrade, ok := cause.err.(*HTTPUpgradeError)
		if !ok || upgrade == nil || upgrade.Terminal {
			return nil
		}
		upgradeTs = append(upgradeTs, upgrade)
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
