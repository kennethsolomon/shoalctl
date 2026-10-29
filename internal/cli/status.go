package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/store"
)

type statusRow struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	Lane     string `json:"lane"`
	Stage    string `json:"stage"`
	QARound  int    `json:"qa_round"`
	Base     string `json:"base"`
	Baseline string `json:"baseline"`
	Window   string `json:"window"`
	LastSeen string `json:"last_seen"`
	Worktree string `json:"worktree"`
}

func (env Env) cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parseArgs(fs, args, 0); err != nil {
		return err
	}
	p, err := env.openProject(ctx)
	if err != nil {
		return err
	}
	defer p.close()

	tasks, err := p.db.ListTasks(ctx, time.Now().UTC().Add(-14*24*time.Hour).Format(time.RFC3339))
	if err != nil {
		return err
	}
	alive := env.liveWindows(ctx)
	rows := make([]statusRow, 0, len(tasks))
	for _, t := range tasks {
		row := statusRow{ID: t.ID, Slug: t.Slug, Lane: t.Lane, Stage: t.Stage, QARound: t.QARound, Base: "-", Baseline: "-", Window: "-",
			LastSeen: ago(t.LastSeenAt), Worktree: t.Worktree}
		if t.BaseSHA != "" {
			row.Base = t.BaseRef + "@" + shortSHA(t.BaseSHA)
		}
		if t.BaselineRunID != 0 {
			run, err := p.db.GetRun(ctx, t.BaselineRunID)
			if err != nil {
				return err
			}
			row.Baseline = baselineLabel(run)
		}
		if t.TmuxWindow != "" {
			row.Window = "gone"
			if alive[t.TmuxWindow] {
				row.Window = "alive"
			}
		}
		rows = append(rows, row)
	}

	if *asJSON {
		body, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(env.Stdout, string(body))
	} else {
		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SLUG\tLANE\tSTAGE\tQA\tBASE\tBASELINE\tWINDOW\tLAST SEEN")
		for _, r := range rows {
			qa := "-"
			if r.QARound > 0 {
				qa = fmt.Sprintf("%d/%d", r.QARound, p.cfg.Lanes.QAMaxRounds)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Slug, r.Lane, r.Stage, qa, r.Base, r.Baseline, r.Window, r.LastSeen)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if n, path, err := newHookLogLines(p.home); err != nil {
		fmt.Fprintf(env.Stderr, "warning: %v\n", err)
	} else if n > 0 {
		fmt.Fprintf(env.Stderr, "warning: %d new hook failure(s) since the last status; see %s\n", n, path)
	}
	return nil
}

func baselineLabel(r store.Run) string {
	switch r.Fidelity {
	case "none":
		return "none"
	case "suite":
		if r.Failed > 0 {
			return "(suite) fail"
		}
		return "(suite) pass"
	}
	return fmt.Sprintf("%d/%dF/%dS", r.Passed, r.Failed, r.Skipped)
}

// liveWindows lists every tmux window id. No tmux server means no windows.
func (env Env) liveWindows(ctx context.Context) map[string]bool {
	out, err := env.tmux(ctx, "list-windows", "-a", "-F", "#{window_id}")
	live := map[string]bool{}
	if err != nil {
		return live
	}
	for _, id := range strings.Fields(out) {
		live[id] = true
	}
	return live
}

func ago(ts string) string {
	if ts == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// newHookLogLines counts hooks.log lines added since the previous call and
// remembers the new size in hooks.log.seen.
func newHookLogLines(home string) (int, string, error) {
	path := filepath.Join(home, "logs", "hooks.log")
	seenPath := path + ".seen"
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, path, nil
	}
	if err != nil {
		return 0, path, fmt.Errorf("read %s: %w", path, err)
	}
	seen := 0
	if raw, err := os.ReadFile(seenPath); err == nil {
		seen, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	if seen < 0 || seen > len(data) {
		seen = 0
	}
	n := bytes.Count(data[seen:], []byte("\n"))
	if n > 0 {
		if err := writeFileAtomic(seenPath, []byte(strconv.Itoa(len(data))+"\n"), 0o600); err != nil {
			return n, path, err
		}
	}
	return n, path, nil
}
