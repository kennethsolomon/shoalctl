package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// claudeConfigPath is the file where Claude Code records workspace trust:
// $CLAUDE_CONFIG_DIR/.claude.json when that is set, otherwise ~/.claude.json.
func claudeConfigPath() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fail(ExitUntrusted, "find home directory for ~/.claude.json: %v", err)
	}
	return filepath.Join(home, ".claude.json"), nil
}

// checkTrust fails unless Claude Code has accepted its workspace trust dialog
// for the main checkout. Task worktrees inherit that trust; without it the
// dialog would block the session, defaulting to "No, exit". shoal only reads
// the file: it never records trust and never answers the dialog.
func checkTrust(root string) error {
	path, err := claudeConfigPath()
	if err != nil {
		return err
	}
	hint := "run `claude` once in " + root + " and choose \"Yes, I trust this folder\", then retry"
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fail(ExitUntrusted, "Claude Code has not trusted %s (%s does not exist); %s", root, path, hint)
	}
	if err != nil {
		return fail(ExitUntrusted, "read %s to check workspace trust: %v", path, err)
	}
	var cfg struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fail(ExitUntrusted, "cannot check workspace trust: %s is not valid JSON (%v); %s", path, err, hint)
	}
	keys := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		keys = append(keys, resolved)
	}
	for _, k := range keys {
		if cfg.Projects[k].HasTrustDialogAccepted {
			return nil
		}
	}
	return fail(ExitUntrusted, "Claude Code has not trusted %s; %s", root, hint)
}
