package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// Byte-exact goldens: init must change only the hook entries it owns, keeping
// the user's key order, escapes, unicode, indentation, line endings and final
// newline (or lack of one).
var settingsGoldens = []struct {
	name, in, want string
}{
	{
		name: "empty file",
		in:   "",
		want: "{\n  \"hooks\": {\n    \"SessionStart\": [\n      {\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n            \"command\": \"/bin/shoalctl hook session-start || true\",\n            \"timeout\": 10\n          }\n        ]\n      }\n    ],\n    \"Stop\": [\n      {\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n            \"command\": \"/bin/shoalctl hook stop || true\",\n            \"timeout\": 10\n          }\n        ]\n      }\n    ]\n  }\n}\n",
	},
	{
		name: "minified, unusual key order, & < unicode and escapes, no trailing newline",
		in:   `{"zzz":1,"permissions":{"allow":["Bash(npm test && echo <ok>)"]},"aaa":"héllo ✓ & <","model":"x"}`,
		want: `{"zzz":1,"permissions":{"allow":["Bash(npm test && echo <ok>)"]},"aaa":"héllo ✓ & <","model":"x","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/bin/shoalctl hook session-start || true","timeout":10}]}],"Stop":[{"hooks":[{"type":"command","command":"/bin/shoalctl hook stop || true","timeout":10}]}]}}`,
	},
	{
		name: "pretty, existing non-shoal Stop hook kept in place",
		in: `{
  "permissions": {
    "allow": ["Bash(npm test && echo <ok>)"]
  },
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [
          { "type": "command", "command": "echo user-stop-hook" }
        ]
      }
    ]
  },
  "zzz": 1,
  "aaa": 2
}
`,
		want: `{
  "permissions": {
    "allow": ["Bash(npm test && echo <ok>)"]
  },
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [
          { "type": "command", "command": "echo user-stop-hook" }
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "/bin/shoalctl hook stop || true",
            "timeout": 10
          }
        ]
      }
    ],
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/bin/shoalctl hook session-start || true",
            "timeout": 10
          }
        ]
      }
    ]
  },
  "zzz": 1,
  "aaa": 2
}
`,
	},
	{
		name: "CRLF, four spaces, stale shoal path next to a user entry",
		in:   "{\r\n    \"hooks\": {\r\n        \"SessionStart\": [\r\n            {\r\n                \"hooks\": [\r\n                    {\"type\": \"command\", \"command\": \"say hi\"},\r\n                    {\"type\": \"command\", \"command\": \"/old/place/shoalctl hook session-start\", \"timeout\": 10}\r\n                ]\r\n            }\r\n        ],\r\n        \"Stop\": [\r\n            {\"hooks\": [{\"type\": \"command\", \"command\": \"/old/place/shoalctl hook stop\", \"timeout\": 10}]}\r\n        ]\r\n    },\r\n    \"env\": {\"PATH_NOTE\": \"a<b>&c\"}\r\n}\r\n",
		want: "{\r\n    \"hooks\": {\r\n        \"SessionStart\": [\r\n            {\r\n                \"hooks\": [\r\n                    {\"type\": \"command\", \"command\": \"say hi\"}\r\n                ]\r\n            },\r\n            {\r\n                \"hooks\": [\r\n                    {\r\n                        \"type\": \"command\",\r\n                        \"command\": \"/bin/shoalctl hook session-start || true\",\r\n                        \"timeout\": 10\r\n                    }\r\n                ]\r\n            }\r\n        ],\r\n        \"Stop\": [\r\n            {\r\n                \"hooks\": [\r\n                    {\r\n                        \"type\": \"command\",\r\n                        \"command\": \"/bin/shoalctl hook stop || true\",\r\n                        \"timeout\": 10\r\n                    }\r\n                ]\r\n            }\r\n        ]\r\n    },\r\n    \"env\": {\"PATH_NOTE\": \"a<b>&c\"}\r\n}\r\n",
	},
	{
		name: "tabs, empty hooks object, no trailing newline",
		in:   "{\n\t\"hooks\": {}\n}",
		want: "{\n\t\"hooks\": {\n\t\t\"SessionStart\": [\n\t\t\t{\n\t\t\t\t\"hooks\": [\n\t\t\t\t\t{\n\t\t\t\t\t\t\"type\": \"command\",\n\t\t\t\t\t\t\"command\": \"/bin/shoalctl hook session-start || true\",\n\t\t\t\t\t\t\"timeout\": 10\n\t\t\t\t\t}\n\t\t\t\t]\n\t\t\t}\n\t\t],\n\t\t\"Stop\": [\n\t\t\t{\n\t\t\t\t\"hooks\": [\n\t\t\t\t\t{\n\t\t\t\t\t\t\"type\": \"command\",\n\t\t\t\t\t\t\"command\": \"/bin/shoalctl hook stop || true\",\n\t\t\t\t\t\t\"timeout\": 10\n\t\t\t\t\t}\n\t\t\t\t]\n\t\t\t}\n\t\t]\n\t}\n}",
	},
	{
		name: "duplicate shoal entries collapse, other event untouched",
		in:   "{\n  \"hooks\": {\n    \"PreToolUse\": [{\"matcher\": \"Edit\", \"hooks\": [{\"type\": \"command\", \"command\": \"lint <file> && ok\"}]}],\n    \"SessionStart\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"/bin/shoalctl hook session-start || true\", \"timeout\": 10}]}],\n    \"Stop\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"/bin/shoalctl hook stop || true\", \"timeout\": 10}]}, {\"hooks\": [{\"type\": \"command\", \"command\": \"/bin/shoalctl hook stop || true\", \"timeout\": 10}]}]\n  }\n}\n",
		want: "{\n  \"hooks\": {\n    \"PreToolUse\": [{\"matcher\": \"Edit\", \"hooks\": [{\"type\": \"command\", \"command\": \"lint <file> && ok\"}]}],\n    \"SessionStart\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"/bin/shoalctl hook session-start || true\", \"timeout\": 10}]}],\n    \"Stop\": [\n      {\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n            \"command\": \"/bin/shoalctl hook stop || true\",\n            \"timeout\": 10\n          }\n        ]\n      }\n    ]\n  }\n}\n",
	},
}

func TestMergeHooksGolden(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("goldens use the POSIX entry; the Windows entry has its own tests")
	}
	for _, tt := range settingsGoldens {
		t.Run(tt.name, func(t *testing.T) {
			out, changed, err := mergeHooks([]byte(tt.in), "/bin/shoalctl")
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if string(out) != tt.want {
				t.Fatalf("output differs\n got: %q\nwant: %q", out, tt.want)
			}
			again, changed, err := mergeHooks(out, "/bin/shoalctl")
			if err != nil || changed || string(again) != string(out) {
				t.Fatalf("second merge changed the file (changed=%v err=%v)", changed, err)
			}
		})
	}
}

func TestInstallHooksAtomicKeepsModeAndOneBackup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	root := t.TempDir()
	bak := filepath.Join(t.TempDir(), "backups", "settings.local.json.bak")
	path := filepath.Join(root, ".claude", "settings.local.json")
	original := `{"model":"keep & <me>"}`
	testutil.WriteFile(t, path, original)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	if _, changed, err := installHooks(root, "/bin/shoalctl", bak); err != nil || !changed {
		t.Fatalf("first install: changed=%v err=%v", changed, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode after merge: %v %v", info.Mode(), err)
	}
	if got := readFile(t, bak); got != original {
		t.Fatalf("backup = %q, want the original bytes", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	testutil.WriteFile(t, path, `{"model":"changed later"}`)
	if _, _, err := installHooks(root, "/other/shoalctl", bak); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, bak); got != original {
		t.Fatalf("backup overwritten on a later merge: %q", got)
	}
}

func TestInstallHooksRefusesSymlinkedSettings(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(t.TempDir(), "shared-settings.json")
	testutil.WriteFile(t, real, `{"a":1}`)
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, ".claude", "settings.local.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := installHooks(root, "/bin/shoalctl", filepath.Join(t.TempDir(), "x.bak")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("installHooks through a symlink: %v, want a symlink error", err)
	}
	if got := readFile(t, real); got != `{"a":1}` {
		t.Fatalf("symlink target changed:\n%s", got)
	}
}
