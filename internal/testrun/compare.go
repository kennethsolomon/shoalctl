package testrun

import "sort"

type Class string

const (
	Regression   Class = "regression"
	Missing      Class = "missing"
	NewlySkipped Class = "newly-skipped"
	NewFailure   Class = "new-failure"
	PreExisting  Class = "pre-existing"
	Fixed        Class = "fixed"
	New          Class = "new"
	OK           Class = "ok"
)

// Classes is the report order.
var Classes = []Class{Regression, Missing, NewlySkipped, NewFailure, PreExisting, Fixed, New, OK}

// Compare classifies every test id in either run (§8.5). Both maps go from
// test id to pass | fail | skip.
func Compare(baseline, final map[string]string) map[Class][]string {
	out := map[Class][]string{}
	for id, b := range baseline {
		c := classify(b, final[id])
		out[c] = append(out[c], id)
	}
	for id, f := range final {
		if _, ok := baseline[id]; !ok {
			c := classify("", f)
			out[c] = append(out[c], id)
		}
	}
	for _, ids := range out {
		sort.Strings(ids)
	}
	return out
}

func classify(baseline, final string) Class {
	switch baseline {
	case "pass":
		switch final {
		case "pass":
			return OK
		case "fail":
			return Regression
		case "skip":
			return NewlySkipped
		}
		return Missing
	case "fail":
		if final == "pass" {
			return Fixed
		}
		return PreExisting
	case "skip":
		if final == "fail" {
			return NewFailure
		}
		return OK
	}
	switch final {
	case "fail":
		return NewFailure
	case "pass":
		return New
	}
	return OK
}
