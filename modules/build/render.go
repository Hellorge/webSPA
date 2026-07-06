// Package build owns the build-time templating pipeline for gogogo
// content pages.
//
// Engine: Go's stdlib text/template — full feature set (eq/lt/range/
// with/template/define/block/…), no auto-escape. The previous
// html/template was dropped because its context-aware escape pipeline
// was ~37% of per-request CPU on hole-rendered pages and gogogo's
// content is curated, not user-supplied. If a value needs escaping,
// write `{{ html .X }}` explicitly (`html` is a text/template builtin).
//
// Three of our source-level tags are NOT text/template constructs —
// they're our own build-time directives, extracted by byte pre-scans
// before text/template ever sees the source:
//
//   {% extends "name" %}       chooses the master template to wrap with
//   {% alias    "X" %}         sets the URL alias for this page
//   {% hole "<sql>" %}body{% endhole %}
//                               per-row runtime data insertion
//
// Everything else uses native Go template syntax (`{{ .Var }}`,
// `{{ if eq .X "y" }}…{{ end }}`, `{{ template "sidebar" . }}`,
// `{{ block "content" . }}…{{ end }}`).
//
// Inheritance pattern: master templates declare named blocks via
// `{{ block "name" . }}…{{ end }}`; child pages override via
// `{{ define "name" }}…{{ end }}`. Parsing master + child into the
// same Template set wires it up automatically.

package build

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

// Hole describes one runtime-data insertion point in a rendered page.
// Position is the byte offset within the rendered body where the
// hole's per-row content goes. SQL is the query to run at request
// time. Template is the per-row sub-template source (Go text/template
// syntax — `{{ .field }}` for column access).
type Hole struct {
	Position uint32
	SQL      string
	Template []byte
}

// Marker bytes the build emits at every hole site during rendering.
// After html/template returns, we scan for these markers to record
// byte positions, then strip them. Bytes 0x02 (STX) bracket the
// marker so it can never collide with normal HTML/CSS/JS — both
// control characters with no semantic meaning in any text content
// type.
const (
	holeMarkerPrefix = "\x02\x1FHOLE_"
	holeMarkerSuffix = "_HOLE\x1E\x02"
)

// extractDirectives byte-scans src once and:
//   - captures the {% extends "name" %} value (master template name)
//   - captures every {% hole "<sql>" %} body {% endhole %} block (sql + body)
//   - strips the extends + alias + hole tags from the returned source
//   - rewrites each hole site to `{{ hole <index> }}` so the html/template
//     parser sees a normal func call
//
// Alias is NOT returned — caller's processAlias() byte-scanned for it
// separately during dir walk; we just strip it here so the template
// parser doesn't choke. extends defaults to "def" if absent.
func extractDirectives(src []byte) (cleanSrc []byte, masterName string, sqls []string, bodies [][]byte, err error) {
	masterName = "def"

	var out bytes.Buffer
	out.Grow(len(src))

	i := 0
	for i < len(src) {
		if !bytes.HasPrefix(src[i:], []byte("{% ")) {
			out.WriteByte(src[i])
			i++
			continue
		}

		// Found a {% — figure out which directive (if any).
		switch {
		case bytes.HasPrefix(src[i:], []byte("{% extends ")):
			end, name, perr := readQuotedTagArg(src, i, "extends")
			if perr != nil {
				return nil, "", nil, nil, perr
			}
			masterName = name
			i = end // skip past {% extends "x" %}, emit nothing

		case bytes.HasPrefix(src[i:], []byte("{% alias ")):
			end, _, perr := readQuotedTagArg(src, i, "alias")
			if perr != nil {
				return nil, "", nil, nil, perr
			}
			i = end // alias is captured by processAlias; just strip here

		case bytes.HasPrefix(src[i:], []byte("{% hole ")):
			endTag, sql, perr := readQuotedTagArg(src, i, "hole")
			if perr != nil {
				return nil, "", nil, nil, perr
			}
			bodyStart := endTag
			endIdx := bytes.Index(src[bodyStart:], []byte("{% endhole %}"))
			if endIdx < 0 {
				return nil, "", nil, nil, fmt.Errorf("unterminated {%% hole %%} block at offset %d", i)
			}
			bodyEnd := bodyStart + endIdx
			blockEnd := bodyEnd + len("{% endhole %}")

			idx := len(sqls)
			sqls = append(sqls, sql)
			bodies = append(bodies, bytes.TrimSpace(src[bodyStart:bodyEnd]))

			// Rewrite block site to a Go template func call. `hole` is
			// registered in the FuncMap; it returns the sentinel marker
			// for this index.
			out.WriteString("{{ hole ")
			out.WriteString(strconv.Itoa(idx))
			out.WriteString(" }}")
			i = blockEnd

		default:
			// Some other `{%` — could be a typo'd directive. Pass through;
			// html/template will reject it with a parse error and the user
			// will see exactly what was wrong.
			out.WriteByte(src[i])
			i++
		}
	}

	return out.Bytes(), masterName, sqls, bodies, nil
}

// readQuotedTagArg reads `{% tagName "value" %}` starting at start.
// Returns the offset just past the closing `%}`, the unquoted value,
// and an error if the syntax doesn't match.
func readQuotedTagArg(src []byte, start int, tagName string) (int, string, error) {
	prefix := []byte("{% " + tagName + " ")
	if !bytes.HasPrefix(src[start:], prefix) {
		return 0, "", fmt.Errorf("expected %q at offset %d", string(prefix), start)
	}
	j := start + len(prefix)
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	if j >= len(src) || src[j] != '"' {
		return 0, "", fmt.Errorf("%%s argument missing opening quote at offset %d", j)
	}
	j++
	valStart := j
	for j < len(src) && src[j] != '"' {
		j++
	}
	if j >= len(src) {
		return 0, "", fmt.Errorf("unterminated string in {%% %s %%} at offset %d", tagName, start)
	}
	val := string(src[valStart:j])
	j++ // past closing quote
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	if j+1 >= len(src) || src[j] != '%' || src[j+1] != '}' {
		return 0, "", fmt.Errorf("expected %%} closing {%% %s %%} at offset %d", tagName, start)
	}
	return j + 2, val, nil
}

// ScanAlias scans the leading bytes of a page source for a single
//
//	{% alias "X" %}
//
// declaration and returns X. Returns "" if no alias tag is present in
// the first 1 KiB. Used by the build's dir walk to discover URL
// aliases without parsing/rendering the page.
func ScanAlias(src []byte) string {
	limit := len(src)
	if limit > 1024 {
		limit = 1024
	}
	hay := src[:limit]
	const open = "{% alias "
	idx := bytes.Index(hay, []byte(open))
	if idx < 0 {
		return ""
	}
	j := idx + len(open)
	for j < len(hay) && (hay[j] == ' ' || hay[j] == '\t') {
		j++
	}
	if j >= len(hay) || hay[j] != '"' {
		return ""
	}
	j++
	start := j
	for j < len(hay) && hay[j] != '"' {
		j++
	}
	if j >= len(hay) {
		return ""
	}
	return string(hay[start:j])
}

// PageRenderer owns the master-template set parsed from <masterRoot>/.
// Each top-level dir under masterRoot is one master (e.g. "def",
// "premium", "sidebar"); its index.html is parsed and registered in the
// shared Template set. Page renders Clone() the set so each page can
// add its own block overrides without polluting the shared state.
type PageRenderer struct {
	masterSet *template.Template
}

// NewPageRenderer parses every <masterRoot>/<name>/index.html and
// registers each under the directory name. Returns an empty renderer
// (with a working set) if masterRoot doesn't exist — caller will get a
// nice "template X not defined" error from html/template later if a
// page references a missing master.
func NewPageRenderer(masterRoot string) *PageRenderer {
	root := template.New("").Funcs(builtinFuncs())
	entries, err := os.ReadDir(masterRoot)
	if err != nil {
		return &PageRenderer{masterSet: root}
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		idxPath := filepath.Join(masterRoot, entry.Name(), "index.html")
		src, rerr := os.ReadFile(idxPath)
		if rerr != nil {
			continue
		}
		// Each master file gets parsed into the set under its directory
		// name. Within the file users declare blocks via
		// `{{ block "head" . }}…{{ end }}` etc. We also auto-wrap the
		// file contents in a `{{ define "<name>" }}…{{ end }}` if it
		// isn't already, so ExecuteTemplate(name, data) works.
		body := autoWrapDefine(src, entry.Name())
		if _, perr := root.New(entry.Name()).Parse(string(body)); perr != nil {
			// Skip masters that fail to parse — the user will see the
			// error when they hit a page that extends one.
			continue
		}
	}
	return &PageRenderer{masterSet: root}
}

// autoWrapDefine ensures the file is structured as
// `{{ define "<name>" }}…{{ end }}` so ExecuteTemplate can name it
// directly. If the file already starts with a `{{ define }}` it's
// passed through unchanged.
func autoWrapDefine(src []byte, name string) []byte {
	trimmed := bytes.TrimLeftFunc(src, isWhitespace)
	if bytes.HasPrefix(trimmed, []byte("{{ define ")) || bytes.HasPrefix(trimmed, []byte("{{define ")) {
		return src
	}
	var out bytes.Buffer
	out.Grow(len(src) + 40)
	out.WriteString(`{{ define "`)
	out.WriteString(name)
	out.WriteString("\" }}\n")
	out.Write(src)
	out.WriteString("\n{{ end }}\n")
	return out.Bytes()
}

func isWhitespace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

// builtinFuncs is the FuncMap available to every template. Keep this
// list small — apps that want more register their own via a wrapper.
//
// `hole` emits the marker the build later scans for to record per-row
// data insertion points. With text/template all output is raw bytes by
// default, so the marker emits as a plain string.
func builtinFuncs() template.FuncMap {
	return template.FuncMap{
		"hole": func(idx int) string {
			return holeMarkerPrefix + strconv.Itoa(idx) + holeMarkerSuffix
		},
	}
}

// RenderPage takes a page's content.html bytes plus a data context,
// extracts our directives, parses the remainder as Go templates, and
// renders to a byte slice with hole markers scanned out and recorded.
func (pr *PageRenderer) RenderPage(source []byte, data map[string]any) ([]byte, []Hole, error) {
	cleanSrc, masterName, sqls, bodies, err := extractDirectives(source)
	if err != nil {
		return nil, nil, fmt.Errorf("extract directives: %w", err)
	}

	// Clone master set so we don't mutate the shared one across pages.
	tpl, err := pr.masterSet.Clone()
	if err != nil {
		return nil, nil, fmt.Errorf("clone master set: %w", err)
	}
	// Parse the page source. Each `{{ define "block" }}…{{ end }}` in the
	// page lands in the set and overrides the master's default block.
	if _, err := tpl.Parse(string(cleanSrc)); err != nil {
		return nil, nil, fmt.Errorf("parse page: %w", err)
	}

	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, masterName, data); err != nil {
		return nil, nil, fmt.Errorf("render (master=%q): %w", masterName, err)
	}

	clean, holes, err := scanHoleMarkers(buf.Bytes(), sqls, bodies)
	if err != nil {
		return nil, nil, err
	}
	return clean, holes, nil
}

// scanHoleMarkers walks the rendered body, finds every
// `\x02..HOLE_<idx>_HOLE..\x02` sentinel, builds a list of (position,
// sql, body) records, and returns the body with markers stripped.
func scanHoleMarkers(body []byte, sqls []string, bodies [][]byte) ([]byte, []Hole, error) {
	prefix := []byte(holeMarkerPrefix)
	suffix := []byte(holeMarkerSuffix)

	var holes []Hole
	var clean bytes.Buffer
	clean.Grow(len(body))
	cursor := 0
	for {
		i := bytes.Index(body[cursor:], prefix)
		if i < 0 {
			clean.Write(body[cursor:])
			break
		}
		i += cursor
		clean.Write(body[cursor:i])

		idxStart := i + len(prefix)
		j := bytes.Index(body[idxStart:], suffix)
		if j < 0 {
			return nil, nil, fmt.Errorf("unterminated hole marker at offset %d", i)
		}
		idxStr := string(body[idxStart : idxStart+j])
		cursor = idxStart + j + len(suffix)

		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 0 || idx >= len(sqls) {
			return nil, nil, fmt.Errorf("invalid hole marker index %q", idxStr)
		}

		holes = append(holes, Hole{
			Position: uint32(clean.Len()),
			SQL:      sqls[idx],
			Template: bodies[idx],
		})
	}
	return clean.Bytes(), holes, nil
}

// HelpersForHandlers exposes the runtime FuncMap for per-hole
// sub-template parsing. `hole` is a build-time concern; the runtime
// never needs to emit a marker because per-row rendering IS the marker
// target.
//
// Currently empty — text/template's own builtins (eq, lt, len, html,
// printf, …) are always available without registration. Apps that want
// additional funcs wrap this and add their own.
func HelpersForHandlers() template.FuncMap {
	return template.FuncMap{}
}

// EnsurePages — placeholder hook for keep-imports linter; non-critical.
var _ = strings.TrimSpace
