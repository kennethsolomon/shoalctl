package cli

import (
	"bytes"
	"errors"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"
)

// slowSuite keeps the baseline running long enough for a second start to
// arrive while the first is still inside it.
const slowSuite = "[tests]\ncommand = \"sleep 2; true\"\nformat = \"exit-code\"\n"

type startResult struct {
	code   int
	stderr string
}

// assertOneStartWins checks that exactly one of two concurrent starts ran the
// task and the other exited 8 naming the holder.
func assertOneStartWins(t *testing.T, h *harness, p *project, results []startResult) {
	t.Helper()
	sort.Slice(results, func(i, j int) bool { return results[i].code < results[j].code })
	if results[0].code != ExitOK || results[1].code != ExitBusy {
		t.Fatalf("exit codes %d and %d, want 0 and %d\nstderr 1: %s\nstderr 2: %s",
			results[0].code, results[1].code, ExitBusy, results[0].stderr, results[1].stderr)
	}
	loser := results[1].stderr
	for _, want := range []string{"already starting or running", "pid "} {
		if !strings.Contains(loser, want) {
			t.Errorf("losing start's stderr lacks %q: %s", want, loser)
		}
	}
	if n := countRows(t, p, `SELECT count(*) FROM test_runs WHERE kind = 'baseline'`); n != 1 {
		t.Errorf("baseline runs = %d, want 1", n)
	}
	waitFor(t, func() bool { return len(claudeLaunches(t, h.argsLog)) >= 1 })
	time.Sleep(300 * time.Millisecond)
	if n := len(claudeLaunches(t, h.argsLog)); n != 1 {
		t.Errorf("claude launched %d times, want 1", n)
	}
	windows := strings.Fields(h.tmuxOut("list-windows", "-a", "-F", "#{window_name}"))
	if n := countOf(windows, "1-race"); n != 1 {
		t.Errorf("tmux windows for the task = %d, want 1: %v", n, windows)
	}
	if got := h.task("1"); got.Stage != "in-progress" {
		t.Errorf("task after the race: %+v", got)
	}
}

func countOf(ss []string, v string) int {
	n := 0
	for _, s := range ss {
		if s == v {
			n++
		}
	}
	return n
}

func TestStartConcurrentSameTaskInProcess(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(slowSuite)
	h.mustRun("new", "feature", "Race")

	ch := make(chan startResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			code, _, errOut := h.run("start", "1")
			ch <- startResult{code, errOut}
		}()
	}
	assertOneStartWins(t, h, p, []startResult{<-ch, <-ch})
}

func TestStartConcurrentSameTaskSubprocesses(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(slowSuite)
	h.mustRun("new", "feature", "Race")

	ch := make(chan startResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			cmd := h.command("start", "1")
			var errb bytes.Buffer
			cmd.Stderr = &errb
			err := cmd.Run()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				code = -1
			}
			ch <- startResult{code, errb.String()}
		}()
	}
	assertOneStartWins(t, h, p, []startResult{<-ch, <-ch})
}
