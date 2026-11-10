package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	vcs := func(rev, when, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.time", Value: when}, {Key: "vcs.modified", Value: modified}}
	}
	cases := []struct {
		name                  string
		version, commit, date string
		bi                    *debug.BuildInfo
		want                  Info
	}{
		{"release ldflags win", "1.2.3", "abc", "2026-11-10T00:00:00Z",
			&debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}, Settings: vcs("zzz", "2000-01-01T00:00:00Z", "true")},
			Info{"1.2.3", "abc", "2026-11-10T00:00:00Z"}},
		{"go install module version", "", "", "", &debug.BuildInfo{Main: debug.Module{Version: "v0.5.0"}},
			Info{"0.5.0", Unknown, Unknown}},
		{"go build in a checkout", "", "", "", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: vcs("0123456789abcdef", "2026-11-09T10:00:00Z", "false")},
			Info{Unknown, "0123456789abcdef", "2026-11-09T10:00:00Z"}},
		{"dirty checkout", "", "", "", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261109084008-6bc02cad0f61+dirty"}, Settings: vcs("6bc02cad0f61", "t", "true")},
			Info{"0.0.0-20261109084008-6bc02cad0f61+dirty", "6bc02cad0f61", "t"}},
		{"dev placeholder counts as missing", Unknown, Unknown, Unknown, &debug.BuildInfo{Main: debug.Module{Version: "v1.0.0"}},
			Info{"1.0.0", Unknown, Unknown}},
		{"no build info", "", "", "", nil, Info{Unknown, Unknown, Unknown}},
	}
	for _, c := range cases {
		if got := Resolve(c.version, c.commit, c.date, c.bi); got != c.want {
			t.Errorf("%s: Resolve = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestString(t *testing.T) {
	got := Info{"1.2.3", "0123456789abcdef0123", "2026-11-10T00:00:00Z"}.String()
	if want := "1.2.3 (commit 0123456789ab, built 2026-11-10T00:00:00Z)"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	if got := (Info{Unknown, Unknown, Unknown}).String(); got != "dev (commit dev, built dev)" {
		t.Fatalf("String of an unknown build = %q", got)
	}
}
