package main

import (
	"fmt"
	"io"
	"os"

	"github.com/kennethsolomon/shoalctl/internal/buildinfo"
	"github.com/kennethsolomon/shoalctl/internal/cli"
)

// Set by release builds: -ldflags "-X main.version=... -X main.commit=...
// -X main.date=...". A go install or go build leaves them "dev", and
// buildinfo fills in what the toolchain recorded.
var (
	version = buildinfo.Unknown
	commit  = buildinfo.Unknown
	date    = buildinfo.Unknown
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		// Hooks need nothing from DefaultEnv, whose filesystem calls would run
		// before the hook's deadline starts. They never write to stdout or
		// stderr (§4.6).
		os.Exit(cli.Main(os.Args[1:], cli.Env{Stdin: os.Stdin, Stdout: io.Discard, Stderr: io.Discard}, version))
	}
	env, err := cli.DefaultEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "shoalctl: %v\n", err)
		os.Exit(cli.ExitInternal)
	}
	os.Exit(cli.Main(os.Args[1:], env, buildinfo.Current(version, commit, date).String()))
}
