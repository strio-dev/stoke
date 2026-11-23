package ci

import (
	"bytes"
	"slices"
	"testing"
)

func TestContainerNetworkDefaultIsFrozenInPlan(t *testing.T) {
	w := decodeWorkflow(t, "version: 1\ntasks:\n  check: {image: scratch, steps: [{run: [true]}]}\n")
	base := buildPlanFixture(t, w, Config{})
	p := buildPlanFixture(t, w, Config{Network: "none"})
	if p.ID == base.ID || p.Config.Network != "none" || !slices.Contains(p.Engine.Capabilities, "run-network") || slices.Contains(base.Engine.Capabilities, "run-network") {
		t.Fatal("network does not participate in invocation identity and capabilities", p, base)
	}
	encoded, err := Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	round, err := Decode(bytes.NewReader(encoded))
	if err != nil || round.ID != p.ID || round.Config.Network != "none" {
		t.Fatal("network identity changed on roundtrip", round, err)
	}
}
