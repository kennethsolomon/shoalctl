package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/globx"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

// Tools whose file the hooks look at (§4.6), matched by the hook entries too.
var (
	fileTools  = []string{"Read", "Edit", "Write", "MultiEdit", "NotebookEdit"}
	writeTools = []string{"Edit", "Write", "MultiEdit", "NotebookEdit"}
)

// doubleFireWindow: a second SessionStart of the same source within this
// long is the same start seen twice (a hook registered twice), not a new one.
const doubleFireWindow = 5 * time.Second

// hookOutput is the JSON answer Claude Code reads from a hook's stdout.
type hookOutput struct {
	SystemMessage      string          `json:"systemMessage,omitempty"`
	HookSpecificOutput hookSpecificOut `json:"hookSpecificOutput"`
}

type hookSpecificOut struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// hookJSON is the answer that adds text to the agent's context; a non-empty
// userMessage is also shown to the human.
func hookJSON(event, text, userMessage string) (string, error) {
	b, err := json.Marshal(hookOutput{SystemMessage: userMessage, HookSpecificOutput: hookSpecificOut{HookEventName: event, AdditionalContext: text}})
	if err != nil {
		return "", fmt.Errorf("encode %s hook output: %w", event, err)
	}
	return string(b) + "\n", nil
}

// project opens the task's project for a hook: its config (the main
// checkout comes from project.json) and memory.db.
func (env Env) hookProject(ctx context.Context, hc hookCtx) (*project, error) {
	p := &project{id: hc.tf.ProjectID, home: hc.home, dir: hc.projDir}
	meta, ok, err := readMeta(p.metaPath())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s is missing", p.metaPath())
	}
	p.root = meta.Root
	if err := env.loadConfig(p); err != nil {
		return nil, err
	}
	if p.db, err = hc.openDB(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// hookTask reads the task task.json names and checks the slug agrees.
func hookTask(ctx context.Context, p *project, hc hookCtx) (store.Task, error) {
	t, err := p.db.GetTask(ctx, fmt.Sprint(hc.tf.TaskID))
	if err != nil {
		return t, err
	}
	if t.Slug != hc.tf.Slug {
		return t, fmt.Errorf("task.json names %s, but task %d is %s", hc.tf.Slug, hc.tf.TaskID, t.Slug)
	}
	return t, nil
}

// hookSessionStart records the session and injects the brief (§4.6),
// once even when the hook is registered twice.
func (env Env) hookSessionStart(ctx context.Context, hc hookCtx) (string, error) {
	p, err := env.hookProject(ctx, hc)
	if err != nil {
		return "", err
	}
	defer p.close()
	if err := p.db.Heartbeat(ctx, hc.tf.TaskID, hc.tf.Slug, hc.in.SessionID); err != nil {
		return "", err
	}
	source := hc.in.Source
	if source == "" {
		source = "startup"
	}
	first, err := p.db.FireOnce(ctx, hc.in.SessionID, "fire:session-start:"+source, hc.tf.TaskID, doubleFireWindow)
	if err != nil || !first {
		return "", err
	}
	// A compaction or /clear dropped the injected records from the agent's
	// context: let the hooks inject them again.
	if source == "compact" || source == "clear" {
		if err := p.db.ResetInjected(ctx, hc.in.SessionID); err != nil {
			return "", err
		}
	}
	t, err := hookTask(ctx, p, hc)
	if err != nil {
		return "", err
	}
	brief, keys, err := sessionBrief(ctx, p, t, hc.wt)
	if err != nil {
		return "", err
	}
	if _, err := p.db.Mark(ctx, hc.in.SessionID, t.ID, keys); err != nil {
		return "", err
	}
	return hookJSON("SessionStart", brief, "")
}

// toolPath is the repo-relative path of the file a tool call touches, and
// false when the call names no file or one outside the worktree.
func (hc hookCtx) toolPath() (string, bool) {
	p := hc.in.ToolInput.FilePath
	if p == "" {
		p = hc.in.ToolInput.NotebookPath
	}
	if p == "" {
		return "", false
	}
	if !filepath.IsAbs(p) {
		base := hc.in.Cwd
		if base == "" {
			base = hc.wt
		}
		p = filepath.Join(base, p)
	}
	rel, err := relativeTo(hc.wt, p)
	if err != nil {
		return "", false
	}
	return rel, true
}

// internalPath is git's and shoal's own bookkeeping inside a worktree.
func internalPath(rel string) bool {
	return rel == ".git" || strings.HasPrefix(rel, ".git/") || rel == ".shoal" || strings.HasPrefix(rel, ".shoal/")
}

// denied reports whether rel is covered by [privacy].deny_globs: a glob with
// a slash matches the repo path, one without matches any path segment.
func denied(globs []string, rel string) bool {
	for _, g := range globs {
		if globx.Match(g, rel) {
			return true
		}
		if !strings.Contains(g, "/") {
			for _, seg := range strings.Split(rel, "/") {
				if ok, _ := path.Match(g, seg); ok {
					return true
				}
			}
		}
	}
	return false
}

// hookPreToolUse injects the active conventions, lessons, decisions and bugs about the file a
// tool is about to touch, each once per session, within perFileBudget.
func (env Env) hookPreToolUse(ctx context.Context, hc hookCtx) (string, error) {
	if !contains(fileTools, hc.in.ToolName) {
		return "", nil
	}
	rel, ok := hc.toolPath()
	if !ok || internalPath(rel) {
		return "", nil
	}
	p, err := env.hookProject(ctx, hc)
	if err != nil {
		return "", err
	}
	defer p.close()
	if denied(p.cfg.Privacy.DenyGlobs, rel) {
		return "", nil
	}
	all, err := memos(ctx, p.db)
	if err != nil {
		return "", err
	}
	var cands []memo
	var keys []string
	for _, m := range all {
		if m.inject && (m.kind == store.KindConvention || m.kind == store.KindLesson || m.kind == store.KindDecision || m.kind == store.KindBug) && m.about(rel) {
			cands = append(cands, m)
			keys = append(keys, m.ref)
		}
	}
	if len(cands) == 0 {
		return "", nil
	}
	seen, err := p.db.Marked(ctx, hc.in.SessionID, keys)
	if err != nil {
		return "", err
	}
	slugs, err := taskSlugs(ctx, p.db)
	if err != nil {
		return "", err
	}
	header := fmt.Sprintf("shoal: records about %s. %s", rel, verifyNote)
	text := header
	var shown, lines []string
	for _, m := range cands {
		if seen[m.ref] {
			continue
		}
		l := m.line(ctx, hc.wt, slugs)
		if approxTokens(text+"\n"+l) > perFileBudget {
			if len(lines) > 0 {
				break
			}
			l = fitTokens(l, perFileBudget-approxTokens(header)-2)
		}
		text += "\n" + l
		shown = append(shown, m.ref)
		lines = append(lines, l)
	}
	if len(shown) == 0 {
		return "", nil
	}
	added, err := p.db.Mark(ctx, hc.in.SessionID, hc.tf.TaskID, shown)
	if err != nil {
		return "", err
	}
	out := header
	for i, k := range shown {
		if added[k] {
			out += "\n" + lines[i]
		}
	}
	if out == header {
		return "", nil // another registration of this hook injected them
	}
	return hookJSON("PreToolUse", out, "")
}

// hookPostToolUse records an actual claim for a written file and warns, in
// one line each, about a write outside the agent's claims, onto another
// agent's claim, by a non-lead onto contract.md, or onto another open task's
// claim; the last also orders the merges (late overlap, §9.2).
func (env Env) hookPostToolUse(ctx context.Context, hc hookCtx) (string, error) {
	if !contains(writeTools, hc.in.ToolName) {
		return "", nil
	}
	rel, ok := hc.toolPath()
	if !ok || (internalPath(rel) && rel != ".shoal/contract.md") {
		return "", nil
	}
	agent := hc.in.AgentType
	if agent == "" {
		agent = leadAgent // the session itself is the lead (the brief says so)
	}
	p, err := env.hookProject(ctx, hc)
	if err != nil {
		return "", err
	}
	defer p.close()
	t, err := hookTask(ctx, p, hc)
	if err != nil {
		return "", err
	}
	type warning struct{ key, text string }
	var warns []warning
	req := store.WriteRequest{Session: hc.in.SessionID, TaskID: t.ID, Slug: t.Slug, Agent: agent, Path: rel}

	if rel == ".shoal/contract.md" {
		if agent == leadAgent {
			return "", nil
		}
		warns = append(warns, warning{"contract:" + agent, fmt.Sprintf("shoal: %s edited .shoal/contract.md, which only shoal-lead changes; "+
			"tell the lead (shoalctl msg send %s --from %s --to shoal-lead --kind contract-change \"<what and why>\") so it re-freezes the contract",
			agent, t.Slug, agent)})
	} else {
		own, err := p.db.Claims(ctx, t.ID)
		if err != nil {
			return "", err
		}
		var mine []string
		covered, holder, holderGlob := false, "", ""
		for _, c := range own {
			hit := globx.Overlaps(c.Glob, rel)
			switch {
			case c.Agent == agent && c.Kind == "expected":
				mine = append(mine, c.Glob)
				covered = covered || hit
			case c.Agent == store.PlanAgent:
				covered = covered || hit
			case c.Kind == "expected" && hit && holder == "":
				holder, holderGlob = c.Agent, c.Glob
			}
			if c.Agent == agent && c.Kind == "actual" && c.Glob == rel {
				covered = true
			}
		}
		req.ClaimActual = !covered
		switch {
		case holder != "":
			warns = append(warns, warning{"held:" + agent + ":" + rel, fmt.Sprintf("shoal: %s wrote %s, which %s claims in this task (%s); "+
				"hand the change to %s, or have the lead reassign the claim", agent, rel, holder, holderGlob, holder)})
		case len(mine) > 0 && !anyOverlap(mine, rel):
			warns = append(warns, warning{"outside:" + agent + ":" + rel, fmt.Sprintf("shoal: %s wrote %s, outside its claims (%s); "+
				"claim it first: shoalctl claim %s --as %s %s", agent, rel, strings.Join(mine, ", "), t.Slug, agent, rel)})
		}
		open, err := p.db.OpenClaims(ctx)
		if err != nil {
			return "", err
		}
		globs := map[int64]string{}
		for _, c := range open {
			if c.Owner.ID == t.ID || !globx.Overlaps(c.Glob, rel) {
				continue
			}
			if _, dup := globs[c.Owner.ID]; !dup {
				globs[c.Owner.ID] = c.Glob
				req.Behind = append(req.Behind, c.Owner)
			}
		}
		for _, o := range req.Behind {
			warns = append(warns, warning{fmt.Sprintf("overlap:%d:%s", o.ID, rel), ""})
		}
	}
	if !req.ClaimActual && len(warns) == 0 {
		return "", nil
	}
	for _, w := range warns {
		req.Keys = append(req.Keys, w.key)
	}
	res, err := p.db.RecordWrite(ctx, req, leadAgent)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, w := range warns {
		if !res.NewKeys[w.key] {
			continue
		}
		if w.text != "" {
			lines = append(lines, w.text)
		}
	}
	for _, o := range req.Behind {
		if !res.NewKeys[fmt.Sprintf("overlap:%d:%s", o.ID, rel)] {
			continue
		}
		var order string
		switch res.Overlap[o.ID] {
		case store.OverlapQueued:
			order = fmt.Sprintf("%s now merges after %s, and %s's lead was told", t.Slug, o.Slug, o.Slug)
		case store.OverlapAlready:
			order = fmt.Sprintf("%s already merges after %s", t.Slug, o.Slug)
		default:
			order = fmt.Sprintf("%s already waits on %s, so no merge order was added; ask the human how to split the work", o.Slug, t.Slug)
		}
		lines = append(lines, fmt.Sprintf("shoal: %s is also claimed by task %s (late overlap); %s", rel, o.Slug, order))
	}
	if len(lines) == 0 {
		return "", nil
	}
	msg := strings.Join(lines, "\n")
	return hookJSON("PostToolUse", msg, msg)
}

func anyOverlap(globs []string, rel string) bool {
	for _, g := range globs {
		if globx.Overlaps(g, rel) {
			return true
		}
	}
	return false
}

// fitTokens cuts s until approxTokens says it fits in budget.
func fitTokens(s string, budget int) string {
	r := []rune(s)
	for len(r) > 0 && approxTokens(string(r)+"...") > budget {
		r = r[:len(r)*9/10]
	}
	return string(r) + "..."
}
