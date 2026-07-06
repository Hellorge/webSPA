package router

import (
	"testing"
)

// trieFromPatterns builds a trie with one entry per pattern, using
// (i+1) as the routeIdx for the i-th pattern. Returns the trie or
// fails the test with t.Fatal if Insert errors.
func trieFromPatterns(t *testing.T, patterns []string) *Trie {
	t.Helper()
	tr := NewTrie()
	for i, p := range patterns {
		if err := tr.Insert(p, i+1); err != nil {
			t.Fatalf("Insert %q: %v", p, err)
		}
	}
	return tr
}

func TestTrie_StaticOnly(t *testing.T) {
	tr := trieFromPatterns(t, []string{
		"/", "/about", "/showcase", "/api/v1/health",
	})

	cases := []struct {
		path    string
		wantIdx int
	}{
		{"/", 0},
		{"/about", 1},
		{"/showcase", 2},
		{"/api/v1/health", 3},
		{"/missing", -1},
		{"/api", -1},
		{"/api/v2/health", -1},
	}
	for _, c := range cases {
		var params []Param
		got := tr.Match(c.path, &params)
		if got != c.wantIdx {
			t.Errorf("Match(%q) = %d, want %d", c.path, got, c.wantIdx)
		}
		if len(params) != 0 {
			t.Errorf("Match(%q): unexpected params %v", c.path, params)
		}
	}
}

func TestTrie_SingleParam(t *testing.T) {
	tr := trieFromPatterns(t, []string{
		"/users/:id",
		"/posts/:slug",
	})

	cases := []struct {
		path     string
		wantIdx  int
		wantName string
		wantVal  string
	}{
		{"/users/123", 0, "id", "123"},
		{"/users/abc", 0, "id", "abc"},
		{"/posts/hello-world", 1, "slug", "hello-world"},
		{"/users", -1, "", ""},
		{"/users/123/extra", -1, "", ""},
	}
	for _, c := range cases {
		var params []Param
		got := tr.Match(c.path, &params)
		if got != c.wantIdx {
			t.Errorf("Match(%q) = %d, want %d", c.path, got, c.wantIdx)
			continue
		}
		if c.wantIdx == -1 {
			continue
		}
		if len(params) != 1 || params[0].Name != c.wantName || params[0].Value != c.wantVal {
			t.Errorf("Match(%q): params=%v, want [{%s %s}]", c.path, params, c.wantName, c.wantVal)
		}
	}
}

func TestTrie_StaticBeatsParam(t *testing.T) {
	// /users/me must win over /users/:id when path is /users/me.
	tr := trieFromPatterns(t, []string{
		"/users/me",  // idx 0
		"/users/:id", // idx 1
	})

	var params []Param

	got := tr.Match("/users/me", &params)
	if got != 0 {
		t.Errorf("Match(/users/me) = %d, want 0 (literal)", got)
	}
	if len(params) != 0 {
		t.Errorf("Match(/users/me): unexpected params %v", params)
	}

	params = params[:0]
	got = tr.Match("/users/123", &params)
	if got != 1 {
		t.Errorf("Match(/users/123) = %d, want 1 (param)", got)
	}
	if len(params) != 1 || params[0].Value != "123" {
		t.Errorf("Match(/users/123): params=%v", params)
	}
}

func TestTrie_NestedParams(t *testing.T) {
	tr := trieFromPatterns(t, []string{
		"/:user/collections/:collection_id",
	})

	var params []Param
	got := tr.Match("/alice/collections/42", &params)
	if got != 0 {
		t.Errorf("got idx %d, want 0", got)
	}
	if len(params) != 2 ||
		params[0].Name != "user" || params[0].Value != "alice" ||
		params[1].Name != "collection_id" || params[1].Value != "42" {
		t.Errorf("params = %v", params)
	}
}

func TestTrie_CatchAll(t *testing.T) {
	tr := trieFromPatterns(t, []string{
		"/static/*path",
	})

	cases := []struct {
		path    string
		wantIdx int
		wantVal string
	}{
		{"/static/a", 0, "a"},
		{"/static/css/main.css", 0, "css/main.css"},
		{"/static/deep/nested/file.png", 0, "deep/nested/file.png"},
		{"/static/", 0, ""},
		{"/other/foo", -1, ""},
	}
	for _, c := range cases {
		var params []Param
		got := tr.Match(c.path, &params)
		if got != c.wantIdx {
			t.Errorf("Match(%q) = %d, want %d", c.path, got, c.wantIdx)
			continue
		}
		if c.wantIdx == -1 {
			continue
		}
		if len(params) != 1 || params[0].Name != "path" || params[0].Value != c.wantVal {
			t.Errorf("Match(%q): params=%v, want [{path %q}]", c.path, params, c.wantVal)
		}
	}
}

func TestTrie_StaticBeatsParamBeatsCatchAll(t *testing.T) {
	// All three at the same parent. Precedence: literal > param > catchall.
	tr := trieFromPatterns(t, []string{
		"/x/foo",  // idx 0
		"/x/:p",   // idx 1
		"/x/*all", // idx 2
	})

	cases := []struct {
		path    string
		wantIdx int
	}{
		{"/x/foo", 0},     // literal wins
		{"/x/bar", 1},     // param wins (single segment)
		{"/x/a/b/c", 2},   // catch-all wins (multi segment)
	}
	for _, c := range cases {
		var params []Param
		got := tr.Match(c.path, &params)
		if got != c.wantIdx {
			t.Errorf("Match(%q) = %d, want %d", c.path, got, c.wantIdx)
		}
	}
}

func TestTrie_MixedRealistic(t *testing.T) {
	// A realistic mix: static pages + CRUD API + admin path.
	tr := trieFromPatterns(t, []string{
		"/",                       // 0  (root)
		"/about",                  // 1  (static)
		"/api/posts",              // 2  (list)
		"/api/posts/:id",          // 3  (get one)
		"/api/posts/:id/comments", // 4  (nested under param)
		"/admin/dashboard",        // 5  (literal)
		"/admin/:section",         // 6  (param)
		"/static/*path",           // 7  (catch-all)
	})

	cases := []struct {
		path    string
		wantIdx int
		params  []Param
	}{
		{"/", 0, nil},
		{"/about", 1, nil},
		{"/api/posts", 2, nil},
		{"/api/posts/123", 3, []Param{{"id", "123"}}},
		{"/api/posts/abc/comments", 4, []Param{{"id", "abc"}}},
		{"/admin/dashboard", 5, nil},
		{"/admin/users", 6, []Param{{"section", "users"}}},
		{"/static/css/x.css", 7, []Param{{"path", "css/x.css"}}},
		{"/missing", -1, nil},
	}
	for _, c := range cases {
		var params []Param
		got := tr.Match(c.path, &params)
		if got != c.wantIdx {
			t.Errorf("Match(%q) = %d, want %d", c.path, got, c.wantIdx)
			continue
		}
		if len(params) != len(c.params) {
			t.Errorf("Match(%q): got %d params, want %d (got=%v)", c.path, len(params), len(c.params), params)
			continue
		}
		for i := range params {
			if params[i] != c.params[i] {
				t.Errorf("Match(%q): param[%d] = %v, want %v", c.path, i, params[i], c.params[i])
			}
		}
	}
}

func TestTrie_ParamConflict(t *testing.T) {
	tr := NewTrie()
	if err := tr.Insert("/users/:id", 1); err != nil {
		t.Fatalf("first insert failed: %v", err)
	}
	if err := tr.Insert("/users/:slug", 2); err == nil {
		t.Errorf("expected param-name conflict, got no error")
	}
}

func TestTrie_DuplicateRoute(t *testing.T) {
	tr := NewTrie()
	if err := tr.Insert("/about", 1); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := tr.Insert("/about", 2); err == nil {
		t.Errorf("expected duplicate-route error, got nil")
	}
}

func TestTrie_BacktrackOnDeadEnd(t *testing.T) {
	// Backtracking: literal "/users/me" matches /users/me, but
	// /users/admin must fall back to the param child.
	tr := trieFromPatterns(t, []string{
		"/users/me/profile", // 0
		"/users/:id/info",   // 1
	})

	var params []Param
	got := tr.Match("/users/admin/info", &params)
	if got != 1 {
		t.Errorf("backtrack failed: got idx %d, want 1", got)
	}
	if len(params) != 1 || params[0].Value != "admin" {
		t.Errorf("backtrack params: got %v", params)
	}

	// Sanity: /users/me/profile is still found via literal.
	params = params[:0]
	got = tr.Match("/users/me/profile", &params)
	if got != 0 {
		t.Errorf("literal path: got idx %d, want 0", got)
	}
	if len(params) != 0 {
		t.Errorf("literal path: unexpected params %v", params)
	}
}
