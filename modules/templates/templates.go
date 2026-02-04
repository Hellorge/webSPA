package templates

import (
	"fmt"
	"html/template"
	"io"
	"path/filepath"
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

type TemplateEngine struct {
	templates      map[string]*template.Template
	templateMutex  sync.RWMutex
	fm             *filemanager.FileManager
	dir            string
	defaultLayout string
	GetTemplate    func(string) (*template.Template, error)
}

func New(fm *filemanager.FileManager, dir string, defaultLayout string, productionMode bool) *TemplateEngine {
	t := &TemplateEngine{
		templates:     make(map[string]*template.Template),
		fm:            fm,
		dir:           dir,
		defaultLayout: defaultLayout,
	}

	if productionMode {
		t.GetTemplate = t.getProduction
	} else {
		t.GetTemplate = t.getDevelopment
	}

	return t
}

func (t *TemplateEngine) getProduction(name string) (*template.Template, error) {
	t.templateMutex.RLock()
	tmpl, exists := t.templates[name]
	t.templateMutex.RUnlock()
	if exists {
		return tmpl, nil
	}

	t.templateMutex.Lock()
	defer t.templateMutex.Unlock()
	if tmpl, exists = t.templates[name]; exists {
		return tmpl, nil
	}

	if name == "" {
		name = t.defaultLayout
	}

	// Slow path - load and parse template
	path := filepath.Join(t.dir, name, "index.html")
	content, err := t.fm.GetContent(path)
	if err != nil {
		return nil, fmt.Errorf("error reading template file: %w", err)
	}

	tmpl, err = template.New(filepath.Base(name)).Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("error parsing template: %w", err)
	}

	// Store in sync.Map (handles race conditions internally)
	t.templates[name] = tmpl

	return tmpl, nil
}

func (t *TemplateEngine) getDevelopment(name string) (*template.Template, error) {
	if name == "" {
		name = t.defaultLayout
	}
	path := filepath.Join(t.dir, name, "index.html")
	content, err := t.fm.GetContent(path)
	if err != nil {
		return nil, fmt.Errorf("error reading template file %s: %w", path, err)
	}

	return template.New(filepath.Base(name)).Parse(string(content))
}

func (t *TemplateEngine) Render(w io.Writer, name string, data RenderData) error {
	tmpl, err := t.GetTemplate(name)
	if err != nil {
		return err
	}
	return tmpl.Execute(w, data)
}
