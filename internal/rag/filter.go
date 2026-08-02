package rag

import (
	"path/filepath"
	"strings"
)

// FileFilter controls which files are eligible for RAG indexing.
// A nil *FileFilter allows all files (backward compatible).
//
// Two independent mechanisms are supported:
//
//   - Include extensions (allowlist): when non-empty, only files whose
//     extension matches one of the configured extensions are indexed.
//     Example: [".go", ".py", ".ts"].
//
//   - Exclude patterns (denylist): files whose base name matches any
//     glob pattern are skipped. Example: ["*_test.go", "*.gen.go"].
//
// A file must pass both checks to be indexed: it must have an allowed
// extension (when the allowlist is non-empty) AND must not match any
// exclude pattern.
type FileFilter struct {
	extensions      map[string]bool // lowercase ".go", ".py", etc. nil/empty = allow all
	excludePatterns []string        // glob patterns matched against base name
}

// NewFileFilter creates a filter from include extensions and exclude
// patterns. Both slices accept nil/empty for permissive behaviour:
//   - Empty extensions → all extensions allowed
//   - Empty excludePatterns → no files excluded by pattern
//
// Extensions are normalised: leading dots are added if missing, and all
// are lowercased for case-insensitive comparison.
func NewFileFilter(extensions, excludePatterns []string) *FileFilter {
	f := &FileFilter{}
	if len(extensions) > 0 {
		f.extensions = make(map[string]bool, len(extensions))
		for _, ext := range extensions {
			ext = strings.TrimSpace(ext)
			if ext == "" {
				continue
			}
			if !strings.HasPrefix(ext, ".") {
				ext = "." + ext
			}
			f.extensions[strings.ToLower(ext)] = true
		}
	}
	for _, p := range excludePatterns {
		p = strings.TrimSpace(p)
		if p != "" {
			f.excludePatterns = append(f.excludePatterns, p)
		}
	}
	return f
}

// ShouldIndex returns true when the file name passes both the extension
// allowlist and the exclude-pattern denylist. A nil filter allows
// everything (backward compatible with pre-issue-#1148 behaviour).
func (f *FileFilter) ShouldIndex(name string) bool {
	if f == nil {
		return true
	}
	if len(f.extensions) > 0 {
		ext := strings.ToLower(filepath.Ext(name))
		if ext == "" || !f.extensions[ext] {
			return false
		}
	}
	for _, pattern := range f.excludePatterns {
		if matched, _ := filepath.Match(pattern, name); matched {
			return false
		}
	}
	return true
}

// ParseCommaSeparated splits a comma-separated string into trimmed,
// non-empty fields. Returns nil for an empty input.
func ParseCommaSeparated(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
