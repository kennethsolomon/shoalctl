package releaseguard

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseArtifacts is the release CI guard: the release workflow runs it
// with SHOAL_DIST set to GoReleaser's dist/ before anything is published.
func TestReleaseArtifacts(t *testing.T) {
	dist := os.Getenv("SHOAL_DIST")
	if dist == "" {
		t.Skip("SHOAL_DIST is not set; the release workflow sets it to dist/")
	}
	problems, err := Check(dist)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

func writeTarGz(t *testing.T, path string, files ...string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, name := range files {
		if strings.HasSuffix(name, "/") {
			tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755})
			continue
		}
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1})
		tw.Write([]byte("x"))
	}
	tw.Close()
	gz.Close()
	f.Close()
}

func writeZip(t *testing.T, path string, files ...string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, name := range files {
		w, _ := zw.Create(name)
		w.Write([]byte("x"))
	}
	zw.Close()
	f.Close()
}

// writeSums writes checksums.txt for every archive in dist.
func writeSums(t *testing.T, dist string) {
	t.Helper()
	entries, _ := os.ReadDir(dist)
	var b strings.Builder
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tar.gz") || strings.HasSuffix(e.Name(), ".zip") {
			data, _ := os.ReadFile(filepath.Join(dist, e.Name()))
			sum := sha256.Sum256(data)
			b.WriteString(hex.EncodeToString(sum[:]) + "  " + e.Name() + "\n")
		}
	}
	os.WriteFile(filepath.Join(dist, "checksums.txt"), []byte(b.String()), 0o644)
}

func goodDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	writeTarGz(t, filepath.Join(dist, "shoalctl_1.0.0_linux_amd64.tar.gz"), "shoalctl", "LICENSE", "README.md")
	writeZip(t, filepath.Join(dist, "shoalctl_1.0.0_windows_arm64.zip"), "shoalctl.exe", "LICENSE", "README.md")
	os.MkdirAll(filepath.Join(dist, "shoalctl_linux_amd64_v1"), 0o755)
	os.WriteFile(filepath.Join(dist, "artifacts.json"), []byte("[]"), 0o644)
	writeSums(t, dist)
	return dist
}

func TestCheckPassesACleanDist(t *testing.T) {
	problems, err := Check(goodDist(t))
	if err != nil || len(problems) != 0 {
		t.Fatalf("clean dist: %v %v", problems, err)
	}
}

func TestCheckFailsOnAPlantedDatabaseOrReport(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, dist string)
		want  string
	}{
		{"memory.db in a tarball", func(t *testing.T, d string) {
			writeTarGz(t, filepath.Join(d, "shoalctl_1.0.0_darwin_arm64.tar.gz"), "shoalctl", "LICENSE", "README.md", "memory.db")
		}, "memory.db is a .db file"},
		{"WAL in a zip", func(t *testing.T, d string) {
			writeZip(t, filepath.Join(d, "shoalctl_1.0.0_windows_amd64.zip"), "shoalctl.exe", "x/memory.db-wal")
		}, "x/memory.db-wal is a .db file"},
		{"migration backup", func(t *testing.T, d string) {
			writeTarGz(t, filepath.Join(d, "shoalctl_1.0.0_linux_arm64.tar.gz"), "shoalctl", "memory.db.bak-7")
		}, "memory.db.bak-7 is a .db file"},
		{"reports directory", func(t *testing.T, d string) {
			writeTarGz(t, filepath.Join(d, "shoalctl_1.0.0_linux_arm64.tar.gz"), "shoalctl", "reports/", "reports/1-task.md")
		}, "reports/ is a reports/ path"},
		{"report nested", func(t *testing.T, d string) {
			writeZip(t, filepath.Join(d, "shoalctl_1.0.0_windows_amd64.zip"), "shoalctl.exe", "projects/abc/Reports/x.md")
		}, "projects/abc/Reports/x.md is a reports/ path"},
		{"loose db in dist", func(t *testing.T, d string) {
			os.WriteFile(filepath.Join(d, "memory.db"), []byte("SQLite"), 0o644)
		}, "memory.db: a .db file in dist"},
		{"unexpected file", func(t *testing.T, d string) {
			writeTarGz(t, filepath.Join(d, "shoalctl_1.0.0_linux_arm64.tar.gz"), "shoalctl", "config.toml")
		}, "config.toml is not the binary, LICENSE or README.md"},
		{"no binary", func(t *testing.T, d string) {
			writeTarGz(t, filepath.Join(d, "shoalctl_1.0.0_linux_arm64.tar.gz"), "LICENSE")
		}, "no shoalctl binary"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dist := goodDist(t)
			c.plant(t, dist)
			writeSums(t, dist)
			problems, err := Check(dist)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(problems, "\n"), c.want) {
				t.Fatalf("problems %q lack %q", problems, c.want)
			}
		})
	}
}

func TestCheckVerifiesChecksums(t *testing.T) {
	dist := goodDist(t)
	writeTarGz(t, filepath.Join(dist, "shoalctl_1.0.0_darwin_amd64.tar.gz"), "shoalctl")
	problems, _ := Check(dist)
	if !strings.Contains(strings.Join(problems, "\n"), "shoalctl_1.0.0_darwin_amd64.tar.gz: not in checksums.txt") {
		t.Fatalf("an unlisted archive passed: %q", problems)
	}
	os.WriteFile(filepath.Join(dist, "checksums.txt"), []byte(strings.Repeat("0", 64)+"  shoalctl_1.0.0_darwin_amd64.tar.gz\n"), 0o644)
	problems, _ = Check(dist)
	if !strings.Contains(strings.Join(problems, "\n"), "checksums.txt says 000") {
		t.Fatalf("a wrong checksum passed: %q", problems)
	}
	os.Remove(filepath.Join(dist, "checksums.txt"))
	if problems, _ := Check(dist); !strings.Contains(strings.Join(problems, "\n"), "checksums.txt") {
		t.Fatalf("a missing checksums.txt passed: %q", problems)
	}
	if problems, _ := Check(t.TempDir()); len(problems) != 1 || !strings.Contains(problems[0], "no .tar.gz or .zip archive") {
		t.Fatalf("an empty dist passed: %q", problems)
	}
}

func TestForbidden(t *testing.T) {
	for p, want := range map[string]bool{
		"memory.db": true, "a/b.DB": true, "x.db-shm": true, "x.db-journal": true, "reports": true, "a/reports/": true,
		"shoalctl": false, "README.md": false, "docs/database.md": false, "dbtool": false, "reporting/x": false, "x.dbx": false,
	} {
		if got := Forbidden(p) != ""; got != want {
			t.Errorf("Forbidden(%q) = %v, want %v", p, got, want)
		}
	}
}
