package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gogogo/modules/studio"
)

// buildStudioManifest emits the studio site's manifest. Studio is small —
// one HTML page baked as a static route at "" plus its dynamic action
// routes — so we use V2Compiler's direct API rather than the full pipeline.
//
// Studio's content tree could grow (extra pages, JS bundles) without
// changing this code: just add files under modules/studio/content and
// extend the loop below to walk that directory. Keeping it minimal until
// that's actually needed.
func buildStudioManifest(outPath string) error {
	indexPath := filepath.Join(studio.ContentDir, "index.html")
	indexBytes, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("read studio index: %w", err)
	}

	c := NewV2Compiler()
	c.AddStaticRoute("", indexBytes, "text/html; charset=utf-8")
	for _, route := range studio.Routes {
		c.AddActionRoute(route.Path, route.Method, route.Middleware, route.ActionID)
	}

	if err := c.Write(outPath); err != nil {
		return err
	}
	fmt.Printf("Studio manifest: %d routes (1 static + %d action) → %s\n",
		1+len(studio.Routes), len(studio.Routes), outPath)
	return nil
}
