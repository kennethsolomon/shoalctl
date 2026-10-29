package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kennethsolomon/shoalctl/internal/globx"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

// Roles of the agent team (§7.2), in brief order.
var roles = []string{"lead", "backend", "frontend", "integration", "docs", "qa-breaker", "qa-regression", "qa-scope"}

const leadAgent = "shoal-lead"

var messageKinds = []string{"question", "answer", "handoff", "contract-change", "blocker", "qa-failure", "info"}

func validAgent(name string) error {
	if name == "human" {
		return nil
	}
	if role, ok := strings.CutPrefix(name, "shoal-"); ok && contains(roles, role) {
		return nil
	}
	return fail(ExitUsage, "agent %q must be human or one of shoal-%s", name, strings.Join(roles, ", shoal-"))
}

// closedStages end a task's working life: no claims or messages after them.
var closedStages = []string{store.StageMerged}

func (env Env) cmdMsg(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fail(ExitUsage, "usage: msg send|inbox <task> ...")
	}
	switch args[0] {
	case "send":
		return env.msgSend(ctx, args[1:])
	case "inbox":
		return env.msgInbox(ctx, args[1:])
	}
	return fail(ExitUsage, "unknown msg command %q: use send or inbox", args[0])
}

func (env Env) msgSend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("msg send", flag.ContinueOnError)
	from := fs.String("from", "", "sending agent")
	to := fs.String("to", "", "receiving agent, or * for everyone in the task")
	kind := fs.String("kind", "", strings.Join(messageKinds, ", "))
	subject := fs.String("subject", "", "one line, at most 120 characters (default: the body's first line)")
	replyTo := fs.Int64("reply-to", 0, "id of the message this answers")
	pos, err := parseArgs(fs, args, 2)
	if err != nil {
		return err
	}
	if err := validAgent(*from); err != nil {
		return err
	}
	if *to != "*" {
		if err := validAgent(*to); err != nil {
			return err
		}
	}
	if !contains(messageKinds, *kind) {
		return fail(ExitUsage, "--kind %q must be one of %s", *kind, strings.Join(messageKinds, ", "))
	}
	if *kind == "answer" && *replyTo == 0 {
		return fail(ExitUsage, "an answer needs --reply-to <message id>")
	}
	body := strings.TrimSpace(pos[1])
	if body == "" || utf8.RuneCountInString(body) > 4000 {
		return fail(ExitUsage, "the body must be 1-4000 characters, got %d; point at files and lines instead of pasting them", utf8.RuneCountInString(body))
	}
	subj := strings.TrimSpace(*subject)
	if subj == "" {
		subj, _, _ = strings.Cut(body, "\n")
		if r := []rune(subj); len(r) > 120 {
			subj = string(r[:117]) + "..."
		}
	}
	if utf8.RuneCountInString(subj) > 120 || strings.ContainsAny(subj, "\r\n") {
		return fail(ExitUsage, "--subject must be one line of at most 120 characters")
	}
	p, t, err := env.openTask(ctx, pos[0])
	if err != nil {
		return err
	}
	defer p.close()
	if contains(closedStages, t.Stage) {
		return fail(ExitState, "task %s is %s; its messages are closed", t.Slug, t.Stage)
	}
	id, err := p.db.SendMessage(ctx, store.Message{TaskID: t.ID, From: *from, To: *to, Kind: *kind, Subject: subj, Body: body, ReplyTo: *replyTo})
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "message %d sent to %s\n", id, *to)
	return nil
}

func (env Env) msgInbox(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("msg inbox", flag.ContinueOnError)
	as := fs.String("as", "", "the agent reading its inbox")
	all := fs.Bool("all", false, "also show messages already read")
	pos, err := parseArgs(fs, args, 1)
	if err != nil {
		return err
	}
	if err := validAgent(*as); err != nil {
		return err
	}
	p, t, err := env.openTask(ctx, pos[0])
	if err != nil {
		return err
	}
	defer p.close()
	msgs, err := p.db.Inbox(ctx, t.ID, *as, *all)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		fmt.Fprintf(env.Stdout, "no unread messages for %s\n", *as)
		return nil
	}
	for _, m := range msgs {
		reply := ""
		if m.ReplyTo != 0 {
			reply = fmt.Sprintf(" (reply to #%d)", m.ReplyTo)
		}
		read := ""
		if m.Read {
			read = " [read]"
		}
		fmt.Fprintf(env.Stdout, "#%d %s %s -> %s [%s]%s%s\n  %s\n%s\n\n", m.ID, m.CreatedAt, m.From, m.To, m.Kind, reply, read, m.Subject, indent(m.Body))
	}
	return nil
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

func contractPath(t store.Task) string { return filepath.Join(t.Worktree, ".shoal", "contract.md") }

// contractHash returns the SHA-256 of the task's contract.md, and false when
// the file does not exist.
func contractHash(t store.Task) (string, bool, error) {
	if t.Worktree == "" {
		return "", false, nil
	}
	data, err := os.ReadFile(contractPath(t))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", contractPath(t), err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), true, nil
}

// contractProblem says why the contract blocks qa and done: contract.md
// differs from its latest frozen version, or exists and was never frozen.
// It returns "" when there is no contract or it matches.
func contractProblem(ctx context.Context, p *project, t store.Task) (string, error) {
	sum, exists, err := contractHash(t)
	if err != nil {
		return "", err
	}
	latest, frozen, err := p.db.LatestContract(ctx, t.ID)
	if err != nil {
		return "", err
	}
	switch {
	case !exists && !frozen:
		return "", nil
	case !exists:
		return fmt.Sprintf("contract.md (frozen as version %d) is missing from %s", latest.Version, contractPath(t)), nil
	case !frozen:
		return fmt.Sprintf("contract.md exists but was never frozen; run shoalctl contract freeze %s", t.Slug), nil
	case latest.SHA256 != sum:
		return fmt.Sprintf("contract.md changed since frozen version %d; the lead runs shoalctl contract freeze %s --note \"<what changed>\"", latest.Version, t.Slug), nil
	}
	return "", nil
}

func (env Env) cmdContract(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "freeze" {
		return fail(ExitUsage, "usage: contract freeze <task> [--as <agent>] [--note \"<what changed>\"]")
	}
	fs := flag.NewFlagSet("contract freeze", flag.ContinueOnError)
	as := fs.String("as", leadAgent, "who freezes it: shoal-lead or human")
	note := fs.String("note", "", "what changed; required for every version after the first")
	pos, err := parseArgs(fs, args[1:], 1)
	if err != nil {
		return err
	}
	if *as != leadAgent && *as != "human" {
		return fail(ExitUsage, "only shoal-lead (or a human) freezes the contract, not %q", *as)
	}
	if utf8.RuneCountInString(*note) > 4000 {
		return fail(ExitUsage, "--note must be at most 4000 characters")
	}
	p, t, err := env.openTask(ctx, pos[0])
	if err != nil {
		return err
	}
	defer p.close()
	if !contains(workingStages, t.Stage) {
		return fail(ExitState, "task %s is at %q; contract freeze needs a started task (%s)", t.Slug, t.Stage, strings.Join(workingStages, ", "))
	}
	sum, exists, err := contractHash(t)
	if err != nil {
		return err
	}
	if !exists {
		return fail(ExitNotFound, "%s does not exist; shoal-lead writes it first (sections: Endpoints, Events, Types, Errors, Open questions)", contractPath(t))
	}
	c, changed, err := p.db.FreezeContract(ctx, t.ID, sum, *as, strings.TrimSpace(*note))
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintf(env.Stdout, "contract.md unchanged: still version %d\n", c.Version)
		return nil
	}
	fmt.Fprintf(env.Stdout, "contract.md frozen as version %d (sha256 %s)\n", c.Version, c.SHA256[:12])
	if c.Version > 1 {
		fmt.Fprintln(env.Stdout, "broadcast a contract-change message to every agent of the task")
	}
	return nil
}

func (env Env) claimArgs(ctx context.Context, name string, args []string, minGlobs int) (*project, store.Task, string, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	as := fs.String("as", "", "the agent")
	pos, err := parseArgsRange(fs, args, 1+minGlobs, -1)
	if err != nil {
		return nil, store.Task{}, "", nil, err
	}
	if err := validAgent(*as); err != nil {
		return nil, store.Task{}, "", nil, err
	}
	globs := pos[1:]
	for _, g := range globs {
		if err := globx.Validate(g); err != nil {
			return nil, store.Task{}, "", nil, fail(ExitUsage, "%v", err)
		}
	}
	p, t, err := env.openTask(ctx, pos[0])
	if err != nil {
		return nil, store.Task{}, "", nil, err
	}
	if contains(closedStages, t.Stage) || t.Stage == store.StageDone {
		p.close()
		return nil, store.Task{}, "", nil, fail(ExitState, "task %s is %s; its claims are closed", t.Slug, t.Stage)
	}
	return p, t, *as, globs, nil
}

func (env Env) cmdClaim(ctx context.Context, args []string) error {
	p, t, as, globs, err := env.claimArgs(ctx, "claim", args, 1)
	if err != nil {
		return err
	}
	defer p.close()
	n, err := p.db.AddClaims(ctx, t.ID, as, globs, globx.Overlaps)
	if errors.Is(err, store.ErrClaimConflict) {
		return fail(ExitState, "%v; the lead reassigns it, or the holder releases it first", err)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s claimed %d new glob(s) in %s\n", as, n, t.Slug)
	return nil
}

func (env Env) cmdRelease(ctx context.Context, args []string) error {
	p, t, as, globs, err := env.claimArgs(ctx, "release", args, 0)
	if err != nil {
		return err
	}
	defer p.close()
	n, err := p.db.ReleaseClaims(ctx, t.ID, as, globs)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s released %s claim(s) in %s\n", as, strconv.Itoa(n), t.Slug)
	return nil
}
