package worker

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Claude Code settings editing (KON-MFHK0). ~/.claude/settings.json belongs to the user:
// Konsert never re-encodes it. Installing or removing the Konsert hook entries edits the
// JSON text in place (only our array elements, or the members that hold nothing but
// them, are inserted or cut), in the file's own indentation, separators and line
// endings, so the user's key order and formatting survive and removing our entries gives
// back what was there before. Nothing is written when there is nothing to change; before
// any change a timestamped backup is taken next to the file and only our last
// claudeSettingsBackups backups are kept.

// claudeHookEvents are the Claude Code hook events Konsert installs, in install order.
var claudeHookEvents = []string{"PreToolUse", "PostToolUse", "Notification", "Stop"}

// konsertHookMarker identifies Konsert-managed hook entries (quoted or unquoted binary path).
const konsertHookMarker = " hook-event --agent claude --event "

// claudeSettingsBackupInfix names our backups: settings.json.konsert-bak-YYYYMMDD-HHMMSS.
const claudeSettingsBackupInfix = ".konsert-bak-"

// claudeSettingsBackups is how many of our backups are kept per file.
const claudeSettingsBackups = 3

// settingsNow is the backup clock (tests replace it).
var settingsNow = time.Now

type claudeHookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type claudeHookEntry struct {
	Matcher string              `json:"matcher"`
	Hooks   []claudeHookCommand `json:"hooks"`
}

// konsertHookEntry is the entry Konsert installs for event.
func konsertHookEntry(konsertdBin, event string) claudeHookEntry {
	cmd := fmt.Sprintf("%s hook-event --agent claude --event %s", shellQuote(konsertdBin), event)
	return claudeHookEntry{Matcher: "*", Hooks: []claudeHookCommand{{Type: "command", Command: cmd}}}
}

// installClaudeHooks adds the Konsert hook entries to ~/.claude/settings.json (replacing
// stale ones from an older install). It reports whether the file changed.
func installClaudeHooks(homeDir, konsertdBin string) error {
	_, err := editClaudeSettingsFile(filepath.Join(homeDir, ".claude", "settings.json"), konsertdBin, true)
	return err
}

// removeClaudeHooks takes the Konsert hook entries out of ~/.claude/settings.json.
func removeClaudeHooks(homeDir string) (bool, error) {
	return editClaudeSettingsFile(filepath.Join(homeDir, ".claude", "settings.json"), "", false)
}

// editClaudeSettingsFile installs (or removes) our entries in the file at path, with a
// backup before any change. A missing file is created on install and left alone on
// removal.
func editClaudeSettingsFile(path, konsertdBin string, install bool) (bool, error) {
	// settings.json may be a symlink (a second Claude config dir sharing it): edit the
	// file it points to, so the rename below never replaces the link.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	orig, err := os.ReadFile(path)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return false, err
	}
	if missing && !install {
		return false, nil
	}
	var out []byte
	if install {
		out, err = installClaudeHookText(orig, konsertdBin)
	} else {
		out, err = removeClaudeHookText(orig)
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if !missing && bytes.Equal(out, orig) {
		return false, nil
	}
	mode := os.FileMode(0o600)
	if missing {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, err
		}
	} else {
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
		if err := backupClaudeSettings(path, orig, mode); err != nil {
			return false, fmt.Errorf("back up %s: %w", path, err)
		}
	}
	return true, writeFileAtomic(path, out, mode)
}

// backupClaudeSettings writes orig to path.konsert-bak-<timestamp> and keeps only our
// last claudeSettingsBackups backups.
func backupClaudeSettings(path string, orig []byte, mode os.FileMode) error {
	base := path + claudeSettingsBackupInfix + settingsNow().Format("20060102-150405")
	name := base
	for i := 2; ; i++ {
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, os.ErrExist) {
			name = fmt.Sprintf("%s-%d", base, i)
			continue
		}
		if err != nil {
			return err
		}
		_, werr := f.Write(orig)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return werr
		}
		break
	}
	old, err := claudeSettingsBackupList(path)
	if err != nil {
		return err
	}
	for len(old) > claudeSettingsBackups {
		if err := os.Remove(old[0]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		old = old[1:]
	}
	return nil
}

// claudeSettingsBackupList lists our backups of path, oldest first.
func claudeSettingsBackupList(path string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	prefix := filepath.Base(path) + claudeSettingsBackupInfix
	type bak struct {
		name string
		mod  time.Time
	}
	var list []bak
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		var mod time.Time
		if fi, err := e.Info(); err == nil {
			mod = fi.ModTime()
		}
		list = append(list, bak{filepath.Join(filepath.Dir(path), e.Name()), mod})
	}
	// Names sort by their timestamp; a same-second suffix (-2, -3) sorts after its base.
	slices.SortFunc(list, func(a, b bak) int {
		if c := strings.Compare(backupStamp(a.name), backupStamp(b.name)); c != 0 {
			return c
		}
		return a.mod.Compare(b.mod)
	})
	out := make([]string, len(list))
	for i, b := range list {
		out[i] = b.name
	}
	return out, nil
}

// backupStamp is the sortable part of a backup name: the timestamp, then the same-second
// counter zero-padded.
func backupStamp(name string) string {
	const layout = len("20060102-150405")
	_, s, _ := strings.Cut(filepath.Base(name), claudeSettingsBackupInfix)
	if len(s) <= layout {
		return s + "-000"
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(s[layout:], "-"))
	return s[:layout] + fmt.Sprintf("-%03d", n)
}

// writeFileAtomic writes data to path through a temporary file in the same directory.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- text edits ----------------------------------------------------------------------

// installClaudeHookText returns raw with the Konsert entries installed: stale Konsert
// entries (another binary path, an event Konsert no longer uses, duplicates) are cut,
// missing ones appended. raw is returned unchanged when everything is in place.
func installClaudeHookText(raw []byte, konsertdBin string) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}\n")
	}
	for range 64 { // each pass makes one edit; a settings file never needs this many
		doc, err := parseJSONSpans(raw)
		if err != nil {
			return nil, err
		}
		if doc.kind != '{' {
			return nil, errors.New("settings are not a JSON object")
		}
		f := newJSONFormat(raw, doc)
		hooks := doc.member("hooks")
		if hooks == nil {
			obj := renderHooksObject(konsertdBin)
			raw = f.insertMember(raw, doc, "hooks", func(prefix string) []byte { return f.reindent(obj, prefix) })
			continue
		}
		if hooks.kind != '{' {
			return nil, errors.New(`"hooks" is not a JSON object`)
		}
		if next, ok := cutStaleKonsertEntry(raw, f, doc, hooks, konsertdBin); ok {
			raw = next
			continue
		}
		edited := false
		for _, ev := range claudeHookEvents {
			want := konsertHookEntry(konsertdBin, ev)
			arr := hooks.member(ev)
			if arr == nil {
				val := mustJSON([]claudeHookEntry{want})
				raw = f.insertMember(raw, hooks, ev, func(prefix string) []byte { return f.reindent(val, prefix) })
				edited = true
				break
			}
			if arr.kind != '[' {
				return nil, fmt.Errorf("hooks.%s is not a JSON array", ev)
			}
			if !slices.ContainsFunc(arr.kids, func(n *jsonSpan) bool { return isEntry(raw, n, want) }) {
				val := mustJSON(want)
				raw = f.insertElem(raw, arr, func(prefix string) []byte { return f.reindent(val, prefix) })
				edited = true
				break
			}
		}
		if !edited {
			return raw, nil
		}
	}
	return nil, errors.New("too many edits")
}

// removeClaudeHookText returns raw without any Konsert entry. An event array (and the
// hooks object) left empty by the removal goes too, which is how install added them.
func removeClaudeHookText(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, nil
	}
	for range 64 {
		doc, err := parseJSONSpans(raw)
		if err != nil {
			return nil, err
		}
		if doc.kind != '{' {
			return nil, errors.New("settings are not a JSON object")
		}
		hooks := doc.member("hooks")
		if hooks == nil || hooks.kind != '{' {
			return raw, nil
		}
		f := newJSONFormat(raw, doc)
		next, ok := cutKonsertEntry(raw, f, doc, hooks, func(string, *jsonSpan) bool { return true })
		if !ok {
			return raw, nil
		}
		raw = next
	}
	return nil, errors.New("too many edits")
}

// cutStaleKonsertEntry cuts one Konsert entry that install would not leave in place.
func cutStaleKonsertEntry(raw []byte, f jsonFormat, doc, hooks *jsonSpan, konsertdBin string) ([]byte, bool) {
	return cutKonsertEntry(raw, f, doc, hooks, func(ev string, n *jsonSpan) bool {
		if !slices.Contains(claudeHookEvents, ev) || !isEntry(raw, n, konsertHookEntry(konsertdBin, ev)) {
			return true
		}
		// A duplicate of the wanted entry: keep the first.
		arr := hooks.member(ev)
		first := slices.IndexFunc(arr.kids, func(k *jsonSpan) bool { return isEntry(raw, k, konsertHookEntry(konsertdBin, ev)) })
		return arr.kids[first] != n
	})
}

// cutKonsertEntry cuts the first Konsert entry for which cut says so. When it is the
// last element of its array, the array's member goes (and the hooks member when that
// was its last member).
func cutKonsertEntry(raw []byte, f jsonFormat, doc, hooks *jsonSpan, cut func(ev string, n *jsonSpan) bool) ([]byte, bool) {
	for mi, ev := range hooks.keys {
		arr := hooks.kids[mi]
		if arr.kind != '[' {
			continue
		}
		for i, n := range arr.kids {
			if !bytes.Contains(raw[n.start:n.end], []byte(konsertHookMarker)) || !cut(ev, n) {
				continue
			}
			if len(arr.kids) > 1 {
				return f.cutElem(raw, arr, i), true
			}
			if len(hooks.kids) > 1 {
				return f.cutMember(raw, hooks, mi), true
			}
			return f.cutMember(raw, doc, slices.Index(doc.keys, "hooks")), true
		}
	}
	return raw, false
}

// isEntry reports whether the JSON value at n is want.
func isEntry(raw []byte, n *jsonSpan, want claudeHookEntry) bool {
	if n.kind != '{' {
		return false
	}
	var got claudeHookEntry
	if err := json.Unmarshal(raw[n.start:n.end], &got); err != nil {
		return false
	}
	return got.Matcher == want.Matcher && slices.Equal(got.Hooks, want.Hooks)
}

func mustJSON(v any) jsontext.Value {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// renderHooksObject is {"PreToolUse": [entry], ...} in claudeHookEvents order.
func renderHooksObject(konsertdBin string) jsontext.Value {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, ev := range claudeHookEvents {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(mustJSON(ev))
		b.WriteByte(':')
		b.Write(mustJSON([]claudeHookEntry{konsertHookEntry(konsertdBin, ev)}))
	}
	b.WriteByte('}')
	return b.Bytes()
}

// --- JSON spans ----------------------------------------------------------------------

// jsonSpan is a JSON value's byte range in the document, with its members (objects:
// keys, keyStarts, kids) or elements (arrays: kids).
type jsonSpan struct {
	kind       jsontext.Kind
	start, end int
	keys       []string
	keyStarts  []int
	kids       []*jsonSpan
}

func (n *jsonSpan) member(key string) *jsonSpan {
	if i := slices.Index(n.keys, key); i >= 0 {
		return n.kids[i]
	}
	return nil
}

// parseJSONSpans parses raw (one JSON value, strict) into spans.
func parseJSONSpans(raw []byte) (*jsonSpan, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	root, err := parseSpan(dec, raw)
	if err != nil {
		return nil, err
	}
	if _, err := dec.ReadToken(); err != io.EOF {
		return nil, errors.New("unexpected data after the JSON object")
	}
	return root, nil
}

// skipSeparators returns the offset of the next value or name at or after off.
func skipSeparators(raw []byte, off int) int {
	for off < len(raw) && strings.IndexByte(" \t\r\n,:", raw[off]) >= 0 {
		off++
	}
	return off
}

func parseSpan(dec *jsontext.Decoder, raw []byte) (*jsonSpan, error) {
	start := skipSeparators(raw, int(dec.InputOffset()))
	switch kind := dec.PeekKind(); kind {
	case '{', '[':
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		n := &jsonSpan{kind: kind, start: start}
		end := jsontext.Kind('}')
		if kind == '[' {
			end = ']'
		}
		for dec.PeekKind() != end {
			if kind == '{' {
				ks := skipSeparators(raw, int(dec.InputOffset()))
				tok, err := dec.ReadToken()
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, tok.String())
				n.keyStarts = append(n.keyStarts, ks)
			}
			kid, err := parseSpan(dec, raw)
			if err != nil {
				return nil, err
			}
			n.kids = append(n.kids, kid)
		}
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		n.end = int(dec.InputOffset())
		return n, nil
	default:
		if _, err := dec.ReadValue(); err != nil {
			return nil, err
		}
		return &jsonSpan{kind: kind, start: start, end: int(dec.InputOffset())}, nil
	}
}

// --- format-preserving edits ---------------------------------------------------------

// jsonFormat is the document's layout: indentation unit, name separator, line ending.
type jsonFormat struct {
	unit    string // one indentation level ("  ", "    ", "\t")
	colon   string // between a name and its value (": " or ":")
	eol     string // "\n" or "\r\n"
	compact bool   // the whole document is on one line
}

func newJSONFormat(raw []byte, doc *jsonSpan) jsonFormat {
	f := jsonFormat{unit: "  ", colon: ": ", eol: "\n"}
	if bytes.Contains(raw, []byte("\r\n")) {
		f.eol = "\r\n"
	}
	f.compact = len(doc.kids) > 0 && !bytes.ContainsAny(raw[doc.start:doc.end], "\n")
	if f.compact && len(doc.kids) > 0 {
		f.colon = ":"
	}
	// The first nested member or element on its own line gives the unit: its
	// indentation minus its container's.
	var find func(n *jsonSpan) bool
	find = func(n *jsonSpan) bool {
		for i, k := range n.kids {
			at := k.start
			if n.kind == '{' {
				at = n.keyStarts[i]
				// Name separator as written.
				sep := string(raw[skipName(raw, at):k.start])
				if strings.Contains(sep, ":") {
					f.colon = strings.TrimLeft(sep, " \t")
				}
			}
			if ind, ok := lineIndent(raw, at); ok {
				parent, _ := lineIndent(raw, n.start)
				if u, ok := strings.CutPrefix(ind, parent); ok && u != "" {
					f.unit = u
					return true
				}
			}
			if find(k) {
				return true
			}
		}
		return false
	}
	find(doc)
	return f
}

// skipName returns the offset just past the JSON string starting at off.
func skipName(raw []byte, off int) int {
	i := off + 1
	for i < len(raw) {
		switch raw[i] {
		case '\\':
			i += 2
			continue
		case '"':
			return i + 1
		}
		i++
	}
	return i
}

// lineIndent returns the whitespace before off on its line, and whether only whitespace
// precedes off there (off starts the line's content).
func lineIndent(raw []byte, off int) (string, bool) {
	ls := bytes.LastIndexByte(raw[:off], '\n') + 1
	ind := string(raw[ls:off])
	return ind, strings.TrimLeft(ind, " \t") == ""
}

// containerIndent is the indentation of the line a container starts on.
func containerIndent(raw []byte, n *jsonSpan) string {
	ls := bytes.LastIndexByte(raw[:n.start], '\n') + 1
	i := ls
	for i < n.start && (raw[i] == ' ' || raw[i] == '\t') {
		i++
	}
	return string(raw[ls:i])
}

// reindent renders v for insertion at a line indented by prefix (compact in a compact
// document).
func (f jsonFormat) reindent(v jsontext.Value, prefix string) []byte {
	if f.compact {
		out := bytes.Clone(v)
		if err := (*jsontext.Value)(&out).Compact(); err != nil {
			return v
		}
		return out
	}
	out := bytes.Clone(v)
	if err := (*jsontext.Value)(&out).Indent(jsontext.WithIndentPrefix(prefix), jsontext.WithIndent(f.unit)); err != nil {
		return v
	}
	s := string(out)
	if f.colon != ": " {
		s = strings.ReplaceAll(s, `": `, `"`+f.colon) // keys never end in `": ` inside strings we render
	}
	if f.eol != "\n" {
		s = strings.ReplaceAll(s, "\n", f.eol)
	}
	return []byte(s)
}

// childIndent is where n's members or elements go: like its first one when that is on
// its own line, else one unit deeper than n's line.
func (f jsonFormat) childIndent(raw []byte, n *jsonSpan) (string, bool) {
	if len(n.kids) > 0 {
		at := n.kids[0].start
		if n.kind == '{' {
			at = n.keyStarts[0]
		}
		if ind, ok := lineIndent(raw, at); ok && bytes.ContainsAny(raw[n.start:at], "\n") {
			return ind, true
		}
		return "", false // members on the container's line
	}
	return containerIndent(raw, n) + f.unit, !f.compact
}

// insert puts text (a member or an element, rendered for an indentation prefix) last in
// container n.
func (f jsonFormat) insert(raw []byte, n *jsonSpan, render func(prefix string) []byte) []byte {
	ind, multiline := f.childIndent(raw, n)
	var piece []byte
	var at int
	switch {
	case len(n.kids) == 0 && multiline:
		// {} → {⏎ind piece⏎containerIndent}
		at = n.start + 1
		piece = []byte(f.eol + ind)
		piece = append(piece, render(ind)...)
		piece = append(piece, (f.eol + containerIndent(raw, n))...)
		return splice(raw, at, n.end-1, piece)
	case len(n.kids) == 0:
		return splice(raw, n.start+1, n.end-1, render(""))
	case multiline:
		at = n.kids[len(n.kids)-1].end
		piece = []byte("," + f.eol + ind)
		piece = append(piece, render(ind)...)
	default:
		at = n.kids[len(n.kids)-1].end
		sep := ","
		if !f.compact {
			sep = ", "
		}
		piece = append([]byte(sep), render("")...)
	}
	return splice(raw, at, at, piece)
}

func (f jsonFormat) insertElem(raw []byte, arr *jsonSpan, render func(prefix string) []byte) []byte {
	return f.insert(raw, arr, render)
}

func (f jsonFormat) insertMember(raw []byte, obj *jsonSpan, key string, render func(prefix string) []byte) []byte {
	name := mustJSON(key)
	return f.insert(raw, obj, func(prefix string) []byte {
		return append(append(bytes.Clone([]byte(name)), f.colon...), render(prefix)...)
	})
}

// cutElem removes element i of arr with the separator and whitespace that came with it.
func (f jsonFormat) cutElem(raw []byte, arr *jsonSpan, i int) []byte {
	starts := make([]int, len(arr.kids))
	for j, k := range arr.kids {
		starts[j] = k.start
	}
	return f.cut(raw, arr, starts, i)
}

// cutMember removes member i of obj with the separator and whitespace that came with it.
func (f jsonFormat) cutMember(raw []byte, obj *jsonSpan, i int) []byte {
	return f.cut(raw, obj, obj.keyStarts, i)
}

func (f jsonFormat) cut(raw []byte, n *jsonSpan, starts []int, i int) []byte {
	switch {
	case len(n.kids) == 1:
		return splice(raw, n.start+1, n.end-1, nil)
	case i > 0:
		return splice(raw, n.kids[i-1].end, n.kids[i].end, nil)
	default:
		return splice(raw, starts[0], starts[1], nil)
	}
}

func splice(raw []byte, from, to int, piece []byte) []byte {
	out := make([]byte, 0, len(raw)-(to-from)+len(piece))
	out = append(out, raw[:from]...)
	out = append(out, piece...)
	return append(out, raw[to:]...)
}
