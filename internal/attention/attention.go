// Package attention decides whether a terminal screen is waiting for a
// human: a dialog (Claude Code's trust or permission prompt, any numbered
// menu with a selection cursor) or a yes/no question on the last line.
package attention

import (
	"regexp"
	"strings"
)

// window is how many lines from the bottom of the screen a menu may sit.
// A dialog Claude Code is waiting on is drawn at the bottom; an answered one
// scrolls away or is cleared.
const window = 12

// boxChars are the frame characters dialogs are drawn with.
const boxChars = " \t│┃|╭╮╰╯─━┌┐└┘"

var (
	cursorOption = regexp.MustCompile(`^❯\s*(\d+)[.)]\s+\S`)
	plainOption  = regexp.MustCompile(`^(\d+)[.)]\s+\S`)
	yesNo        = regexp.MustCompile(`(?i)(\(y/n\)|\[y/n\]|\(yes/no\)|\[yes/no\])\s*[:?]?$`)
)

// Detect reports what the screen waits for, as a one-line reason, or false
// when it is not waiting for a human.
func Detect(screen string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(screen, "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.Trim(lines[len(lines)-1], boxChars) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return "", false
	}
	if last := strings.Trim(lines[len(lines)-1], boxChars); yesNo.MatchString(last) {
		return "prompt: " + clip(last), true
	}
	from := max(0, len(lines)-window)
	for i := from; i < len(lines); i++ {
		m := cursorOption.FindStringSubmatch(strings.Trim(lines[i], boxChars))
		if m == nil || !hasOtherOption(lines, i, m[1]) {
			continue
		}
		return "dialog: " + question(lines, i), true
	}
	return "", false
}

// hasOtherOption reports whether a differently numbered option sits within
// three lines of the cursor line: a lone "❯ 1." is not a menu.
func hasOtherOption(lines []string, at int, num string) bool {
	for i := max(0, at-3); i < len(lines) && i <= at+3; i++ {
		if i == at {
			continue
		}
		if m := plainOption.FindStringSubmatch(strings.Trim(lines[i], boxChars)); m != nil && m[1] != num {
			return true
		}
	}
	return false
}

// question finds the menu's question: the nearest line above it that ends
// in a question mark, else a generic reason.
func question(lines []string, at int) string {
	for i := at - 1; i >= 0 && i >= at-10; i-- {
		l := strings.Trim(lines[i], boxChars)
		if strings.HasSuffix(l, "?") {
			return clip(l)
		}
	}
	return "a menu is waiting for a choice"
}

func clip(s string) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > 100 {
		return string(r[:97]) + "..."
	}
	return string(r)
}
