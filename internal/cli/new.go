package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxSlugWords = 40

// slugWords lowercases the title, collapses runs outside [a-z0-9] into "-",
// and cuts it to at most 40 characters at a "-" boundary.
func slugWords(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > maxSlugWords {
		cut := s[:maxSlugWords+1]
		if i := strings.LastIndexByte(cut, '-'); i > 0 {
			s = cut[:i]
		} else {
			s = s[:maxSlugWords]
		}
		s = strings.Trim(s, "-")
	}
	if s == "" {
		return "task"
	}
	return s
}

func validTitle(title string) error {
	n := utf8.RuneCountInString(title)
	if strings.TrimSpace(title) == "" || n > 120 {
		return fail(ExitUsage, "title must be 1-120 characters, got %d", n)
	}
	for _, r := range title {
		if unicode.IsControl(r) {
			return fail(ExitUsage, "title must be one line without control characters")
		}
	}
	return nil
}

func (env Env) cmdNew(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	ask := fs.String("ask", "", "what the task should achieve (default: the title)")
	askFile := fs.String("ask-file", "", "read the ask from a file")
	pos, err := parseArgs(fs, args, 2)
	if err != nil {
		return err
	}
	lane, title := pos[0], strings.TrimSpace(pos[1])
	if lane != "feature" && lane != "bugfix" && lane != "hotfix" {
		return fail(ExitUsage, "lane %q must be feature, bugfix or hotfix", lane)
	}
	if err := validTitle(title); err != nil {
		return err
	}
	if *ask != "" && *askFile != "" {
		return fail(ExitUsage, "use --ask or --ask-file, not both")
	}
	text := title
	switch {
	case *ask != "":
		text = *ask
	case *askFile != "":
		path := *askFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(env.Dir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fail(ExitUsage, "read --ask-file: %v", err)
		}
		if strings.TrimSpace(string(data)) == "" {
			return fail(ExitUsage, "--ask-file %s is empty", *askFile)
		}
		text = string(data)
	}

	p, err := env.openProject(ctx)
	if err != nil {
		return err
	}
	defer p.close()
	words := slugWords(title)
	t, err := p.db.CreateTask(ctx, lane, title, strings.TrimSpace(text), func(id int64) string { return fmt.Sprintf("%d-%s", id, words) })
	if err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, t.Slug)
	return nil
}
