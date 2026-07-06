package router

import (
	"fmt"
	"strings"
)

// NodeKind discriminates trie node types. Literal nodes match by exact
// segment string; param nodes match any single segment and capture it;
// catchall nodes match the entire remaining path and capture it.
type NodeKind uint8

const (
	NodeLiteral  NodeKind = 0
	NodeParam    NodeKind = 1
	NodeCatchAll NodeKind = 2
)

// TrieNode is one node in the route-matching trie.
//
// Children are stored as indices into the Trie's flat Nodes array (not
// pointers) for cache locality when serialized into the manifest. Index
// 0 is reserved to mean "no child" — Trie.Nodes[0] is the root, never
// referenced as a child.
//
// Literal nodes carry the segment string they match. Param/catchall
// nodes carry the param name (e.g. "id" for ":id" → ParamName="id").
//
// RouteIdx is the leaf marker: 0 = non-leaf, otherwise (RouteIdx-1) is
// the index into the routes table this node resolves to. The +1 offset
// keeps zero-value-as-sentinel.
type TrieNode struct {
	Kind            NodeKind
	Label           string         // literal segment OR param name
	LiteralChildren map[string]int // segment string → node index
	ParamChild      int            // 0 = none
	CatchAllChild   int            // 0 = none
	RouteIdx        int            // 0 = non-leaf
}

// Trie holds the route-matching tree. Nodes[0] is the root.
type Trie struct {
	Nodes []TrieNode
}

// Param is one captured parameter from a successful match.
type Param struct {
	Name  string
	Value string
}

// NewTrie returns an empty trie with just the root node.
func NewTrie() *Trie {
	return &Trie{Nodes: []TrieNode{{Kind: NodeLiteral, LiteralChildren: map[string]int{}}}}
}

// Insert adds a route pattern to the trie, mapping it to the given
// route index. Pattern syntax:
//   - literal segments:    "users", "api/v1"
//   - param segments:      ":id"     (captures one segment)
//   - catch-all (terminal): "*path"  (captures rest of URL)
//
// Returns an error on conflicts:
//   - param name mismatch at the same position
//   - duplicate route (two routes ending at the same trie leaf)
//
// Mixing static + dynamic siblings is allowed (e.g. "/users/me" and
// "/users/:id" coexist; literal wins on overlap during lookup).
func (t *Trie) Insert(pattern string, routeIdx int) error {
	if routeIdx <= 0 {
		return fmt.Errorf("router: routeIdx must be >0 (got %d)", routeIdx)
	}
	segs := splitPath(pattern)
	cursor := 0 // start at root
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		isLast := i == len(segs)-1

		switch {
		case strings.HasPrefix(seg, ":"):
			// Param segment.
			name := seg[1:]
			if name == "" {
				return fmt.Errorf("router: empty param name in pattern %q", pattern)
			}
			if t.Nodes[cursor].ParamChild == 0 {
				newIdx := t.addNode(TrieNode{Kind: NodeParam, Label: name, LiteralChildren: map[string]int{}})
				t.Nodes[cursor].ParamChild = newIdx
				cursor = newIdx
			} else {
				existing := &t.Nodes[t.Nodes[cursor].ParamChild]
				if existing.Label != name {
					return fmt.Errorf("router: param name conflict at position in pattern %q: existing %q vs new %q",
						pattern, existing.Label, name)
				}
				cursor = t.Nodes[cursor].ParamChild
			}

		case strings.HasPrefix(seg, "*"):
			// Catch-all segment — must be terminal.
			if !isLast {
				return fmt.Errorf("router: catch-all %q must be the last segment in %q", seg, pattern)
			}
			name := seg[1:]
			if name == "" {
				return fmt.Errorf("router: empty catch-all name in pattern %q", pattern)
			}
			if t.Nodes[cursor].CatchAllChild != 0 {
				existing := &t.Nodes[t.Nodes[cursor].CatchAllChild]
				if existing.Label != name {
					return fmt.Errorf("router: catch-all name conflict at position in pattern %q", pattern)
				}
				cursor = t.Nodes[cursor].CatchAllChild
			} else {
				newIdx := t.addNode(TrieNode{Kind: NodeCatchAll, Label: name})
				t.Nodes[cursor].CatchAllChild = newIdx
				cursor = newIdx
			}

		default:
			// Literal segment.
			if existingIdx, ok := t.Nodes[cursor].LiteralChildren[seg]; ok {
				cursor = existingIdx
			} else {
				newIdx := t.addNode(TrieNode{Kind: NodeLiteral, Label: seg, LiteralChildren: map[string]int{}})
				t.Nodes[cursor].LiteralChildren[seg] = newIdx
				cursor = newIdx
			}
		}
	}

	// Mark cursor as the leaf for this routeIdx.
	if t.Nodes[cursor].RouteIdx != 0 {
		return fmt.Errorf("router: duplicate route at pattern %q (already mapped to routeIdx %d)",
			pattern, t.Nodes[cursor].RouteIdx-1)
	}
	t.Nodes[cursor].RouteIdx = routeIdx
	return nil
}

func (t *Trie) addNode(n TrieNode) int {
	t.Nodes = append(t.Nodes, n)
	return len(t.Nodes) - 1
}

// Match walks the trie for the given path and returns the route index
// (0-based) of the matched route, or -1 if none. Captured params are
// appended to *params; the caller is responsible for resetting it
// before calling.
//
// Lookup precedence at each node: literal > param > catch-all.
// Backtracking happens automatically — if a literal match leads to a
// dead end, we fall back to trying the param child.
func (t *Trie) Match(path string, params *[]Param) int {
	segs := splitPath(path)
	return t.walk(0, segs, 0, params)
}

func (t *Trie) walk(nodeIdx int, segs []string, depth int, params *[]Param) int {
	node := &t.Nodes[nodeIdx]

	// Reached the end of the path?
	if depth == len(segs) {
		if node.RouteIdx > 0 {
			return node.RouteIdx - 1
		}
		// Special case: a catch-all child can match an empty remainder
		// (so "/static/*path" matches "/static/" with path="").
		if node.CatchAllChild != 0 {
			caChild := &t.Nodes[node.CatchAllChild]
			if caChild.RouteIdx > 0 {
				*params = append(*params, Param{Name: caChild.Label, Value: ""})
				return caChild.RouteIdx - 1
			}
		}
		return -1
	}

	seg := segs[depth]

	// 1. Literal child.
	if childIdx, ok := node.LiteralChildren[seg]; ok {
		if r := t.walk(childIdx, segs, depth+1, params); r >= 0 {
			return r
		}
	}

	// 2. Param child.
	if node.ParamChild != 0 {
		child := &t.Nodes[node.ParamChild]
		*params = append(*params, Param{Name: child.Label, Value: seg})
		if r := t.walk(node.ParamChild, segs, depth+1, params); r >= 0 {
			return r
		}
		// Backtrack: drop the param we just appended.
		*params = (*params)[:len(*params)-1]
	}

	// 3. Catch-all child — captures everything from depth onward.
	if node.CatchAllChild != 0 {
		caChild := &t.Nodes[node.CatchAllChild]
		if caChild.RouteIdx > 0 {
			*params = append(*params, Param{Name: caChild.Label, Value: strings.Join(segs[depth:], "/")})
			return caChild.RouteIdx - 1
		}
	}

	return -1
}

// splitPath splits a URL path into segments, ignoring empty leading
// and trailing pieces. "/" → []. "/users/123" → ["users", "123"].
// "users/123/" → ["users", "123"].
func splitPath(path string) []string {
	if path == "" || path == "/" {
		return nil
	}
	if path[0] == '/' {
		path = path[1:]
	}
	if len(path) > 0 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}
