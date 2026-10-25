package main

import (
	"fmt"
	"os"

	"github.com/kennethsolomon/shoalctl/internal/cli"
)

var version = "0.0.0-dev"

func main() {
	env, err := cli.DefaultEnv()
	if err != nil {
		if len(os.Args) > 1 && os.Args[1] == "hook" {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "shoalctl: %v\n", err)
		os.Exit(cli.ExitInternal)
	}
	os.Exit(cli.Main(os.Args[1:], env, version))
}
