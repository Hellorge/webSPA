package router

import (
	"encoding/gob"
	"os"
	"time"
)

type Router struct {
	Root *RadixNode
}

type FileInfo struct {
	ModTime     time.Time `json:"ModTime"`
	DistPath    string    `json:"DistPath"`
	DependsOn   []string  `json:"DependsOn"`
	AliasedPath  string    `json:"AliasedPath"`
	BrotliPath   string    `json:"BrotliPath"`
	EmbeddedData []byte    `json:"EmbeddedData"` // For nanosecond lookup of small assets
}

type RadixNode struct {
	Children map[string]*RadixNode
	FileInfo *FileInfo
}

func New() *Router {
	return &Router{
		Root: &RadixNode{
			Children: make(map[string]*RadixNode),
		},
	}
}

func LoadFromBinary(binPath string) (*Router, error) {
	f, err := os.Open(binPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	root := &RadixNode{}
	dec := gob.NewDecoder(f)
	if err := dec.Decode(root); err != nil {
		return nil, err
	}

	return &Router{
		Root: root,
	}, nil
}

func (r *Router) Route(path string) (string, bool) {
	fileInfo := r.findRoute(path)
	if fileInfo == nil {
		return "", false
	}
	return fileInfo.DistPath, true
}

func (r *Router) RouteWithBrotli(path string) (string, string, []byte, bool) {
	fileInfo := r.findRoute(path)
	if fileInfo == nil {
		return "", "", nil, false
	}
	return fileInfo.DistPath, fileInfo.BrotliPath, fileInfo.EmbeddedData, true
}

func (r *Router) findRoute(path string) *FileInfo {
	node := r.Root
	// Optimization: empty path or just "/"
	if len(path) <= 1 {
		return node.FileInfo
	}

	start := 1
	for end := 1; end <= len(path); end++ {
		if end == len(path) || path[end] == '/' {
			child := node.Children[path[start:end]]
			if child == nil {
				return nil
			}
			node = child
			start = end + 1
		}
	}

	return node.FileInfo
}

func (n *RadixNode) Insert(segments []string, fileInfo *FileInfo) {
	current := n

	// Initialize children map if nil
	if current.Children == nil {
		current.Children = make(map[string]*RadixNode)
	}

	for i, segment := range segments {
		// O(1) map lookup
		matchingChild, found := current.Children[segment]

		// Create new node if no match found
		if !found {
			matchingChild = &RadixNode{
				Children: make(map[string]*RadixNode),
			}
			current.Children[segment] = matchingChild
		}

		// Move to next node
		current = matchingChild

		// If this is the last segment, store the file info
		if i == len(segments)-1 {
			current.FileInfo = fileInfo
		}
	}

	// Handle empty path or root case
	if len(segments) == 0 {
		current.FileInfo = fileInfo
	}
}
