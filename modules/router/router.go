package router

import "time"

// FileInfo carries per-file metadata between build pipeline stages and the
// build cache. The runtime never reads this — the runtime works off
// RouteEntry / Variant / Manifest in v2_binary.go. FileInfo lives here
// because the build cache uses JSON-serialized FileInfo as its on-disk
// format and the runtime router types share this package.
type FileInfo struct {
	ModTime      time.Time `json:"ModTime"`
	AliasedPath  string    `json:"AliasedPath"`
	EmbeddedData []byte    `json:"EmbeddedData"`
	ActionID     uint16    `json:"ActionID"`
}
