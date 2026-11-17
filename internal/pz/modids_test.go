package pz

import (
	"strings"
	"testing"
)

func TestNormalizeModIDs(t *testing.T) {
	got := normalizeModIDs([]string{"", "  Alpha ", `\Beta`, `\`, "TacHold Pistol", "   "})
	if joined, want := strings.Join(got, "|"), "Alpha|Beta|TacHold Pistol"; joined != want {
		t.Fatalf("normalize = %q, want %q", joined, want)
	}
}

func TestValidateModIDs(t *testing.T) {
	valid := []string{"TacHold Pistol", "[J&G] Desert Multicam Uniform", "GanydeBielovzki's Frockin Splendor!"}
	if err := validateModIDs(valid); err != nil {
		t.Fatalf("valid ids rejected: %v", err)
	}
	for _, bad := range []string{"Bad\x00Id", "Tab\tInside", "Bell\x07", "Del\x7f", "New\nLine"} {
		if err := validateModIDs([]string{bad}); err == nil {
			t.Fatalf("control character accepted in %q", bad)
		}
	}
}
