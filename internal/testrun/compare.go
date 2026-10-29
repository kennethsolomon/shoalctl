package testrun

import (
	"sort"
	"strings"
)

type Class string

const (
	Regression   Class = "regression"
	Missing      Class = "missing"
	NewlySkipped Class = "newly-skipped"
	Unstable     Class = "unstable"
	NewFailure   Class = "new-failure"
	PreExisting  Class = "pre-existing"
	Fixed        Class = "fixed"
	New          Class = "new"
	OK           Class = "ok"
)

// Classes is the report order.
var Classes = []Class{Regression, Missing, NewlySkipped, Unstable, NewFailure, PreExisting, Fixed, New, OK}

// BlockingClasses block a merge (§8.5); the others are reported.
var BlockingClasses = []Class{Regression, Missing, NewlySkipped, Unstable, NewFailure}

// Flaky is the effective status of a test that failed and then passed on a
// re-run within one run (§8.7).
const Flaky = "flaky"

// Compare classifies every test id in either run (§8.5). Both maps go from
// test id to pass | fail | skip | flaky. A flaky baseline counts as a pass
// (callers drop quarantined tests first); a flaky result after a baseline
// pass, or for a test the baseline lacks, is unstable.
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
	if baseline == Flaky {
		baseline = "pass"
	}
	switch baseline {
	case "pass":
		switch final {
		case Flaky:
			return Unstable
		case "pass":
			return OK
		case "fail":
			return Regression
		case "skip":
			return NewlySkipped
		}
		return Missing
	case "fail":
		if final == "pass" || final == Flaky {
			return Fixed
		}
		return PreExisting
	case "skip":
		switch final {
		case "fail":
			return NewFailure
		case Flaky:
			return Unstable
		}
		return OK
	}
	switch final {
	case "fail":
		return NewFailure
	case Flaky:
		return Unstable
	case "pass":
		return New
	}
	return OK
}

// Blocking lists the test ids in blocking classes, sorted.
func Blocking(classes map[Class][]string) []string {
	var out []string
	for _, c := range BlockingClasses {
		out = append(out, classes[c]...)
	}
	sort.Strings(out)
	return out
}

// Without returns m minus the ids in drop.
func Without(m map[string]string, drop map[string]bool) map[string]string {
	out := make(map[string]string, len(m))
	for id, st := range m {
		if !drop[id] {
			out[id] = st
		}
	}
	return out
}

// RerunCommand fills a rerun_command template: {tests} becomes the ids,
// each shell-quoted and space-separated. {junit} is left for Run.
func RerunCommand(template string, ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = shellQuote(id)
	}
	return strings.ReplaceAll(template, "{tests}", strings.Join(quoted, " "))
}
