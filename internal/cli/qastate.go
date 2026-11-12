package cli

import (
	"context"
	"fmt"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

// QA states (§16 row 88): what the last QA result says about the code now.
const (
	qaNone  = "none"  // never ran
	qaPass  = "pass"  // passed on the code the worktree holds now
	qaFail  = "fail"  // failed
	qaStale = "stale" // passed, but the code changed since
)

// unreadableTree stands for worktree code status could not read: it matches
// no recorded tree, so a pass on it shows as stale.
const unreadableTree = "(unreadable)"

// qaCheckedStages are the stages whose worktree status compares with the
// code QA ran on. Merging tasks are mid-rebase and closed ones may have no
// worktree: their results show as recorded.
var qaCheckedStages = []string{store.StageInProgress, store.StageQA, store.StageNeedsRebase, store.StageEscalated, store.StageDone}

// qaRoundStages are the stages from which shoalctl qa runs a round.
var qaRoundStages = []string{store.StageInProgress, store.StageQA, store.StageNeedsRebase}

// qaState is a task's last human QA round and newest agent QA check.
type qaState struct {
	Human    string // the last shoalctl qa round
	Round    int
	SHA      string // the commit that round ran at
	Agent    string // the newest shoalctl test --kind qa check after that round
	AgentSHA string
}

// qaStateOf compares the code t's QA results ran on with tree, the code in
// its worktree now. An empty tree skips the comparison.
func qaStateOf(ctx context.Context, p *project, t store.Task, tree string) (qaState, error) {
	s := qaState{Human: qaNone, Agent: qaNone, Round: t.QARound}
	rounds, err := p.db.QARounds(ctx, t.ID)
	if err != nil {
		return s, err
	}
	var lastRun int64
	if n := len(rounds); n > 0 {
		r := rounds[n-1]
		s.Human, s.SHA, lastRun = qaVerdict(r.Passed, r.Tree, tree), r.HeadSHA, r.RunID
	}
	a, ok, err := p.db.LatestAgentQA(ctx, t.ID)
	if err != nil {
		return s, err
	}
	if ok && a.RunID > lastRun {
		s.Agent, s.AgentSHA = qaVerdict(a.Passed, a.Tree, tree), a.SHA
	}
	return s, nil
}

func qaVerdict(passed bool, ranOn, now string) string {
	switch {
	case !passed:
		return qaFail
	case now != "" && ranOn != now:
		return qaStale
	}
	return qaPass
}

// currentQAState is t's QA state against the code in its worktree now.
func currentQAState(ctx context.Context, p *project, t store.Task) (qaState, error) {
	tree, err := gitx.CodeTree(ctx, t.Worktree)
	if err != nil {
		return qaState{}, err
	}
	return qaStateOf(ctx, p, t, tree)
}

// label is the status QA column: "pass 1/3", "agent pass", both, or "-".
func (s qaState) label(maxRounds int) string {
	human := "-"
	if s.Human != qaNone {
		human = fmt.Sprintf("%s %d/%d", s.Human, s.Round, maxRounds)
	}
	if s.Agent == qaNone {
		return human
	}
	if human == "-" {
		return "agent " + s.Agent
	}
	return human + ", agent " + s.Agent
}

// warning is the stderr line done and merge print when no human QA round
// passed on this code; "" when one did. QA stays optional: they go on.
func (s qaState) warning(slug, next string) string {
	var why string
	switch s.Human {
	case qaPass:
		return ""
	case qaNone:
		why = "no QA round ran on this code"
	case qaStale:
		why = fmt.Sprintf("the code changed since QA round %d passed", s.Round)
	default:
		why = fmt.Sprintf("QA round %d failed and no round passed since", s.Round)
	}
	return fmt.Sprintf("warning: %s: %s (QA is optional); %s", slug, why, next)
}
