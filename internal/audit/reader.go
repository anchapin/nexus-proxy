package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Verify reads the JSONL audit log at path and recomputes the hash chain,
// returning a non-nil error if any entry has been tampered with, removed,
// reordered, or corrupted. A pristine (unmodified) file always returns
// nil. This is the core tamper-detection routine.
//
// The verification walks every line in order, checking that:
//  1. Each entry's PrevHash matches the previous entry's Hash (or
//     GenesisHash for the first entry).
//  2. Each entry's Hash matches a fresh recomputation from its payload
//     fields and the expected prev_hash.
//
// A break at either check pinpoints the first tampered line.
func Verify(path string) error {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("audit: open %q for verify: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	prev := GenesisHash
	lineNo := 0
	sc := bufio.NewScanner(f)
	// Audit entries are small (< 1 KiB), but raise the buffer to be safe
	// against unusually long model names or request IDs.
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lineNo++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("audit: line %d: invalid JSON: %w", lineNo, err)
		}
		if e.PrevHash != prev {
			return fmt.Errorf(
				"audit: line %d: prev_hash mismatch (expected %s, got %s) — chain broken or entry tampered",
				lineNo, prev, e.PrevHash)
		}
		want := computeHash(prev, hashPayload{
			Timestamp: e.Timestamp,
			RequestID: e.RequestID,
			ClientIP:  e.ClientIP,
			KeyHash:   e.KeyHash,
			Route:     e.Route,
			Model:     e.Model,
			Outcome:   e.Outcome,
		})
		if e.Hash != want {
			return fmt.Errorf(
				"audit: line %d: hash mismatch — entry payload was modified after recording",
				lineNo)
		}
		prev = e.Hash
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("audit: scan %q: %w", path, err)
	}
	return nil
}

// Read loads every entry from the JSONL audit log at path in chain order.
// It does not verify integrity; call Verify first when tamper-detection is
// required. Read is provided for querying/auditing tooling that needs to
// inspect or export the recorded entries.
func Read(path string) ([]AuditEntry, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("audit: open %q for read: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var entries []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("audit: line %d: invalid JSON: %w", lineNo, err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("audit: scan %q: %w", path, err)
	}
	return entries, nil
}
