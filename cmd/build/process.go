package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"html/template"

	"fmt"
	"gogogo/modules/metaparser"
	"gogogo/modules/router"
	"gogogo/modules/templates"
	"os"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

type ProcessResult struct {
	FileInfo     router.FileInfo
	Content      []byte
	Hash         string
	Dependencies []string
}

func (w *Worker) processFile(item WorkItem) (ProcessResult, error) {
	if w.ctx.dryRun {
		return ProcessResult{
			FileInfo: router.FileInfo{
				ModTime:  item.Info.ModTime(),
				DistPath: filepath.Join(w.ctx.config.Directories.Dist, item.AliasedPath),
			},
		}, nil
	}

	content, err := os.ReadFile(item.Path)
	if err != nil {
		return ProcessResult{}, fmt.Errorf("error reading file: %w", err)
	}

	hash := md5.Sum(content)
	hashString := hex.EncodeToString(hash[:])

	if entry, ok := w.ctx.cache.Get(item.RelPath); ok && entry.Hash == hashString {
		return ProcessResult{
			FileInfo: router.FileInfo{
				ModTime:   item.Info.ModTime(),
				DistPath:  entry.FileInfo.DistPath,
				DependsOn: findDependencies(content),
			},
			Content:      content, // Content is already loaded above
			Hash:         entry.Hash,
			Dependencies: findDependencies(content),
		}, nil
	}

	if filepath.Base(item.Path) == "content.html" {
		return w.processContentHTML(item, content, hashString)
	}

	ext := filepath.Ext(item.Path)
	var mimeType string
	switch ext {
	case ".html":
		mimeType = "text/html"
	case ".css":
		mimeType = "text/css"
	case ".js":
		mimeType = "text/javascript"
	}

	var minified []byte
	if ext == ".js" {
		result := api.Transform(string(content), api.TransformOptions{
			Loader:            api.LoaderJS,
			MinifyWhitespace:  true,
			MinifyIdentifiers: true,
			MinifySyntax:      true,
			Sourcemap:         api.SourceMapInline,
		})
		if len(result.Errors) > 0 {
			return ProcessResult{}, fmt.Errorf("error minifying: %v", result.Errors)
		}
		minified = result.Code
	} else if mimeType != "" {
		var err error
		minified, err = w.ctx.minifier.Bytes(mimeType, content)
		if err != nil {
			return ProcessResult{}, fmt.Errorf("error minifying: %w", err)
		}
	} else {
		minified = content
	}

	minifiedHash := md5.Sum(minified)
	fileName := fmt.Sprintf("%s.%s%s",
		strings.TrimSuffix(filepath.Base(item.Path), ext),
		hex.EncodeToString(minifiedHash[:])[:8],
		ext,
	)

	relDir := filepath.Dir(item.RelPath)
	outPath := filepath.Join(w.ctx.outputDir, relDir, fileName)

	if err := atomicWrite(outPath, minified); err != nil {
		return ProcessResult{}, fmt.Errorf("error writing file: %w", err)
	}

	deps := findDependencies(content)
	return ProcessResult{
		FileInfo: router.FileInfo{
			ModTime:   item.Info.ModTime(),
			DistPath:  outPath,
			DependsOn: deps,
		},
		Content:      minified,
		Hash:         hashString,
		Dependencies: deps,
	}, nil
}

func findDependencies(content []byte) []string {
	var deps []string
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "import") || strings.Contains(line, "require") {
			if dep := extractDependency(line); dep != "" {
				deps = append(deps, dep)
			}
		}
	}
	return deps
}

func extractDependency(line string) string {
	line = strings.TrimSpace(line)
	if idx := strings.Index(line, "from"); idx != -1 {
		line = line[idx+4:]
	}
	line = strings.Trim(line, "'\"`;")
	if line != "" && !strings.HasPrefix(line, ".") {
		return line
	}
	return ""
}

// processContentHTML handles content.html files by pre-rendering complete HTML pages
func (w *Worker) processContentHTML(item WorkItem, content []byte, hashString string) (ProcessResult, error) {
	// Determine relative page path for URL formation
	contentRoot := filepath.Join(w.ctx.config.Directories.Web, w.ctx.config.Directories.Content)
	parentDir := filepath.Dir(item.Path)
	pagePath, err := filepath.Rel(contentRoot, parentDir)
	if err != nil {
		return ProcessResult{}, fmt.Errorf("error getting relative path: %w", err)
	}

	// Load meta, style, and script files
	metaPath := filepath.Join(parentDir, "meta.toml")
	stylePath := filepath.Join(parentDir, "style.css")
	scriptPath := filepath.Join(parentDir, "script.js")

	// Initialize page data
	pd := struct {
		content      []byte
		style        []byte
		script       []byte
		styleExists  string
		scriptExists string
		meta         *metaparser.MetaData
		err          error
	}{
		content: content,
		meta:    &metaparser.MetaData{},
	}

	// Load meta.toml
	metaContent, err := os.ReadFile(metaPath)
	if err == nil {
		if meta, err := metaparser.ParseMetaData(metaContent); err == nil {
			pd.meta = meta
		}
	}

	// Process style
	if pd.meta.InlineStyle {
		style, err := os.ReadFile(stylePath)
		if err == nil {
			pd.style = style
		}
	} else if _, err := os.Stat(stylePath); err == nil {
		// Create style URL relative to static path
		pd.styleExists = fmt.Sprintf("/static/%s/style.css", pagePath)
	}

	// Process script
	if pd.meta.InlineScript {
		script, err := os.ReadFile(scriptPath)
		if err == nil {
			pd.script = script
		}
	} else if _, err := os.Stat(scriptPath); err == nil {
		// Create script URL relative to static path
		pd.scriptExists = fmt.Sprintf("/static/%s/script.js", pagePath)
	}

	// Prepare template data
	data := templates.RenderData{
		Content:   template.HTML(pd.content),
		Style:     template.CSS(pd.style),
		Script:    template.JS(pd.script),
		StyleURL:  pd.styleExists,
		ScriptURL: pd.scriptExists,
		Meta:      pd.meta,
		IsSPAMode: false, // Always false for pre-rendered HTML
	}

	// Execute template using unified engine
	var buf bytes.Buffer
	if err := w.ctx.templateEngine.Render(&buf, pd.meta.Template, data); err != nil {
		return ProcessResult{}, fmt.Errorf("error executing template: %w", err)
	}

	// Minify the resulting HTML
	renderedHTML := buf.Bytes()
	minified, err := w.ctx.minifier.Bytes("text/html", renderedHTML)
	if err != nil {
		return ProcessResult{}, fmt.Errorf("error minifying HTML: %w", err)
	}

	ext := filepath.Ext(item.Path)
	minifiedHash := md5.Sum(minified)
	fileName := fmt.Sprintf("%s.%s%s",
		strings.TrimSuffix(filepath.Base(item.Path), ext),
		hex.EncodeToString(minifiedHash[:])[:8],
		ext,
	)

	relDir := filepath.Dir(item.RelPath)
	outPath := filepath.Join(w.ctx.outputDir, relDir, fileName)

	// Write the pre-rendered HTML file
	if err := atomicWrite(outPath, minified); err != nil {
		return ProcessResult{}, fmt.Errorf("error writing HTML file: %w", err)
	}

	// Regular return for the router
	return ProcessResult{
		FileInfo: router.FileInfo{
			ModTime:   item.Info.ModTime(),
			DistPath:  outPath,
			DependsOn: []string{}, // Pre-rendered HTML doesn't need dependencies
		},
		Content:      minified,
		Hash:         hashString,
		Dependencies: []string{},
	}, nil
}
