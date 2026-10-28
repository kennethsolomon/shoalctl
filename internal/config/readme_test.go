package config

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var tomlBlock = regexp.MustCompile("(?s)```toml\n(.*?)```")

// TestREADMEConfigParses keeps the README's sample config in step with the
// strict loader: every ```toml block must load with no unknown keys and pass
// validation.
func TestREADMEConfigParses(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := tomlBlock.FindAllSubmatch(readme, -1)
	if len(blocks) == 0 {
		t.Fatal("README.md has no ```toml block; this test guards the sample config")
	}
	for i, b := range blocks {
		dir := t.TempDir()
		path := write(t, dir, "config.toml", string(b[1]))
		if _, err := Load(filepath.Join(dir, "missing-global.toml"), path); err != nil {
			t.Errorf("README toml block %d does not load: %v\n%s", i+1, err, b[1])
		}
	}
}
