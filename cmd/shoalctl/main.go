package main

import (
	"fmt"
	"os"
)

var version = "0.0.0-dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] != "version" && os.Args[1] != "--version" {
		fmt.Fprintf(os.Stderr, "shoalctl: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
	fmt.Println("shoalctl " + version)
}
