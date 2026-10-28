package cli

import (
	"bytes"
	"encoding/json"
	"strings"
)

// A minimal span scanner for JSON that json.Valid has already accepted. It
// lets init splice its hook entries into a settings file without re-encoding
// the user's bytes: key order, escapes, unicode, indentation and line endings
// outside the edited spans stay exactly as they were.

type span struct{ start, end int } // [start, end) in the document

type jsonMember struct {
	key   string
	whole span // from the key's opening quote to the end of the value
	keyAt span
	val   span
}

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func skipSpace(b []byte, i int) int {
	for i < len(b) && isJSONSpace(b[i]) {
		i++
	}
	return i
}

// valueEnd returns the index just past the value that starts at i.
func valueEnd(b []byte, i int) int {
	switch b[i] {
	case '"':
		for i++; b[i] != '"'; i++ {
			if b[i] == '\\' {
				i++
			}
		}
		return i + 1
	case '{', '[':
		depth := 0
		for ; ; i++ {
			switch b[i] {
			case '"':
				i = valueEnd(b, i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
		}
	}
	for i < len(b) && !isJSONSpace(b[i]) && b[i] != ',' && b[i] != '}' && b[i] != ']' {
		i++
	}
	return i
}

func topValue(b []byte) span {
	i := skipSpace(b, 0)
	return span{i, valueEnd(b, i)}
}

func objectMembers(b []byte, obj span) []jsonMember {
	var out []jsonMember
	for i := skipSpace(b, obj.start+1); b[i] != '}'; {
		keyEnd := valueEnd(b, i)
		var key string
		_ = json.Unmarshal(b[i:keyEnd], &key) // valid JSON: a key is always a string
		vs := skipSpace(b, skipSpace(b, keyEnd)+1)
		ve := valueEnd(b, vs)
		out = append(out, jsonMember{key: key, whole: span{i, ve}, keyAt: span{i, keyEnd}, val: span{vs, ve}})
		if i = skipSpace(b, ve); b[i] == ',' {
			i = skipSpace(b, i+1)
		}
	}
	return out
}

func arrayElements(b []byte, arr span) []span {
	var out []span
	for i := skipSpace(b, arr.start+1); b[i] != ']'; {
		end := valueEnd(b, i)
		out = append(out, span{i, end})
		if i = skipSpace(b, end); b[i] == ',' {
			i = skipSpace(b, i+1)
		}
	}
	return out
}

// member finds key in an object. Like encoding/json, the last duplicate wins.
func member(b []byte, obj span, key string) (jsonMember, bool) {
	var found jsonMember
	ok := false
	for _, m := range objectMembers(b, obj) {
		if m.key == key {
			found, ok = m, true
		}
	}
	return found, ok
}

func splice(b []byte, from, to int, text string) []byte {
	out := make([]byte, 0, len(b)-(to-from)+len(text))
	out = append(out, b[:from]...)
	out = append(out, text...)
	return append(out, b[to:]...)
}

// jsonStyle is how a document lays itself out, copied into inserted text.
type jsonStyle struct {
	nl, unit  string
	multiline bool
}

func detectStyle(b []byte) jsonStyle {
	st := jsonStyle{nl: "\n", unit: "  "}
	if bytes.Contains(b, []byte("\r\n")) {
		st.nl = "\r\n"
	}
	top := topValue(b)
	st.multiline = bytes.IndexByte(b[top.start:top.end], '\n') >= 0
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		ws := lineIndentOf(line)
		if ws != "" && len(ws) < len(line) {
			st.unit = ws
			break
		}
	}
	return st
}

func lineIndentOf(line []byte) string {
	n := 0
	for n < len(line) && (line[n] == ' ' || line[n] == '\t') {
		n++
	}
	return string(line[:n])
}

// lineIndent is the leading whitespace of the line holding pos.
func lineIndent(b []byte, pos int) string {
	return lineIndentOf(b[bytes.LastIndexByte(b[:pos], '\n')+1:])
}

// render encodes v without HTML escaping. In a multi-line document it is
// indented so that it fits at indentation ind.
func render(v any, ind string, st jsonStyle) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if st.multiline {
		enc.SetIndent(ind, st.unit)
	}
	if err := enc.Encode(v); err != nil {
		panic(err) // only fixed shoal types and strings are rendered
	}
	return strings.ReplaceAll(strings.TrimSuffix(buf.String(), "\n"), "\n", st.nl)
}

// appendItem adds an item at the end of the container c, whose current items
// are items. item builds the text for a given indentation. The new item
// copies the whitespace that precedes the current last item.
func appendItem(b []byte, c span, items []span, st jsonStyle, item func(ind string) string) []byte {
	if len(items) > 0 {
		last := items[len(items)-1]
		ws := last.start
		for ws > 0 && isJSONSpace(b[ws-1]) {
			ws--
		}
		lead := string(b[ws:last.start])
		ind := lead[strings.LastIndexByte(lead, '\n')+1:]
		return splice(b, last.end, last.end, ","+lead+item(ind))
	}
	if !st.multiline {
		return splice(b, c.start+1, c.end-1, item(""))
	}
	base := lineIndent(b, c.start)
	ind := base + st.unit
	return splice(b, c.start+1, c.end-1, st.nl+ind+item(ind)+st.nl+base)
}

// appendMember adds "key": value as the last member of obj.
func appendMember(b []byte, obj span, key, value string, st jsonStyle) []byte {
	ms := objectMembers(b, obj)
	sep := ": "
	if !st.multiline {
		sep = ":"
	}
	items := make([]span, len(ms))
	for i, m := range ms {
		items[i] = m.whole
		sep = string(b[m.keyAt.end:m.val.start])
	}
	return appendItem(b, obj, items, st, func(string) string { return render(key, "", st) + sep + value })
}

// removeItem deletes items[i] from container c, with its separator.
func removeItem(b []byte, c span, items []span, i int) []byte {
	switch {
	case len(items) == 1:
		return splice(b, c.start+1, c.end-1, "")
	case i == 0:
		return splice(b, items[0].start, items[1].start, "")
	}
	return splice(b, items[i-1].end, items[i].end, "")
}
