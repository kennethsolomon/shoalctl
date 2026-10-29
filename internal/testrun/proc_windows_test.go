//go:build windows

package testrun

import "testing"

func TestComparableStartWindows(t *testing.T) {
	for _, tt := range []struct {
		name           string
		recorded, live string
		want           bool
	}{
		{"both nanoseconds", "133000000000000000", "133000003000000000", true},
		{"recorded lstart", "Wed Oct 28 10:11:12 2026", "133000000000000000", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ComparableStart(tt.recorded, tt.live); got != tt.want {
				t.Errorf("ComparableStart(%q, %q) = %v, want %v", tt.recorded, tt.live, got, tt.want)
			}
		})
	}
}

func TestSameStartWindows(t *testing.T) {
	for _, tt := range []struct {
		name           string
		recorded, live string
		want           bool
	}{
		{"identical", "133000000000000000", "133000000000000000", true},
		{"1s apart", "133000000000000000", "133000001000000000", true},
		{"3s apart", "133000000000000000", "133000003000000000", false},
		{"recorded lstart", "Wed Oct 28 10:11:12 2026", "133000000000000000", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := SameStart(tt.recorded, tt.live); got != tt.want {
				t.Errorf("SameStart(%q, %q) = %v, want %v", tt.recorded, tt.live, got, tt.want)
			}
		})
	}
}
