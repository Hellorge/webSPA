package templates

import (
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"gogogo/modules/filemanager"
	"gogogo/modules/metaparser"
)

type RenderData struct {
	Content   template.HTML
	Style     template.CSS
	Script    template.JS
	StyleURL  string
	ScriptURL string
	Meta      *metaparser.MetaData
	IsSPAMode bool
}

type TemplateEngine interface {
	Render(w io.Writer, name string, data RenderData) error
}

type ProductionEngine struct {
	chunks        map[string][]TemplateChunk
	defaultLayout string
}

type DevelopmentEngine struct {
	fm            *filemanager.FileManager
	dir           string
	defaultLayout string
	templates     map[string]*template.Template
	mu            sync.RWMutex
}

type TemplateChunk struct {
	Data    []byte
	IsVar   bool
	VarName string
}

func New(fm *filemanager.FileManager, dir string, defaultLayout string, productionMode bool) TemplateEngine {
	if productionMode {
		chunks := make(map[string][]TemplateChunk)
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && filepath.Ext(path) == ".html" {
				rel, _ := filepath.Rel(dir, path)
				name := rel
				// Build-time alignment: standardize name
				if filepath.Base(rel) == "index.html" {
					name = filepath.Dir(rel)
				}
				content, _ := os.ReadFile(path)
				chunks[name] = parseFast(string(content))
			}
			return nil
		})
		return &ProductionEngine{
			chunks:        chunks,
			defaultLayout: defaultLayout,
		}
	}

	return &DevelopmentEngine{
		fm:            fm,
		dir:           dir,
		defaultLayout: defaultLayout,
		templates:     make(map[string]*template.Template),
	}
}


func (e *ProductionEngine) Render(w io.Writer, name string, data RenderData) error {
	if name == "" {
		name = e.defaultLayout
	}
	chunks := e.chunks[name]
	
	if flusher, ok := w.(http.Flusher); ok {
		for _, chunk := range chunks {
			if chunk.IsVar {
				renderVar(w, chunk.VarName, data)
			} else {
				w.Write(chunk.Data)
			}
			flusher.Flush()
		}
		return nil
	}

	for _, chunk := range chunks {
		if chunk.IsVar {
			renderVar(w, chunk.VarName, data)
		} else {
			w.Write(chunk.Data)
		}
	}
	return nil
}

func (e *DevelopmentEngine) Render(w io.Writer, name string, data RenderData) error {
	if name == "" {
		name = e.defaultLayout
	}

	e.mu.RLock()
	tmpl, ok := e.templates[name]
	e.mu.RUnlock()

	if !ok {
		e.mu.Lock()
		path := filepath.Join(e.dir, name, "index.html")
		content, _ := e.fm.GetContent(path)
		tmpl, _ = template.New(name).Parse(string(content))
		e.templates[name] = tmpl
		e.mu.Unlock()
	}

	return tmpl.Execute(w, data)
}

func renderVar(w io.Writer, name string, data RenderData) {
	switch name {
	case "Content":
		w.Write([]byte(data.Content))
	case "Style":
		w.Write([]byte(data.Style))
	case "Script":
		w.Write([]byte(data.Script))
	case "StyleURL":
		w.Write([]byte(data.StyleURL))
	case "ScriptURL":
		w.Write([]byte(data.ScriptURL))
	case "IsSPAMode":
		if data.IsSPAMode {
			w.Write([]byte("true"))
		} else {
			w.Write([]byte("false"))
		}
	}
}


var varRegex = regexp.MustCompile(`\{\{\s*\.([a-zA-Z0-9]+)\s*\}\}`)

func parseFast(content string) []TemplateChunk {
	var chunks []TemplateChunk
	matches := varRegex.FindAllStringSubmatchIndex(content, -1)

	lastEnd := 0
	for _, match := range matches {
		if match[0] > lastEnd {
			chunks = append(chunks, TemplateChunk{
				Data: []byte(content[lastEnd:match[0]]),
			})
		}

		varName := content[match[2]:match[3]]
		chunks = append(chunks, TemplateChunk{
			IsVar:   true,
			VarName: varName,
		})

		lastEnd = match[1]
	}

	if lastEnd < len(content) {
		chunks = append(chunks, TemplateChunk{
			Data: []byte(content[lastEnd:]),
		})
	}
	return chunks
}
