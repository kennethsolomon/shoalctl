// Package guide holds the text of shoalctl guide. The .md files beside this
// one are plain text; the README quotes quick.md, setup.md and tests.md
// verbatim.
package guide

import (
	"embed"
	"errors"
	"fmt"
)

//go:embed *.md
var files embed.FS

// Topics are the guide's topics, in the order the cheat sheet lists them.
var Topics = []string{"setup", "tests", "flow", "memory"}

// ConfigPlaceholder marks where the cheat sheet shows the project's config
// path.
const ConfigPlaceholder = "{config}"

// ErrUnknownTopic is returned for a topic not in Topics.
var ErrUnknownTopic = errors.New("unknown guide topic")

// Index is the cheat sheet that shoalctl guide prints with no topic: the
// five-command quick flow (quick.md), then the rest (index.md).
func Index() (string, error) {
	quick, err := Quick()
	if err != nil {
		return "", err
	}
	rest, err := read("index.md")
	if err != nil {
		return "", err
	}
	return quick + rest, nil
}

// Quick is the five-command bugfix flow that opens the cheat sheet.
func Quick() (string, error) {
	return read("quick.md")
}

// Topic is the text of one topic.
func Topic(name string) (string, error) {
	for _, t := range Topics {
		if t == name {
			return read(name + ".md")
		}
	}
	return "", fmt.Errorf("%q: %w", name, ErrUnknownTopic)
}

func read(file string) (string, error) {
	b, err := files.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read embedded guide %s: %w", file, err)
	}
	return string(b), nil
}
