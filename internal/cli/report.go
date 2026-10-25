package cli

import (
	"fmt"
	"strings"

	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testrun"
)

type reportInput struct {
	task        store.Task
	sum         summaryFile
	head        string
	commits     []string
	files       []changedFile
	base        store.Run
	final       store.Run
	baseResults []store.Result
	classes     map[testrun.Class][]string
	ver         verification
	date        string
}

const (
	reducedFidelity  = "Reduced fidelity: whole-suite pass/fail only. A failing baseline hides any new failure."
	protocolInactive = "Zero-regression protocol inactive: [tests].format = \"none\", so no tests ran."
)

// renderReport renders the 11 report sections of §15.1 in order.
func renderReport(in reportInput) string {
	var b strings.Builder
	t := in.task
	short := make([]string, len(in.commits))
	for i, c := range in.commits {
		short[i] = shortSHA(c)
	}
	fmt.Fprintf(&b, "---\ntask: %s\nlane: %s\nstatus: done\nbranch: %s\nbase: %s@%s\ncommits: [%s]\ndate: %s\n---\n",
		t.Slug, t.Lane, t.Branch, t.BaseRef, shortSHA(t.BaseSHA), strings.Join(short, ", "), in.date)
	fmt.Fprintf(&b, "# %s\n\n", t.Title)
	if in.base.Fidelity == "none" {
		b.WriteString("> " + protocolInactive + "\n\n")
	}

	fmt.Fprintf(&b, "## Asked\n%s\n\n", t.Ask)

	fmt.Fprintf(&b, "## Decision\n%s\n", in.sum.Decision)
	for i, a := range in.sum.AlternativesRejected {
		if i == 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- Rejected: %s (%s)\n", a.Option, a.Why)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "## Why\n%s\n\n", in.sum.Why)

	b.WriteString("## Changed\n")
	notes := map[string]string{}
	for _, c := range in.sum.Changed {
		notes[c.Path] = c.Note
	}
	for _, f := range in.files {
		path := "`" + f.Path + "`"
		if f.From != "" {
			path = "`" + f.From + "` -> `" + f.Path + "`"
		}
		line := fmt.Sprintf("- %s %s", f.Status, path)
		if n := notes[f.Path]; n != "" {
			line += " - " + n
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")

	b.WriteString("## Verified\n")
	fmt.Fprintf(&b, "- Baseline: %s\n", runLine(in.base))
	fmt.Fprintf(&b, "- Final: %s\n", runLine(in.final))
	fmt.Fprintf(&b, "- Lint: %s\n- Build: %s\n\n", in.ver.Lint, in.ver.Build)

	b.WriteString("## Regression check (informational)\n")
	switch {
	case in.base.Fidelity == "none":
		b.WriteString("Not computed: no test suite is configured.\n")
	case in.classes == nil:
		fmt.Fprintf(&b, "Not computed: the final run ended with status %s.\n", in.final.Status)
	default:
		var counts []string
		for _, c := range testrun.Classes {
			counts = append(counts, fmt.Sprintf("%d %s", len(in.classes[c]), c))
		}
		b.WriteString("- " + strings.Join(counts, ", ") + "\n")
		for _, c := range []testrun.Class{testrun.Regression, testrun.Missing, testrun.NewlySkipped, testrun.NewFailure, testrun.Fixed} {
			if ids := in.classes[c]; len(ids) > 0 {
				fmt.Fprintf(&b, "- %s: %s\n", c, codeList(ids))
			}
		}
	}
	b.WriteString("\n")

	b.WriteString("## Pre-existing failures\n")
	var failing []string
	for _, r := range in.baseResults {
		if r.Status == "fail" {
			failing = append(failing, r.TestID)
		}
	}
	if len(failing) == 0 {
		b.WriteString("None at baseline.\n\n")
	} else {
		fmt.Fprintf(&b, "Failing at baseline %s, not blamed on this task: %s\n\n", shortSHA(in.base.SHA), codeList(failing))
	}

	b.WriteString("## Risks / Follow-ups\n")
	for _, r := range in.sum.Risks {
		fmt.Fprintf(&b, "- Risk: %s\n", r)
	}
	for _, f := range in.sum.Followups {
		fmt.Fprintf(&b, "- Follow-up: %s\n", f)
	}
	if len(in.sum.Risks)+len(in.sum.Followups) == 0 {
		b.WriteString("None recorded.\n")
	}
	b.WriteString("\n")

	b.WriteString("## Fidelity and limits\n")
	switch in.base.Fidelity {
	case "suite":
		b.WriteString("- " + reducedFidelity + "\n")
	case "none":
		b.WriteString("- " + protocolInactive + "\n")
	}
	b.WriteString("- Coverage and untested areas are not computed in this version.\n")
	b.WriteString("- Flaky re-runs are not applied in this version.\n")
	return b.String()
}

func runLine(r store.Run) string {
	if r.Fidelity == "none" {
		return "no suite configured"
	}
	if r.Status != testrun.StatusCompleted {
		return fmt.Sprintf("run #%d at `%s` %s, see %s", r.ID, shortSHA(r.SHA), r.Status, r.OutputPath)
	}
	return fmt.Sprintf("`%s`: %d pass, %d fail, %d skip (%s, run #%d)", shortSHA(r.SHA), r.Passed, r.Failed, r.Skipped, r.Fidelity, r.ID)
}

func codeList(ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = "`" + id + "`"
	}
	return strings.Join(quoted, ", ")
}
