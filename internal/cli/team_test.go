package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

func TestMsgSendAndInbox(t *testing.T) {
	h := newHarness(t)
	h.initWith(junitConfig)
	slug := strings.TrimSpace(h.mustRun("new", "feature", "Talk"))

	h.mustRun("msg", "send", slug, "--from", "shoal-backend", "--to", "shoal-lead", "--kind", "question", "Is id a string? see api/x.go:12")
	h.mustRun("msg", "send", slug, "--from", "shoal-lead", "--to", "*", "--kind", "info", "--subject", "heads up", "contract v1 frozen")
	h.mustRun("msg", "send", slug, "--from", "shoal-lead", "--to", "shoal-backend", "--kind", "answer", "--reply-to", "1", "yes, a string")

	in := h.mustRun("msg", "inbox", slug, "--as", "shoal-lead")
	if !strings.Contains(in, "#1 ") || !strings.Contains(in, "Is id a string?") || strings.Contains(in, "heads up") {
		t.Fatalf("lead inbox:\n%s", in)
	}
	if again := h.mustRun("msg", "inbox", slug, "--as", "shoal-lead"); !strings.Contains(again, "no unread messages") {
		t.Fatalf("read twice:\n%s", again)
	}
	be := h.mustRun("msg", "inbox", slug, "--as", "shoal-backend")
	if !strings.Contains(be, "heads up") || !strings.Contains(be, "(reply to #1)") {
		t.Fatalf("backend inbox:\n%s", be)
	}
	if fe := h.mustRun("msg", "inbox", slug, "--as", "shoal-frontend"); !strings.Contains(fe, "heads up") || strings.Contains(fe, "a string") {
		t.Fatalf("frontend inbox:\n%s", fe)
	}
	if all := h.mustRun("msg", "inbox", slug, "--as", "shoal-lead", "--all"); !strings.Contains(all, "[read]") {
		t.Fatalf("--all:\n%s", all)
	}

	h.expectCode(ExitUsage, "msg", "send", slug, "--from", "shoal-ceo", "--to", "*", "--kind", "info", "x")
	h.expectCode(ExitUsage, "msg", "send", slug, "--from", "human", "--to", "*", "--kind", "gossip", "x")
	h.expectCode(ExitUsage, "msg", "send", slug, "--from", "human", "--to", "*", "--kind", "answer", "x")
	h.expectCode(ExitUsage, "msg", "send", slug, "--from", "human", "--to", "*", "--kind", "info", strings.Repeat("x", 4001))
	h.expectCode(ExitNotFound, "msg", "send", slug, "--from", "human", "--to", "*", "--kind", "answer", "--reply-to", "99", "x")
	h.expectCode(ExitNotFound, "msg", "inbox", "42-nope", "--as", "human")
}

func TestClaimAndRelease(t *testing.T) {
	h := newHarness(t)
	h.initWith(junitConfig)
	slug := strings.TrimSpace(h.mustRun("new", "feature", "Split"))
	h.mustRun("claim", slug, "--as", "shoal-backend", "api/**", "db/schema.sql")
	h.mustRun("claim", slug, "--as", "shoal-frontend", "ui/**")
	if errOut := h.expectCode(ExitState, "claim", slug, "--as", "shoal-frontend", "api/users.go"); !strings.Contains(errOut, "shoal-backend") {
		t.Fatalf("conflict message: %s", errOut)
	}
	for _, bad := range []string{"../etc/passwd", "/abs", "a/../../b", "~/x"} {
		h.expectCode(ExitUsage, "claim", slug, "--as", "shoal-frontend", bad)
	}
	h.expectCode(ExitUsage, "claim", slug, "--as", "shoal-frontend")
	h.mustRun("release", slug, "--as", "shoal-backend", "api/**")
	h.mustRun("claim", slug, "--as", "shoal-frontend", "api/users.go")
	if out := h.mustRun("release", slug, "--as", "shoal-frontend"); !strings.Contains(out, "released 2 claim(s)") {
		t.Fatalf("release all: %s", out)
	}
}

func TestContractFreeze(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "API")
	h.expectCode(ExitNotFound, "contract", "freeze", task.Slug)
	path := filepath.Join(task.Worktree, ".shoal", "contract.md")
	testutil.WriteFile(t, path, "## Endpoints\nPOST /clients\n")
	if out := h.mustRun("contract", "freeze", task.Slug); !strings.Contains(out, "version 1") {
		t.Fatalf("v1: %s", out)
	}
	if out := h.mustRun("contract", "freeze", task.Slug); !strings.Contains(out, "unchanged") {
		t.Fatalf("same: %s", out)
	}
	testutil.WriteFile(t, path, "## Endpoints\nPOST /clients\nGET /clients\n")
	h.expectCode(ExitState, "contract", "freeze", task.Slug)
	h.expectCode(ExitUsage, "contract", "freeze", task.Slug, "--as", "shoal-backend", "--note", "x")
	h.mustRun("contract", "freeze", task.Slug, "--note", "added GET /clients")
	if in := h.mustRun("msg", "inbox", task.Slug, "--as", "shoal-frontend"); !strings.Contains(in, "contract-change") || !strings.Contains(in, "added GET /clients") {
		t.Fatalf("broadcast: %s", in)
	}
}
