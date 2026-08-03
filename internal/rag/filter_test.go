package rag

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileFilter_NilAllowsAll(t *testing.T) {
	t.Parallel()
	var f *FileFilter
	tests := []string{"foo.go", "bar.py", "image.png", "binary", ".env", "script.sh"}
	for _, name := range tests {
		if !f.ShouldIndex(name) {
			t.Errorf("nil filter should allow %q", name)
		}
	}
}

func TestFileFilter_EmptyAllowsAll(t *testing.T) {
	t.Parallel()
	f := NewFileFilter(nil, nil)
	tests := []string{"foo.go", "bar.py", "image.png", "binary", ".env"}
	for _, name := range tests {
		if !f.ShouldIndex(name) {
			t.Errorf("empty filter should allow %q", name)
		}
	}
}

func TestFileFilter_ExtensionAllowlist(t *testing.T) {
	t.Parallel()
	f := NewFileFilter([]string{".go", ".py", ".ts"}, nil)

	allowed := []string{"main.go", "utils.py", "app.ts"}
	for _, name := range allowed {
		if !f.ShouldIndex(name) {
			t.Errorf("ShouldIndex(%q) = false, want true", name)
		}
	}

	blocked := []string{"image.png", "data.json", "readme.md", "binary"}
	for _, name := range blocked {
		if f.ShouldIndex(name) {
			t.Errorf("ShouldIndex(%q) = true, want false", name)
		}
	}
}

func TestFileFilter_ExtensionWithoutLeadingDot(t *testing.T) {
	t.Parallel()
	f := NewFileFilter([]string{"go", "py"}, nil)
	if !f.ShouldIndex("main.go") {
		t.Error("ShouldIndex(main.go) = false, want true (extension normalised)")
	}
	if !f.ShouldIndex("utils.py") {
		t.Error("ShouldIndex(utils.py) = false, want true")
	}
	if f.ShouldIndex("image.png") {
		t.Error("ShouldIndex(image.png) = true, want false")
	}
}

func TestFileFilter_ExtensionCaseInsensitive(t *testing.T) {
	t.Parallel()
	f := NewFileFilter([]string{".GO", ".Py"}, nil)
	if !f.ShouldIndex("main.go") {
		t.Error("ShouldIndex(main.go) = false, want true (case insensitive)")
	}
	if !f.ShouldIndex("Utils.PY") {
		t.Error("ShouldIndex(Utils.PY) = false, want true (case insensitive)")
	}
}

func TestFileFilter_ExcludePatterns(t *testing.T) {
	t.Parallel()
	f := NewFileFilter(nil, []string{"*_test.go", "*.gen.go", "*.md"})

	blocked := []string{"handler_test.go", "client.gen.go", "README.md"}
	for _, name := range blocked {
		if f.ShouldIndex(name) {
			t.Errorf("ShouldIndex(%q) = true, want false (excluded)", name)
		}
	}

	allowed := []string{"main.go", "utils.py", "handler.go"}
	for _, name := range allowed {
		if !f.ShouldIndex(name) {
			t.Errorf("ShouldIndex(%q) = false, want true (not excluded)", name)
		}
	}
}

func TestFileFilter_CombinedExtensionsAndExcludes(t *testing.T) {
	t.Parallel()
	f := NewFileFilter([]string{".go", ".py"}, []string{"*_test.go", "*.gen.go"})

	// Has allowed extension and not excluded
	allowed := []string{"main.go", "utils.py"}
	for _, name := range allowed {
		if !f.ShouldIndex(name) {
			t.Errorf("ShouldIndex(%q) = false, want true", name)
		}
	}

	// Allowed extension but excluded by pattern
	blocked := []string{"handler_test.go", "types.gen.go"}
	for _, name := range blocked {
		if f.ShouldIndex(name) {
			t.Errorf("ShouldIndex(%q) = true, want false (excluded pattern)", name)
		}
	}

	// Extension not in allowlist
	if f.ShouldIndex("readme.md") {
		t.Error("ShouldIndex(readme.md) = true, want false (wrong extension)")
	}
}

func TestFileFilter_NoExtension(t *testing.T) {
	t.Parallel()
	f := NewFileFilter([]string{".go"}, nil)
	if f.ShouldIndex("Makefile") {
		t.Error("ShouldIndex(Makefile) = true, want false (no extension)")
	}
	if f.ShouldIndex("README") {
		t.Error("ShouldIndex(README) = true, want false (no extension)")
	}
}

func TestParseCommaSeparated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"   ", nil},
		{".go,.py,.ts", []string{".go", ".py", ".ts"}},
		{" .go , .py , .ts ", []string{".go", ".py", ".ts"}},
		{"*_test.go,*.gen.go", []string{"*_test.go", "*.gen.go"}},
		{".go", []string{".go"}},
	}
	for _, tt := range tests {
		got := ParseCommaSeparated(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("ParseCommaSeparated(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("ParseCommaSeparated(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestStoreIndexDir_ExtensionFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create files with different extensions
	files := map[string]string{
		"main.go":         "package main",
		"utils.py":        "print('hi')",
		"image.png":       "\x89PNG",
		"readme.md":       "# Title",
		"handler_test.go": "package main",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Filter: only .go files, exclude *_test.go
	filter := NewFileFilter([]string{".go"}, []string{"*_test.go"})
	store := NewStore(&stubEmbedder{}, 0.5, WithFileFilter(filter))
	if err := store.IndexDir(t.Context(), dir); err != nil {
		t.Fatal(err)
	}

	// Should index main.go only (utils.py is wrong ext, image.png is wrong ext,
	// readme.md is wrong ext, handler_test.go is excluded by pattern)
	if got := store.Size(); got != 1 {
		t.Errorf("store size = %d, want 1", got)
	}

	// Verify the indexed file is main.go
	store.mu.RLock()
	name := store.examples[0].Filename
	store.mu.RUnlock()
	if name != "main.go" {
		t.Errorf("indexed file = %q, want %q", name, "main.go")
	}
}

func TestStoreIndexDir_NoFilterIndexesAll(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]string{
		"main.go":   "package main",
		"image.png": "\x89PNG",
		"readme.md": "# Title",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// No filter — backward compatible, all files indexed
	store := NewStore(&stubEmbedder{}, 0.5)
	if err := store.IndexDir(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	if got := store.Size(); got != 3 {
		t.Errorf("store size = %d, want 3", got)
	}
}

func TestStoreIndexDir_ExcludeOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]string{
		"main.go":         "package main",
		"handler_test.go": "package main",
		"types.gen.go":    "package main",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Only exclude patterns, no extension filter
	filter := NewFileFilter(nil, []string{"*_test.go", "*.gen.go"})
	store := NewStore(&stubEmbedder{}, 0.5, WithFileFilter(filter))
	if err := store.IndexDir(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	if got := store.Size(); got != 1 {
		t.Errorf("store size = %d, want 1", got)
	}
}
