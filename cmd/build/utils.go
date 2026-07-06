package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type ErrorCollector struct {
	mu     sync.Mutex
	errors []error
}

type IgnorePatterns struct {
	mu       sync.RWMutex
	patterns map[string][]string
}

var ignoreCache = &IgnorePatterns{
	patterns: make(map[string][]string),
}

func NewErrorCollector() *ErrorCollector {
	return &ErrorCollector{
		errors: make([]error, 0),
	}
}

func (ec *ErrorCollector) Add(err error) {
	if err == nil {
		return
	}
	ec.mu.Lock()
	ec.errors = append(ec.errors, err)
	ec.mu.Unlock()
}

func (ec *ErrorCollector) HasErrors() bool {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	return len(ec.errors) > 0
}

func (ec *ErrorCollector) Error() error {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	if len(ec.errors) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("build errors:\n")
	for _, err := range ec.errors {
		sb.WriteString("  - ")
		sb.WriteString(err.Error())
		sb.WriteString("\n")
	}
	return fmt.Errorf(sb.String())
}

func atomicWrite(filename string, data []byte) error {
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	tempFile := filename + ".tmp"
	if err := os.WriteFile(tempFile, data, 0644); err != nil {
		return err
	}

	return os.Rename(tempFile, filename)
}

func loadIgnorePatternsForDir(dir string) ([]string, error) {
	ignoreCache.mu.RLock()
	if patterns, ok := ignoreCache.patterns[dir]; ok {
		ignoreCache.mu.RUnlock()
		return patterns, nil
	}
	ignoreCache.mu.RUnlock()

	ignoreCache.mu.Lock()
	defer ignoreCache.mu.Unlock()

	// Double check after acquiring write lock
	if patterns, ok := ignoreCache.patterns[dir]; ok {
		return patterns, nil
	}

	ignoreFile := filepath.Join(dir, ".buildignore")
	patterns, err := readIgnoreFile(ignoreFile)
	if err != nil {
		return nil, err
	}

	ignoreCache.patterns[dir] = patterns
	return patterns, nil
}

func shouldIgnore(dirPath, filePath string) bool {
	patterns, err := loadIgnorePatternsForDir(dirPath)
	if err != nil {
		return false
	}

	base := filepath.Base(filePath)
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, base); matched {
			return true
		}
	}
	return false
}

func readIgnoreFile(ignoreFile string) ([]string, error) {
	file, err := os.Open(ignoreFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	var patterns []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		pattern := strings.TrimSpace(scanner.Text())
		if pattern != "" && !strings.HasPrefix(pattern, "#") {
			patterns = append(patterns, pattern)
		}
	}
	return patterns, scanner.Err()
}
