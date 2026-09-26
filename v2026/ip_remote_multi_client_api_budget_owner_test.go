package connect

import "testing"

// The generator owns its default window carrier budget; a second generator
// must not compete for that admission while windows within one do share it.
func TestApiMultiClientGeneratorDefaultCarrierBudgetIsInstanceOwned(t *testing.T) {
	first := &ApiMultiClientGenerator{
		settings:                       &ApiMultiClientGeneratorSettings{},
		defaultPlatformTransportBudget: DefaultPlatformTransportBudget(),
	}
	second := &ApiMultiClientGenerator{
		settings:                       &ApiMultiClientGeneratorSettings{},
		defaultPlatformTransportBudget: DefaultPlatformTransportBudget(),
	}
	firstWindow := first.newPlatformTransportSettings()
	secondWindow := first.newPlatformTransportSettings()
	otherWindow := second.newPlatformTransportSettings()
	if firstWindow.PlatformTransportBudget != secondWindow.PlatformTransportBudget {
		t.Fatal("windows in one generator have separate admission budgets")
	}
	if firstWindow.PlatformTransportBudget == otherWindow.PlatformTransportBudget {
		t.Fatal("unrelated generators share admission budget")
	}
	claims := make([]*platformTransportBudgetReservation, 0, firstWindow.PlatformTransportBudget.Stats().MaxTransportCount)
	for range cap(claims) {
		claim := firstWindow.PlatformTransportBudget.register(platformTransportBudgetH1, kib(256), true)
		if !claim.TryAcquire() {
			t.Fatal("generator failed to fill its own slot cap")
		}
		claims = append(claims, claim)
	}
	blocked := secondWindow.PlatformTransportBudget.register(platformTransportBudgetH1, kib(256), true)
	if blocked.TryAcquire() {
		t.Fatal("window bypassed its generator's slot cap")
	}
	other := otherWindow.PlatformTransportBudget.register(platformTransportBudgetH1, kib(256), true)
	if !other.TryAcquire() {
		t.Fatal("one generator's cap blocked another")
	}
	other.Release()
	blocked.Release()
	for _, claim := range claims {
		claim.Release()
	}
}

func TestApiMultiClientGeneratorHonorsExplicitCarrierOwner(t *testing.T) {
	owner := NewPlatformTransportBudget(mib(8), 32)
	template := DefaultPlatformTransportSettings()
	template.PlatformTransportBudget = owner
	generator := &ApiMultiClientGenerator{
		settings: &ApiMultiClientGeneratorSettings{
			PlatformTransportSettingsGenerator: func() *PlatformTransportSettings { return template },
		},
		defaultPlatformTransportBudget: DefaultPlatformTransportBudget(),
	}
	first := generator.newPlatformTransportSettings()
	second := generator.newPlatformTransportSettings()
	if first.PlatformTransportBudget != owner || second.PlatformTransportBudget != owner || first == template || second == template {
		t.Fatal("explicit owner or settings copy was lost")
	}
}
