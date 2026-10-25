package testrun

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

type junitSuite struct {
	Name   string       `xml:"name,attr"`
	File   string       `xml:"file,attr"`
	Suites []junitSuite `xml:"testsuite"`
	Cases  []junitCase  `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	File      string        `xml:"file,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitMessage `xml:"failure"`
	Error     *junitMessage `xml:"error"`
	Skipped   *junitMessage `xml:"skipped"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

// ParseJUnitFiles reads every file in order and assigns stable test ids:
// <classname>::<name>, falling back to the file attribute and then the
// enclosing suite name, with #2, #3 suffixes for duplicates.
func ParseJUnitFiles(files []string) ([]Result, error) {
	var out []Result
	seen := map[string]int{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read JUnit file %s: %w", f, err)
		}
		var root struct {
			XMLName xml.Name
			junitSuite
		}
		if err := xml.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("parse JUnit file %s: %w", f, err)
		}
		if root.XMLName.Local != "testsuites" && root.XMLName.Local != "testsuite" {
			return nil, fmt.Errorf("parse JUnit file %s: root element is <%s>, want <testsuites> or <testsuite>", f, root.XMLName.Local)
		}
		walkSuite(root.junitSuite, "", "", seen, &out)
	}
	return out, nil
}

func walkSuite(s junitSuite, parentName, parentFile string, seen map[string]int, out *[]Result) {
	name := firstNonEmpty(s.Name, parentName)
	file := firstNonEmpty(s.File, parentFile)
	for _, c := range s.Cases {
		id := firstNonEmpty(c.Classname, c.File, file, name) + "::" + c.Name
		seen[id]++
		if n := seen[id]; n > 1 {
			id = fmt.Sprintf("%s#%d", id, n)
		}
		r := Result{TestID: id, Status: "pass", DurationMS: seconds(c.Time)}
		switch {
		case c.Failure != nil:
			r.Status, r.Message = "fail", c.Failure.text()
		case c.Error != nil:
			r.Status, r.Message = "fail", c.Error.text()
		case c.Skipped != nil:
			r.Status, r.Message = "skip", c.Skipped.text()
		}
		*out = append(*out, r)
	}
	for _, child := range s.Suites {
		walkSuite(child, name, file, seen, out)
	}
}

func (m *junitMessage) text() string {
	return truncate(strings.TrimSpace(m.Message + "\n" + m.Text))
}

func seconds(s string) int64 {
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0
	}
	return int64(f * 1000)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

type goEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
	Elapsed float64
}

type goTest struct {
	status  string
	elapsed float64
	output  strings.Builder
}

// ParseGoJSON reads go test -json output. Lines that are not JSON events are
// ignored. It returns the results and the number of events read.
func ParseGoJSON(r io.Reader) ([]Result, int, error) {
	tests := map[string]*goTest{}
	var order []string
	pkgFailed := map[string]bool{}
	pkgHasTests := map[string]bool{}
	pkgOutput := map[string]*strings.Builder{}
	var pkgOrder []string
	events := 0

	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[0] == '{' {
			var ev goEvent
			if json.Unmarshal(line, &ev) == nil && ev.Action != "" {
				events++
				if _, ok := pkgOutput[ev.Package]; !ok {
					pkgOutput[ev.Package] = &strings.Builder{}
					pkgOrder = append(pkgOrder, ev.Package)
				}
				if ev.Test == "" {
					switch ev.Action {
					case "fail":
						pkgFailed[ev.Package] = true
					case "output", "build-output":
						appendCapped(pkgOutput[ev.Package], ev.Output)
					}
				} else {
					key := ev.Package + "::" + ev.Test
					t, ok := tests[key]
					if !ok {
						t = &goTest{}
						tests[key] = t
						order = append(order, key)
						pkgHasTests[ev.Package] = true
					}
					switch ev.Action {
					case "pass", "fail", "skip":
						t.status, t.elapsed = ev.Action, ev.Elapsed
					case "output":
						appendCapped(&t.output, ev.Output)
					}
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, events, fmt.Errorf("read go test -json output: %w", err)
		}
	}

	var out []Result
	for _, key := range order {
		t := tests[key]
		r := Result{TestID: key, Status: t.status, DurationMS: int64(t.elapsed * 1000)}
		switch t.status {
		case "":
			r.Status, r.Message = "fail", truncate("no pass/fail/skip event (timeout or panic)\n"+t.output.String())
		case "fail", "skip":
			r.Message = truncate(t.output.String())
		}
		out = append(out, r)
	}
	for _, pkg := range pkgOrder {
		if pkgFailed[pkg] && !pkgHasTests[pkg] {
			out = append(out, Result{TestID: pkg + "::(package)", Status: "fail", Message: truncate(pkgOutput[pkg].String())})
		}
	}
	return out, events, nil
}

func appendCapped(b *strings.Builder, s string) {
	if b.Len() < 4*maxMessage {
		b.WriteString(s)
	}
}
