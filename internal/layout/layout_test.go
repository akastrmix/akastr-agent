package layout

import (
	"testing"
)

func TestCandidateIsTheSlotThatIsNotActive(t *testing.T) {
	paths := Layout{Root: t.TempDir()}
	if inactive, _ := paths.InactiveSlot(); inactive != paths.Slot("a") {
		t.Fatalf("fresh install writes %s, want slot a", inactive)
	}
	if err := paths.Activate(paths.Slot("a")); err != nil {
		t.Fatal(err)
	}
	if inactive, _ := paths.InactiveSlot(); inactive != paths.Slot("b") {
		t.Fatalf("inactive slot = %s, want b", inactive)
	}
	// systemd starts the active slot through the current link.
	if _, candidate, _ := paths.Candidate(SlotConfig(paths.Current())); candidate {
		t.Fatal("the active deployment was treated as a candidate")
	}
	slot, candidate, _ := paths.Candidate(SlotConfig(paths.Slot("b")))
	if !candidate || slot != paths.Slot("b") {
		t.Fatalf("candidate=%v slot=%s", candidate, slot)
	}
	if err := paths.Activate(slot); err != nil {
		t.Fatal(err)
	}
	if _, candidate, _ := paths.Candidate(SlotConfig(paths.Slot("b"))); candidate {
		t.Fatal("an activated candidate still considers itself a candidate")
	}
	if inactive, _ := paths.InactiveSlot(); inactive != paths.Slot("a") {
		t.Fatal("the previous deployment is not the next slot to overwrite")
	}
}
