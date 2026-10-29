package testrun

import "time"

// startTolerance absorbs the drift of a start time derived from the wall
// clock (Linux ps computes it from boot time, which an NTP step shifts).
const startTolerance = 2 * time.Second

// ComparableStart reports whether SameStart can confirm or rule out that two
// ProcessStart values name the same process: they are equal or both parse.
func ComparableStart(recorded, live string) bool {
	if recorded == live {
		return true
	}
	_, aok := parseStart(recorded)
	_, bok := parseStart(live)
	return aok && bok
}

// SameStart reports whether two ProcessStart values may name the same
// process: equal, within startTolerance of each other, or not comparable
// because either fails to parse (a reused pid cannot be proven, so the
// caller must treat it as the same process).
func SameStart(recorded, live string) bool {
	if recorded == live {
		return true
	}
	a, aok := parseStart(recorded)
	b, bok := parseStart(live)
	if !aok || !bok {
		return true
	}
	d := a.Sub(b)
	return d >= -startTolerance && d <= startTolerance
}
