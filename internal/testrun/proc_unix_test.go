//go:build unix

package testrun

import "testing"

func TestComparableStartUnix(t *testing.T) {
	for _, tt := range []struct {
		name           string
		recorded, live string
		want           bool
	}{
		{"both lstart", "Wed Oct 28 10:11:12 2026", "Thu Jan 1 00:00:00 1970", true},
		{"recorded nanoseconds", "0", "Wed Oct 28 10:11:12 2026", false},
		{"recorded year", "1970", "Wed Oct 28 10:11:12 2026", false},
		{"both nanoseconds", "133000000000000000", "133000003000000000", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ComparableStart(tt.recorded, tt.live); got != tt.want {
				t.Errorf("ComparableStart(%q, %q) = %v, want %v", tt.recorded, tt.live, got, tt.want)
			}
		})
	}
}

func TestSameStartUnix(t *testing.T) {
	for _, tt := range []struct {
		name           string
		recorded, live string
		want           bool
	}{
		{"identical lstart", "Wed Oct 28 10:11:12 2026", "Wed Oct 28 10:11:12 2026", true},
		{"lstart shifted 1s by an NTP step", "Wed Oct 28 10:11:12 2026", "Wed Oct 28 10:11:13 2026", true},
		{"lstart shifted 2s", "Wed Oct 28 10:11:12 2026", "Wed Oct 28 10:11:10 2026", true},
		{"single-digit day", "Sat Oct 3 23:59:59 2026", "Sun Oct 4 00:00:00 2026", true},
		{"lstart 3s apart", "Wed Oct 28 10:11:12 2026", "Wed Oct 28 10:11:15 2026", false},
		{"different day", "Wed Oct 28 10:11:12 2026", "Thu Jan 1 00:00:00 1970", false},
		{"recorded nanoseconds", "0", "Wed Oct 28 10:11:12 2026", true},
		{"recorded year", "1970", "Wed Oct 28 10:11:12 2026", true},
		{"both nanoseconds 3s apart", "133000000000000000", "133000003000000000", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := SameStart(tt.recorded, tt.live); got != tt.want {
				t.Errorf("SameStart(%q, %q) = %v, want %v", tt.recorded, tt.live, got, tt.want)
			}
		})
	}
}
