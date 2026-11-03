package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/globx"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

// maxTodoBytes bounds the todo.md plan copies into shoal's store.
const maxTodoBytes = 256 << 10

// cmdPlan records a task's expected file claims and resolves overlaps with
// other open tasks, asking the human one keypress per overlapping task
// (§9.1, §9.2). Nothing is written until every live overlap is decided, and
// the claims and the choice land in one transaction (§9.3).
func (env Env) cmdPlan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	todo := fs.String("todo", "", "the task's todo.md; start copies it into the worktree as .shoal/todo.md")
	pos, err := parseArgsRange(fs, args, 2, -1)
	if err != nil {
		return err
	}
	var globs []string
	for _, g := range pos[1:] {
		if err := globx.Validate(g); err != nil {
			return fail(ExitUsage, "%v", err)
		}
		if !contains(globs, g) {
			globs = append(globs, g)
		}
	}
	var todoBody []byte
	if *todo != "" {
		if todoBody, err = readTodoFile(*todo); err != nil {
			return err
		}
	}
	p, t, err := env.openTask(ctx, pos[0])
	if err != nil {
		return err
	}
	defer p.close()
	stale, err := env.staleCheck(ctx, p)
	if err != nil {
		return err
	}
	in := bufio.NewReader(env.Stdin)
	decisions := map[int64]byte{}
	conflicts := 0
	for {
		if !contains([]string{store.StageNew, store.StageReady, store.StageBlocked}, t.Stage) {
			return fail(ExitState, "task %s is at %q; plan runs before start (new, ready or blocked)", t.Slug, t.Stage)
		}
		res, err := p.db.PlanClaims(ctx, store.PlanRequest{Task: t, Globs: globs, Overlaps: globx.Overlaps, Stale: stale,
			Decisions: decisions, Human: "human"})
		if errors.Is(err, store.ErrConflict) {
			if conflicts++; conflicts > 3 {
				return fail(ExitBusy, "task %s kept changing while plan ran (version %d): %v", t.Slug, t.Version, err)
			}
			if t, err = p.db.GetTask(ctx, t.Slug); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if res.Applied {
			return env.reportPlan(ctx, p, t, res, globs, todoBody)
		}
		if !env.interactive() {
			var lines []string
			for _, o := range res.Undecided {
				lines = append(lines, overlapLine(o))
			}
			return fail(ExitState, "%s overlaps open task(s); nothing was planned. A human resolves overlaps at a terminal "+
				"(shoalctl plan %s %s):\n  %s", t.Slug, t.Slug, strings.Join(globs, " "), strings.Join(lines, "\n  "))
		}
		for _, o := range res.Undecided {
			d, err := env.askOverlap(in, p, t, o, todoBody)
			if err != nil {
				return err
			}
			decisions[o.Task.ID] = d
		}
	}
}

func readTodoFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fail(ExitNotFound, "--todo %s: %v", path, err)
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxTodoBytes {
		return nil, fail(ExitUsage, "--todo %s must be a regular file of at most %d bytes", path, maxTodoBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --todo %s: %w", path, err)
	}
	return data, nil
}

func overlapLine(o store.Overlap) string {
	var pairs []string
	for _, pr := range o.Pairs {
		if pr[0] == pr[1] {
			pairs = append(pairs, pr[0])
		} else {
			pairs = append(pairs, pr[0]+" ~ "+pr[1])
		}
	}
	return fmt.Sprintf("%s (%s): %s", o.Task.Slug, o.Task.Stage, strings.Join(pairs, ", "))
}

// askOverlap shows one overlap and reads the human's key (§9.2).
// todo is the todo.md being planned now (nil: none given), shown for t.
func (env Env) askOverlap(in *bufio.Reader, p *project, t store.Task, o store.Overlap, todo []byte) (byte, error) {
	w := env.Stdout
	fmt.Fprintf(w, "overlap: %s and %s (%s, %q) claim the same paths:\n", t.Slug, o.Task.Slug, o.Task.Stage, o.Task.Title)
	for _, pr := range o.Pairs {
		fmt.Fprintf(w, "  %s  overlaps  %s\n", pr[0], pr[1])
	}
	keys := "p?"
	if o.CanSerialize {
		fmt.Fprintf(w, "  [s] serialize: %s waits and starts after %s merges\n", t.Slug, o.Task.Slug)
		keys += "s"
	} else {
		fmt.Fprintf(w, "  [s] not available: %s already waits on %s\n", o.Task.Slug, t.Slug)
	}
	if o.CanFold {
		fmt.Fprintf(w, "  [m] merge: fold %s into %s (same concern); %s closes\n", t.Slug, o.Task.Slug, t.Slug)
		keys += "m"
	} else if o.FoldCycle != "" {
		fmt.Fprintf(w, "  [m] not available: %s waits on %s and %s already waits on it; folding would deadlock them\n",
			o.FoldCycle, t.Slug, o.Task.Slug)
	} else {
		fmt.Fprintf(w, "  [m] not available: %s is %s\n", o.Task.Slug, o.Task.Stage)
	}
	fmt.Fprintf(w, "  [p] parallel anyway: accept the risk; conflicts are resolved at merge time\n")
	fmt.Fprintf(w, "  [?] show the overlapping paths and both todo.md heads\n")
	for {
		fmt.Fprintf(w, "choice [%s]: ", keys)
		line, err := in.ReadString('\n')
		key := strings.TrimSpace(line)
		if key == "" && err != nil {
			if errors.Is(err, io.EOF) {
				return 0, fail(ExitState, "no choice for the overlap with %s; nothing was planned", o.Task.Slug)
			}
			return 0, fmt.Errorf("read the overlap choice: %w", err)
		}
		if len(key) != 1 || !strings.Contains(keys, key) {
			fmt.Fprintf(w, "type one of %s, then Enter\n", strings.Join(strings.Split(keys, ""), ", "))
			continue
		}
		if key == "?" {
			fmt.Fprintf(w, "paths:\n")
			for _, pr := range o.Pairs {
				fmt.Fprintf(w, "  %s (%s)  ~  %s (%s)\n", pr[0], t.Slug, pr[1], o.Task.Slug)
			}
			mine := todoHead(p, t, 10)
			if todo != nil {
				mine = head(string(todo), 10)
			}
			fmt.Fprintf(w, "todo.md of %s:\n%s\n", t.Slug, indent(strings.TrimRight(mine, "\n")))
			fmt.Fprintf(w, "todo.md of %s:\n%s\n", o.Task.Slug, indent(strings.TrimRight(todoHead(p, o.Task, 10), "\n")))
			continue
		}
		return key[0], nil
	}
}

func planTodoPath(p *project, slug string) string { return filepath.Join(p.dir, "plans", slug+".md") }

// todoHead returns the first n lines of the task's todo.md: the worktree's
// copy once started, else the one plan stored.
func todoHead(p *project, t store.Task, n int) string {
	for _, path := range []string{filepath.Join(t.Worktree, ".shoal", "todo.md"), planTodoPath(p, t.Slug)} {
		if t.Worktree == "" && strings.HasSuffix(path, filepath.Join(".shoal", "todo.md")) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		return head(string(data), n)
	}
	return "(no todo.md)"
}

func head(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n")
}

func (env Env) reportPlan(ctx context.Context, p *project, t store.Task, res store.PlanResult, globs []string, todo []byte) error {
	for _, o := range res.Overlaps {
		if o.Stale {
			fmt.Fprintf(env.Stdout, "  stale: %s (last seen %s, window gone): ignored; shoalctl doctor --fix releases its claims\n",
				overlapLine(o), ago(o.Task.LastSeenAt))
		}
	}
	if res.Folded != nil {
		fmt.Fprintf(env.Stdout, "%s folded into %s: its claims, scope items and ask moved there, and %s is closed\n", t.Slug, res.Folded.Slug, t.Slug)
		return nil
	}
	if todo != nil {
		if err := writePrivate(planTodoPath(p, t.Slug), todo); err != nil {
			return err
		}
	}
	if res.Task.Stage == store.StageBlocked {
		var names []string
		for _, id := range res.Blockers {
			b, err := p.db.GetTask(ctx, fmt.Sprint(id))
			if err != nil {
				return err
			}
			names = append(names, b.Slug)
		}
		fmt.Fprintf(env.Stdout, "%s planned (%d glob(s)) and blocked: it starts after %s merge(s)\n", t.Slug, len(globs), strings.Join(names, ", "))
		return nil
	}
	fmt.Fprintf(env.Stdout, "%s planned (%d glob(s)): ready; start it with shoalctl start %s\n", t.Slug, len(globs), t.Slug)
	return nil
}

// staleStages are the stages whose claims can go stale: the task started a
// session that may since have died. A planned task that has not started, or
// a done task waiting to merge, keeps its claims however old they are.
var staleStages = []string{store.StageInProgress, store.StageQA, store.StageNeedsRebase, store.StageEscalated}

// staleCheck returns the rule for a stale claim (§10.4): its task's tmux
// window no longer exists and the task's last sign of life (heartbeat, start,
// or the claim itself) is older than [claims].stale_after. tmux is asked
// once, now; when it cannot answer, nothing counts as stale.
func (env Env) staleCheck(ctx context.Context, p *project) (func(store.Task, store.Claim) bool, error) {
	after, err := time.ParseDuration(p.cfg.Claims.StaleAfter)
	if err != nil {
		return nil, fail(ExitConfig, "[claims].stale_after %q: %v", p.cfg.Claims.StaleAfter, err)
	}
	live, known := env.windowsKnown(ctx)
	now := time.Now()
	return func(owner store.Task, c store.Claim) bool {
		if !known || !contains(staleStages, owner.Stage) {
			return false
		}
		if owner.TmuxWindow != "" && live[owner.TmuxWindow] {
			return false
		}
		last := latestTime(owner.LastSeenAt, owner.StartedAt, c.CreatedAt)
		return !last.IsZero() && now.Sub(last) > after
	}, nil
}

func latestTime(ts ...string) time.Time {
	var out time.Time
	for _, s := range ts {
		if t, err := time.Parse(time.RFC3339, s); err == nil && t.After(out) {
			out = t
		}
	}
	return out
}

// windowsKnown lists every tmux window id and reports whether the list can
// be trusted: no tmux server (or no tmux at all) means no windows; any other
// failure means unknown.
func (env Env) windowsKnown(ctx context.Context) (map[string]bool, bool) {
	out, err := env.tmux(ctx, "list-windows", "-a", "-F", "#{window_id}")
	live := map[string]bool{}
	if err != nil {
		msg := err.Error()
		gone := strings.Contains(msg, "no server running") || strings.Contains(msg, "error connecting") ||
			strings.Contains(msg, "executable file not found") || strings.Contains(msg, "no such file or directory")
		return live, gone
	}
	for _, id := range strings.Fields(out) {
		live[id] = true
	}
	return live, true
}
