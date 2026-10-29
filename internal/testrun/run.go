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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
)

const (
	StatusCompleted = "completed"
	StatusError     = "error"
	StatusTimeout   = "timeout"
	StatusCancelled = "cancelled"

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
	// OnStart, when set, receives the suite's pid (its process group id on
	// unix) right after it starts, before the command runs. An error stops
	// the suite without running the command.
	OnStart func(pid int) error
}

// killGrace is how long a suite gets to exit after SIGTERM before SIGKILL.
const killGrace = 3 * time.Second

type Result struct {
	TestID     string
	Status     string // pass | fail | skip
	DurationMS int64
	Message    string
	Attempt    int // set by the caller for flaky re-runs; 0 means the first attempt
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
func CommandHash(command, format, rerunCommand, setup string) string {
	sum := sha256.Sum256([]byte(command + "\x00" + format + "\x00" + rerunCommand + "\x00" + setup))
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
	var roots reportRoots
	if spec.Format == "junit" {
		pattern = junitPattern(spec.Dir, spec.RunDir, spec.JunitPath)
		if roots, err = checkPattern(spec.Dir, spec.RunDir, spec.JunitPath); err != nil {
			return note(err.Error())
		}
		if err := removeStale(ctx, roots, pattern); err != nil {
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
	prepare(cmd)

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

	runErr, startErr := runInGroup(cmd, spec.OnStart)
	// An interrupt or timeout during the gate kills the held suite, so
	// releasing it fails; checked first, it is reported as what it was.
	if ctx.Err() != nil {
		out.Status = StatusCancelled
		out.Problem = "interrupted; the test command's processes were stopped"
		fmt.Fprintf(logf, "shoal: %s\n", out.Problem)
		return out
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		out.Status = StatusTimeout
		out.Problem = fmt.Sprintf("test command ran past the %s timeout and was killed", spec.Timeout)
		fmt.Fprintf(logf, "shoal: %s\n", out.Problem)
		return out
	}
	if startErr != nil {
		return note(fmt.Sprintf("test command could not start: %v", startErr))
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
		files, err := reportMatches(pattern)
		if err != nil {
			return note(err.Error())
		}
		if len(files) == 0 {
			return note(fmt.Sprintf("test command exited %d and wrote no JUnit file matching %s", *out.ExitCode, pattern))
		}
		for _, f := range files {
			if _, err := roots.resolve(f); err != nil {
				return note(err.Error())
			}
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

// runInGroup runs cmd in its own process group (a job object on Windows).
// The suite is held before its first command until onStart returns nil; if
// onStart fails, the suite is stopped without running.
// When cmd's context ends, the whole group gets SIGTERM, then SIGKILL after
// killGrace. Processes the suite leaves behind are killed once it exits, so
// no test process outlives the run.
func runInGroup(cmd *exec.Cmd, onStart func(pid int) error) (runErr, startErr error) {
	var mu sync.Mutex
	var grp *procGroup
	var hardKill *time.Timer
	cmd.Cancel = func() error {
		mu.Lock()
		defer mu.Unlock()
		if grp == nil {
			return cmd.Process.Kill()
		}
		g := grp
		hardKill = time.AfterFunc(killGrace, func() { _ = g.kill() })
		return g.terminate()
	}
	cmd.WaitDelay = killGrace + 2*time.Second
	held, err := hold(cmd)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		held.shut()
		return nil, err
	}
	held.started()
	g, err := attach(cmd)
	if err != nil {
		held.shut()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	mu.Lock()
	grp = g
	mu.Unlock()
	if onStart != nil {
		if err := onStart(cmd.Process.Pid); err != nil {
			return nil, abort(cmd, g, held, err)
		}
	}
	if err := held.open(); err != nil {
		return nil, abort(cmd, g, held, err)
	}
	runErr = cmd.Wait()
	mu.Lock()
	if hardKill != nil {
		hardKill.Stop()
	}
	mu.Unlock()
	_ = g.kill()
	g.close()
	return runErr, nil
}

// abort stops a suite still held at its gate and returns why.
func abort(cmd *exec.Cmd, g *procGroup, held *gate, why error) error {
	held.shut()
	_ = g.kill()
	_ = cmd.Wait()
	g.close()
	return why
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

func junitPattern(dir, runDir, junitPath string) string {
	pattern := strings.ReplaceAll(junitPath, junitPlaceh, filepath.Join(runDir, "junit.xml"))
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(dir, pattern)
	}
	return pattern
}

// reportRoots are the symlink-resolved directories report files may live in:
// the task worktree and shoal's own run directory.
type reportRoots struct{ worktree, runDir string }

// CheckJunitPath refuses a junit_path whose fixed directory part resolves,
// through symlinks, outside the worktree dir and the run dir. shoal deletes
// stale matches before each run, so such a path could delete user files.
func CheckJunitPath(dir, runDir, junitPath string) error {
	_, err := checkPattern(dir, runDir, junitPath)
	return err
}

func checkPattern(dir, runDir, junitPath string) (reportRoots, error) {
	wt, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return reportRoots{}, fmt.Errorf("resolve worktree %s: %w", dir, err)
	}
	rd, err := resolveExisting(runDir)
	if err != nil {
		return reportRoots{}, err
	}
	roots := reportRoots{worktree: wt, runDir: rd}
	pattern := junitPattern(dir, runDir, junitPath)
	if !strings.HasSuffix(pattern, ".xml") {
		return roots, fmt.Errorf("junit_path %q must end in .xml (or be {junit}); shoal deletes stale matches before each run and only ever deletes .xml reports", junitPath)
	}
	for _, part := range strings.FieldsFunc(junitPath, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".git" {
			return roots, fmt.Errorf("junit_path %q reaches into .git; point it at a report directory in the worktree", junitPath)
		}
	}
	base, err := resolveExisting(globBase(pattern))
	if err != nil {
		return roots, err
	}
	if !roots.contains(base) {
		return roots, fmt.Errorf("junit_path %q resolves to %s, outside the worktree %s; shoal deletes stale matches before each run, so it must point inside the worktree (or be {junit})",
			junitPath, base, wt)
	}
	return roots, nil
}

// resolve returns the real path of an existing report file, or an error when
// it resolves outside the roots.
func (r reportRoots) resolve(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve report %s: %w", path, err)
	}
	if !r.contains(real) {
		return "", fmt.Errorf("report %s resolves to %s, outside the worktree %s; shoal will not read or delete it", path, real, r.worktree)
	}
	return real, nil
}

func (r reportRoots) contains(p string) bool { return within(r.worktree, p) || within(r.runDir, p) }

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// globBase is the longest leading directory of pattern with no glob syntax.
func globBase(pattern string) string {
	base := pattern
	for strings.ContainsAny(base, `*?[`) || (runtime.GOOS != "windows" && strings.Contains(base, `\`)) {
		parent := filepath.Dir(base)
		if parent == base {
			break
		}
		base = parent
	}
	return base
}

// resolveExisting resolves symlinks in the longest existing prefix of p and
// appends the rest unchanged.
func resolveExisting(p string) (string, error) {
	p = filepath.Clean(p)
	var rest []string
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(append([]string{real}, rest...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve %s: %w", p, err)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", fmt.Errorf("resolve %s: %w", p, err)
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}

// reportMatches globs pattern and keeps only paths that can be JUnit
// reports: names ending in .xml that are not directories, with no dotfile or
// dot directory below the pattern's fixed directory part.
func reportMatches(pattern string) ([]string, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("bad junit_path pattern %q: %w", pattern, err)
	}
	base := globBase(pattern)
	var keep []string
	for _, m := range matches {
		if !strings.HasSuffix(m, ".xml") || strings.HasPrefix(filepath.Base(m), ".") {
			continue
		}
		rel, err := filepath.Rel(base, m)
		if err != nil {
			return nil, fmt.Errorf("relate %s to %s: %w", m, base, err)
		}
		hidden := false
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			hidden = hidden || (part != "." && strings.HasPrefix(part, "."))
		}
		if hidden {
			continue
		}
		info, err := os.Lstat(m)
		if err != nil {
			return nil, fmt.Errorf("stat report %s: %w", m, err)
		}
		if info.Mode().IsRegular() || info.Mode()&fs.ModeSymlink != 0 {
			keep = append(keep, m)
		}
	}
	return keep, nil
}

// removeStale deletes old report files matching pattern so a previous run is
// never read. It deletes only regular .xml files, and checks every match
// before deleting any: a match that resolves outside the roots, is a
// symlink, or is tracked by git in the worktree refuses the whole run.
func removeStale(ctx context.Context, roots reportRoots, pattern string) error {
	matches, err := reportMatches(pattern)
	if err != nil {
		return err
	}
	for _, m := range matches {
		real, err := roots.resolve(m)
		if err != nil {
			return err
		}
		if info, err := os.Lstat(m); err != nil {
			return fmt.Errorf("stat report %s: %w", m, err)
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("junit_path matches %s, which is not a regular file; shoal will not delete it. Point junit_path at an untracked report path", m)
		}
		if within(roots.worktree, real) {
			parent, err := filepath.EvalSymlinks(filepath.Dir(m))
			if err != nil {
				return fmt.Errorf("resolve %s: %w", filepath.Dir(m), err)
			}
			for _, p := range []string{filepath.Join(parent, filepath.Base(m)), real} {
				rel, err := filepath.Rel(roots.worktree, p)
				if err != nil {
					return fmt.Errorf("relate %s to %s: %w", p, roots.worktree, err)
				}
				if _, err := gitx.Run(ctx, roots.worktree, "ls-files", "--error-unmatch", "--", filepath.ToSlash(rel)); err == nil {
					return fmt.Errorf("junit_path matches %s, which git tracks; shoal will not delete it. Point junit_path at an untracked path", m)
				}
			}
		}
	}
	for _, m := range matches {
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
