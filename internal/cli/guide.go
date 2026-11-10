package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kennethsolomon/shoalctl/docs/guide"
	"github.com/kennethsolomon/shoalctl/internal/config"
	"github.com/kennethsolomon/shoalctl/internal/gitx"
)

// cmdGuide prints the cheat sheet, or one topic of the manual.
func (env Env) cmdGuide(ctx context.Context, args []string) error {
	topics := strings.Join(guide.Topics, ", ")
	switch len(args) {
	case 0:
		index, err := guide.Index()
		if err != nil {
			return err
		}
		cfg, err := env.guideConfigLine(ctx)
		if err != nil {
			return err
		}
		fmt.Fprint(env.Stdout, strings.Replace(index, guide.ConfigPlaceholder, cfg, 1))
		return nil
	case 1:
		text, err := guide.Topic(args[0])
		if errors.Is(err, guide.ErrUnknownTopic) {
			return fail(ExitUsage, "unknown topic %q; topics: %s", args[0], topics)
		}
		if err != nil {
			return err
		}
		fmt.Fprint(env.Stdout, text)
		return nil
	}
	return fail(ExitUsage, "guide takes at most one topic, got %q; topics: %s", args, topics)
}

// guideConfigLine is the project config path of the repo the guide runs in,
// or what to do first when there is no initialized project here.
func (env Env) guideConfigLine(ctx context.Context) (string, error) {
	p, err := env.identify(ctx)
	if errors.Is(err, gitx.ErrNotRepo) {
		return "not in a git repo: cd into your repo, then run shoalctl init first", nil
	}
	if err != nil {
		return "", err
	}
	_, ok, err := readMeta(p.metaPath())
	if err != nil {
		return "", err
	}
	if !ok {
		return "not set up here yet: run shoalctl init first", nil
	}
	path, err := config.ProjectPath(p.root, filepath.Join(p.dir, "config.toml"))
	if err != nil {
		return "", withCode(ExitConfig, err)
	}
	return path, nil
}
