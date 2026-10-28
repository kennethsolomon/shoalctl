package testrun

import (
	"strconv"
	"time"
)

// startTolerance absorbs the drift of a start time derived from the wall
// clock (Linux ps computes it from boot time, which an NTP step shifts).
const startTolerance = 2 * time.Second

// SameStart reports whether two ProcessStart values name the same process:
// equal, or within startTolerance of each other.
func SameStart(recorded, live string) bool {
	if recorded == live {
		return true
	}
	a, aok := parseStart(recorded)
	b, bok := parseStart(live)
	if !aok || !bok {
		return false
	}
	d := a.Sub(b)
	return d >= -startTolerance && d <= startTolerance
}

func parseStart(s string) (time.Time, bool) {
	if t, err := time.Parse(time.ANSIC, s); err == nil {
		return t, true
	}
	if ns, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(0, ns), true
	}
	return time.Time{}, false
}
