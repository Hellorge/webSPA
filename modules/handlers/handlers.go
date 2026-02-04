package handlers

import (
	"encoding/json"
	"html/template"
	"net/http"
	"path/filepath"
	"sync"

	"gogogo/modules/filemanager"
	"gogogo/modules/metaparser"
	"gogogo/modules/templates"
)

// Pre-computed paths
const (
	contentFile = "content.html"
	metaFile    = "meta.toml"
	styleFile   = "style.css"
	scriptFile  = "script.js"
)

var defaultMeta = &metaparser.MetaData{}

type PageData struct {
	content      []byte
	style        []byte
	script       []byte
	styleExists  string
	scriptExists string
	meta         *metaparser.MetaData
	err          error
}

type WebHandler struct {
	fm           *filemanager.FileManager
	engine       *templates.TemplateEngine
	contentPath  string
	SPAMode      bool
}

type SPAHandler struct {
	fm          *filemanager.FileManager
	contentPath string
	SPAMode     bool
}

type StaticHandler struct {
	fm *filemanager.FileManager
}

type APIHandler struct {
	fm          *filemanager.FileManager
	contentPath string
}

func NewWebHandler(fm *filemanager.FileManager, engine *templates.TemplateEngine, contentPath string, SPAMode bool) *WebHandler {
	return &WebHandler{
		fm:          fm,
		engine:      engine,
		contentPath: contentPath,
		SPAMode:     SPAMode,
	}
}

func NewSPAHandler(fm *filemanager.FileManager, contentPath string, SPAMode bool) *SPAHandler {
	return &SPAHandler{
		fm:          fm,
		contentPath: contentPath,
		SPAMode:     SPAMode,
	}
}

func NewStaticHandler(fm *filemanager.FileManager) *StaticHandler {
	return &StaticHandler{fm: fm}
}

func NewAPIHandler(fm *filemanager.FileManager, contentPath string) *APIHandler {
	return &APIHandler{
		fm:          fm,
		contentPath: contentPath,
	}
}

func loadContent(fm *filemanager.FileManager, dir string, path string) *PageData {
	// Simple aliasing for dev mode: map "/" to "home"
	if path == "/" || path == "" {
		path = "home"
	}

	contentPath := filepath.Join(dir, path, contentFile)
	metaPath := filepath.Join(dir, path, metaFile)
	stylePath := filepath.Join(dir, path, styleFile)
	scriptPath := filepath.Join(dir, path, scriptFile)

	pd := &PageData{
		meta: defaultMeta,
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		pd.content, pd.err = fm.GetContent(contentPath)
	}()

	go func() {
		defer wg.Done()
		metaContent, err := fm.GetContent(metaPath)
		if err == nil {
			if meta, err := metaparser.ParseMetaData(metaContent); err == nil {
				pd.meta = meta
			}
		}
	}()

	wg.Wait()
	if pd.err != nil {
		return pd
	}

	if pd.meta.InlineStyle {
		if style, err := fm.GetContent(stylePath); err == nil {
			pd.style = style
		}
	} else if fm.Exists(stylePath) {
		pd.styleExists = "/" + stylePath
	}

	if pd.meta.InlineScript {
		if script, err := fm.GetContent(scriptPath); err == nil {
			pd.script = script
		}
	} else if fm.Exists(scriptPath) {
		pd.scriptExists = "/" + scriptPath
	}

	return pd
}

func (h *WebHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	pc := loadContent(h.fm, h.contentPath, path)
	if pc.err != nil {
		http.NotFound(w, r)
		return
	}

	// HTTP/2 Push if available and files exist
	if pusher, ok := w.(http.Pusher); ok {
		if pc.styleExists != "" {
			pusher.Push(pc.styleExists, &http.PushOptions{
				Method: "GET",
				Header: http.Header{
					"Accept": []string{"text/css"},
				},
			})
		}
		if pc.scriptExists != "" {
			pusher.Push(pc.scriptExists, &http.PushOptions{
				Method: "GET",
				Header: http.Header{
					"Accept": []string{"application/javascript"},
				},
			})
		}
	}

	data := templates.RenderData{
		Meta:      pc.meta,
		Content:   template.HTML(pc.content),
		Style:     template.CSS(pc.style),
		Script:    template.JS(pc.script),
		StyleURL:  pc.styleExists,
		ScriptURL: pc.scriptExists,
		IsSPAMode: h.SPAMode,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.engine.Render(w, pc.meta.Template, data); err != nil {
		http.Error(w, "Error rendering template", http.StatusInternalServerError)
	}
}

func (h *SPAHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	pc := loadContent(h.fm, h.contentPath, path)
	if pc.err != nil {
		http.NotFound(w, r)
		return
	}

	// HTTP/2 Push if available and files exist
	if pusher, ok := w.(http.Pusher); ok {
		if pc.styleExists != "" {
			pusher.Push(pc.styleExists, &http.PushOptions{
				Method: "GET",
				Header: http.Header{
					"Accept": []string{"text/css"},
				},
			})
		}
		if pc.scriptExists != "" {
			pusher.Push(pc.scriptExists, &http.PushOptions{
				Method: "GET",
				Header: http.Header{
					"Accept": []string{"application/javascript"},
				},
			})
		}
	}

	// Reuse buffer for response
	resp := struct {
		Content   string               `json:"Content"`
		Style     string               `json:"Style,omitempty"`
		Script    string               `json:"Script,omitempty"`
		StyleURL  string               `json:"StyleUrl,omitempty"`
		ScriptURL string               `json:"ScriptUrl,omitempty"`
		Meta      *metaparser.MetaData `json:"Meta,omitempty"`
		IsSPAMode bool
	}{
		Meta:      pc.meta,
		Content:   string(pc.content),
		Style:     string(pc.style),
		Script:    string(pc.script),
		StyleURL:  pc.styleExists,
		ScriptURL: pc.scriptExists,
		IsSPAMode: h.SPAMode,
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.Encode(resp)
}

func (h *StaticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	file, err := h.fm.OpenFile(r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	http.ServeContent(w, r, r.URL.Path, info.ModTime(), file)
}

func (h *APIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path[5:] // strip /api/

	content, err := h.fm.GetContent(filepath.Join(h.contentPath, path, contentFile))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.Encode(map[string]string{"content": string(content)})
}
