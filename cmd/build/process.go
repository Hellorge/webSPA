package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gogogo/modules/actions"
	"gogogo/modules/build"
	"gogogo/modules/router"
)

// ProcessResult is what a worker emits per file. Empty/zero ProcessResult
// (Skip == true) means the file is metadata, not a route.
//
// FilePath is set for ActionFile entries — the on-disk path V2Compiler
// records in the route blob so the runtime can sendfile it.
type ProcessResult struct {
	FileInfo router.FileInfo
	Holes    []build.Hole
	FilePath string
	Skip     bool
}

// embedThresholdBytes — files larger than this go to dist/ and serve via
// ActionFile (sendfile from disk). Smaller files embed in the manifest.
// Override in cmd/build/main.go via the -embed-threshold flag.
var embedThresholdBytes int64 = 1 << 20 // 1 MiB

// processFile is the type-dispatched entry. One function decides per-file
// what to do based on the extension. No multi-stage walk, no per-stage
// guards. Each branch is end-to-end for that file type.
//
// Tier decision: files exceeding embedThresholdBytes go to dist/ and
// become ActionFile routes. HTML never disk-serves (templates need
// processing through pongo2).
func (w *Worker) processFile(item WorkItem) (ProcessResult, error) {
	body, err := os.ReadFile(item.Path)
	if err != nil {
		return ProcessResult{}, fmt.Errorf("read %s: %w", item.RelPath, err)
	}

	ext := strings.ToLower(filepath.Ext(item.Path))

	if int64(len(body)) >= embedThresholdBytes && ext != ".html" && ext != ".htm" {
		return w.processDiskFile(body, item)
	}

	switch ext {
	case ".html", ".htm":
		return w.processHTML(body, item)

	case ".css":
		out, err := w.ctx.minifier.Bytes("text/css", body)
		if err != nil {
			out = body
		}
		return staticResult(item, out), nil

	case ".js", ".mjs":
		out, err := w.ctx.minifier.Bytes("text/javascript", body)
		if err != nil {
			out = body
		}
		return staticResult(item, out), nil

	case ".json":
		return staticResult(item, body), nil

	default:
		return staticResult(item, body), nil
	}
}

// processDiskFile writes body to <distRoot>/<relPath> and returns an
// ActionFile ProcessResult pointing at that disk path. The runtime serves
// it via sendfile (no manifest payload, no in-memory copy). distRoot is
// per-site (dist/<siteName>) so collisions across sites are impossible.
func (w *Worker) processDiskFile(body []byte, item WorkItem) (ProcessResult, error) {
	distPath := filepath.Join(w.ctx.distRoot, filepath.FromSlash(item.RelPath))
	if err := os.MkdirAll(filepath.Dir(distPath), 0755); err != nil {
		return ProcessResult{}, fmt.Errorf("mkdir for %s: %w", distPath, err)
	}
	if err := os.WriteFile(distPath, body, 0644); err != nil {
		return ProcessResult{}, fmt.Errorf("write %s: %w", distPath, err)
	}
	return ProcessResult{
		FileInfo: router.FileInfo{
			ModTime:     item.Info.ModTime(),
			AliasedPath: item.AliasedPath,
			ActionID:    actions.ActionFile,
		},
		FilePath: distPath,
	}, nil
}

// processHTML hands the body to the pongo2 renderer with the hole tag
// wired up. All page metadata — alias, head, holes, page-local data —
// lives in the source itself; there is no sibling meta.toml.
//
// Pages aren't a hardcoded concept — any HTML file that uses
// {% extends %} gets layout inheritance for free. Files without extends
// render as standalone HTML.
func (w *Worker) processHTML(body []byte, item WorkItem) (ProcessResult, error) {
	urlPath := item.AliasedPath
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	urlPath = strings.TrimSuffix(urlPath, "/")
	if urlPath == "" {
		urlPath = "/"
	}

	ctx := map[string]any{
		"PagePath":  urlPath,
		"IsSPAMode": false,
		"StyleURL":  discoverSibling(item.Path, "style.css"),
		"ScriptURL": discoverSibling(item.Path, "script.js"),
	}

	rendered, holes, err := w.ctx.renderer.RenderPage(body, ctx)
	if err != nil {
		return ProcessResult{}, fmt.Errorf("render %s: %w", item.RelPath, err)
	}

	// Pages without holes get HTML-minified. Hole pages must NOT be
	// minified at this layer — hole.Position is a byte offset into the
	// rendered body and the minifier reorders/shrinks bytes, breaking the
	// runtime splice.
	final := rendered
	if len(holes) == 0 {
		if mini, mErr := w.ctx.minifier.Bytes("text/html", rendered); mErr == nil {
			final = mini
		}
	}

	return ProcessResult{
		FileInfo: router.FileInfo{
			ModTime:      item.Info.ModTime(),
			EmbeddedData: final,
			AliasedPath:  item.AliasedPath,
		},
		Holes: holes,
	}, nil
}

// staticResult wraps a non-templated file's bytes into a ProcessResult.
func staticResult(item WorkItem, body []byte) ProcessResult {
	return ProcessResult{
		FileInfo: router.FileInfo{
			ModTime:      item.Info.ModTime(),
			EmbeddedData: body,
			AliasedPath:  item.AliasedPath,
		},
	}
}

// discoverSibling returns the URL of a sibling asset like style.css or
// script.js if it exists next to the page. Returns "" otherwise.
func discoverSibling(pagePath, name string) string {
	siblingPath := filepath.Join(filepath.Dir(pagePath), name)
	if _, err := os.Stat(siblingPath); err != nil {
		return ""
	}
	return "/" + filepath.Join(filepath.Base(filepath.Dir(pagePath)), name)
}
