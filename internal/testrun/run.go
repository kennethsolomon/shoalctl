// Package testrun runs a project's configured test suite and reads per-test
// results from JUnit XML or go test -json (spec §8.1).
package testrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
)

const (
	StatusCompleted = "completed"
	StatusError     = "error"
	StatusTimeout   = "timeout"

	SuiteID     = "(suite)"
	maxMessage  = 1000
	junitPlaceh = "{junit}"
)

type Spec struct {
	Command   string
	Format    string // junit | go-json | exit-code | none
	JunitPath string
	Timeout   time.Duration
	Dir       string // worktree the command runs in
	RunDir    string // absolute dir for output.log and junit.xml
}

type Result struct {
	TestID     string
	Status     string // pass | fail | skip
	DurationMS int64
	Message    string
}

type Outcome struct {
	Status     string
	ExitCode   *int
	Results    []Result
	OutputPath string
	Problem    string // why the run is error or timeout
}

// Fidelity is per-test, suite, or none for a format.
func Fidelity(format string) string {
	switch format {
	case "exit-code":
		return "suite"
	case "none":
		return "none"
	}
	return "per-test"
}

// CommandHash identifies a suite configuration for baseline reuse (§8.2).
func CommandHash(command, format, rerunCommand string) string {
	sum := sha256.Sum256([]byte(command + "\x00" + format + "\x00" + rerunCommand))
	return hex.EncodeToString(sum[:])
}

// Run executes the suite and classifies the outcome. It never returns a Go
// error: every failure is an Outcome with status error and a Problem.
func Run(ctx context.Context, spec Spec) Outcome {
	out := Outcome{OutputPath: filepath.Join(spec.RunDir, "output.log")}
	if err := os.MkdirAll(spec.RunDir, 0o700); err != nil {
		return out.fail(fmt.Sprintf("create run dir %s: %v", spec.RunDir, err))
	}
	logf, err := os.OpenFile(out.OutputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return out.fail(fmt.Sprintf("create %s: %v", out.OutputPath, err))
	}
	defer logf.Close()
	note := func(msg string) Outcome {
		fmt.Fprintf(logf, "shoal: %s\n", msg)
		return out.fail(msg)
	}

	if spec.Format == "none" {
		out.Status = StatusCompleted
		return out
	}
	junitFile := filepath.Join(spec.RunDir, "junit.xml")
	pattern := ""
	if spec.Format == "junit" {
		pattern = strings.ReplaceAll(spec.JunitPath, junitPlaceh, junitFile)
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(spec.Dir, pattern)
		}
		if err := removeStale(ctx, spec.Dir, pattern); err != nil {
			return note(err.Error())
		}
	}

	sh, err := exec.LookPath("sh")
	if err != nil {
		return note("sh not found on PATH: " + err.Error())
	}
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	command := strings.ReplaceAll(spec.Command, junitPlaceh, shellQuote(junitFile))
	cmd := exec.CommandContext(runCtx, sh, "-c", command)
	cmd.Dir = spec.Dir
	cmd.Stderr = logf
	cmd.WaitDelay = 5 * time.Second
	setProcessGroup(cmd)

	var stdout *os.File
	if spec.Format == "go-json" {
		if stdout, err = os.CreateTemp(spec.RunDir, "stdout-*.json"); err != nil {
			return note("create stdout capture: " + err.Error())
		}
		defer func() {
			stdout.Close()
			os.Remove(stdout.Name())
		}()
		cmd.Stdout = io.MultiWriter(logf, stdout)
	} else {
		cmd.Stdout = logf
	}

	runErr := cmd.Run()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		out.Status = StatusTimeout
		out.Problem = fmt.Sprintf("test command ran past the %s timeout and was killed", spec.Timeout)
		fmt.Fprintf(logf, "shoal: %s\n", out.Problem)
		return out
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		code := 0
		out.ExitCode = &code
	case errors.As(runErr, &exitErr) && exitErr.ExitCode() >= 0:
		code := exitErr.ExitCode()
		out.ExitCode = &code
	default:
		return note(fmt.Sprintf("test command could not run: %v", runErr))
	}
	failedExit := *out.ExitCode != 0

	switch spec.Format {
	case "exit-code":
		status := "pass"
		if failedExit {
			status = "fail"
		}
		out.Results = []Result{{TestID: SuiteID, Status: status}}
	case "junit":
		files, err := filepath.Glob(pattern)
		if err != nil {
			return note(fmt.Sprintf("bad junit_path %q: %v", spec.JunitPath, err))
		}
		if len(files) == 0 {
			return note(fmt.Sprintf("test command exited %d and wrote no JUnit file matching %s", *out.ExitCode, pattern))
		}
		sort.Strings(files)
		if out.Results, err = ParseJUnitFiles(files); err != nil {
			return note(err.Error())
		}
	case "go-json":
		if _, err := stdout.Seek(0, io.SeekStart); err != nil {
			return note("rewind stdout capture: " + err.Error())
		}
		results, events, err := ParseGoJSON(stdout)
		if err != nil {
			return note(err.Error())
		}
		if events == 0 {
			return note(fmt.Sprintf("test command exited %d and printed no go test -json events", *out.ExitCode))
		}
		out.Results = results
	default:
		return note(fmt.Sprintf("unknown test format %q", spec.Format))
	}

	if failedExit && spec.Format != "exit-code" && !anyFailed(out.Results) {
		out.Results = append(out.Results, Result{TestID: SuiteID, Status: "fail",
			Message: fmt.Sprintf("test command exited %d although every parsed test passed", *out.ExitCode)})
	}
	out.Status = StatusCompleted
	return out
}

func (o Outcome) fail(problem string) Outcome {
	o.Status = StatusError
	o.Problem = problem
	return o
}

func anyFailed(rs []Result) bool {
	for _, r := range rs {
		if r.Status == "fail" {
			return true
		}
	}
	return false
}

// removeStale deletes old report files matching pattern so a previous run is
// never read. It refuses to delete files git tracks in the worktree.
func removeStale(ctx context.Context, dir, pattern string) error {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("bad junit_path pattern %q: %w", pattern, err)
	}
	for _, m := range matches {
		if rel, err := filepath.Rel(dir, m); err == nil && !strings.HasPrefix(rel, "..") {
			if _, err := gitx.Run(ctx, dir, "ls-files", "--error-unmatch", "--", rel); err == nil {
				return fmt.Errorf("junit_path matches %s, which git tracks; shoal will not delete it. Point junit_path at an untracked path", m)
			}
		}
		if err := os.Remove(m); err != nil {
			return fmt.Errorf("remove stale report %s: %w", m, err)
		}
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > maxMessage {
		return string(r[:maxMessage])
	}
	return s
}
