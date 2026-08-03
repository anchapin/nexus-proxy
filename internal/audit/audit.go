// Package audit implements a tamper-evident, append-only audit log for
// routing decisions (issue #1153).
//
// Each proxied /v1/chat/completions request appends one JSON line to the
// audit file. Lines are hash-chained: every entry's hash incorporates the
// previous entry's hash, so mutating, deleting, or reordering any non-final
// row breaks the chain and is detected by Verify.
//
// The chain uses a fixed genesis hash for the first row so an independent
// verifier can recompute the entire chain from the file alone — no
// out-of-band state is required.
//
// The backend is a plain JSONL file opened with O_APPEND. Each Record call
// performs a single Write of the marshalled line under a mutex so appends
// from concurrent goroutines stay linear. Durability is tunable via the
// sync mode: "full" calls fsync after every append (safe but slower);
// "none" relies on the OS page cache (fastest, survivable only across
// clean shutdowns).
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// GenesisHash is the fixed prev_hash of the first entry in every audit
// chain. It is a well-known constant (64 zeros) so independent verifiers
// can recompute the chain from genesis without out-of-band state.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// keyHashLen is the number of hex characters retained from the SHA-256 of
// an accepted credential (issue #1153: "truncated to 16 hex"). 16 hex
// chars = 8 bytes = 64 bits of entropy — enough for unique attribution
// while guaranteeing the raw secret can never be recovered.
const keyHashLen = 16

// AuditEntry is a single tamper-evident record in the audit chain. The
// Hash and PrevHash fields are computed by the auditor; callers populate
// only the attribution fields.
type AuditEntry struct {
	Timestamp time.Time `json:"timestamp"`
	RequestID string    `json:"request_id"`
	ClientIP  string    `json:"client_ip"`
	KeyHash   string    `json:"key_hash"`
	Route     string    `json:"route"`
	Model     string    `json:"model"`
	Outcome   string    `json:"outcome"`
	PrevHash  string    `json:"prev_hash"`
	Hash      string    `json:"hash"`
}

// hashPayload is the canonical JSON-serialisable subset of AuditEntry used
// for hash computation. It deliberately excludes Hash and PrevHash so that
// hash = SHA256(prev_hash || canonical_json(payload)). Go's json.Marshal
// emits struct fields in declaration order, which is deterministic for a
// fixed struct type — sufficient for a self-verifying chain.
type hashPayload struct {
	Timestamp time.Time `json:"timestamp"`
	RequestID string    `json:"request_id"`
	ClientIP  string    `json:"client_ip"`
	KeyHash   string    `json:"key_hash"`
	Route     string    `json:"route"`
	Model     string    `json:"model"`
	Outcome   string    `json:"outcome"`
}

// computeHash returns the hex-encoded SHA-256 of prev_hash concatenated
// with the canonical JSON encoding of payload. This is the core of the
// tamper-evidence guarantee: changing any payload field or the prev_hash
// produces a completely different hash.
func computeHash(prev string, payload hashPayload) string {
	h := sha256.New()
	h.Write([]byte(prev))
	if b, err := json.Marshal(payload); err == nil {
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// HashCredential returns a truncated SHA-256 hex digest of an accepted
// credential (API key / bearer token), or "" when the credential is empty.
// The raw credential is never recoverable from the digest. This is the
// single source of truth for the key_hash field so write and verify paths
// stay consistent.
func HashCredential(credential string) string {
	if credential == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:])[:keyHashLen]
}

// SyncMode controls the fsync policy after each append.
type SyncMode string

const (
	// SyncFull calls f.Sync() (fsync) after every append so the entry
	// survives a crash immediately. This is the safe default.
	SyncFull SyncMode = "full"
	// SyncNone relies on the OS page cache. Faster, but buffered entries
	// may be lost on a hard crash.
	SyncNone SyncMode = "none"
)

// ParseSyncMode validates a sync-mode string from configuration. Unknown
// values fall back to SyncFull (fail-safe).
func ParseSyncMode(s string) SyncMode {
	if SyncMode(s) == SyncNone {
		return SyncNone
	}
	return SyncFull
}

// Auditor records tamper-evident audit entries to an append-only store.
type Auditor interface {
	Record(entry AuditEntry) error
	Close() error
}

// NoopAuditor discards every entry. It is returned when audit is disabled
// so callers can invoke Record unconditionally without nil-checks.
type NoopAuditor struct{}

// Record implements Auditor; it is a no-op that always succeeds.
func (NoopAuditor) Record(AuditEntry) error { return nil }

// Close implements Auditor; it is a no-op.
func (NoopAuditor) Close() error { return nil }

// FileAuditor writes hash-chained audit entries to an append-only JSONL
// file. It is safe for concurrent use: a mutex serialises Record calls so
// the prev_hash chain stays linear, and O_APPEND with a single Write per
// entry keeps each line atomic at the OS level.
type FileAuditor struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	syncMode SyncMode
	lastHash string
}

// Open creates or opens the audit log at path for appending. The parent
// directory is created with mode 0700 if it does not exist; the file is
// opened with mode 0600. If the file already contains entries (e.g. the
// process restarted), the chain is continued from the last stored hash so
// the file stays verifiable across restarts.
func Open(path string, syncMode SyncMode) (*FileAuditor, error) {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("audit: create log directory %q: %w", dir, err)
		}
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open log file %q: %w", path, err)
	}
	last, err := lastHash(path)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("audit: read existing chain: %w", err)
	}
	if last == "" {
		last = GenesisHash
	}
	slog.Info("audit log enabled",
		slog.String("path", path),
		slog.String("sync", string(syncMode)),
	)
	return &FileAuditor{f: f, path: path, syncMode: syncMode, lastHash: last}, nil
}

// Record appends entry to the log, computing its PrevHash and Hash from
// the current chain tip. The entry's own PrevHash/Hash fields are
// overwritten so callers cannot break the chain by mistake.
func (a *FileAuditor) Record(entry AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	entry.PrevHash = a.lastHash
	entry.Hash = computeHash(a.lastHash, hashPayload{
		Timestamp: entry.Timestamp,
		RequestID: entry.RequestID,
		ClientIP:  entry.ClientIP,
		KeyHash:   entry.KeyHash,
		Route:     entry.Route,
		Model:     entry.Model,
		Outcome:   entry.Outcome,
	})

	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("audit: marshal entry: %w", err)
	}
	line = append(line, '\n')
	if _, err := a.f.Write(line); err != nil {
		return fmt.Errorf("audit: write entry: %w", err)
	}
	if a.syncMode == SyncFull {
		if err := a.f.Sync(); err != nil {
			return fmt.Errorf("audit: fsync: %w", err)
		}
	}
	a.lastHash = entry.Hash
	return nil
}

// Close releases the underlying file. After Close the auditor must not be
// used.
func (a *FileAuditor) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil
	}
	err := a.f.Close()
	a.f = nil
	return err
}

// lastHash reads the existing JSONL log at path and returns the hash of
// the final entry, or "" when the file is empty. It is used by Open to
// continue the chain across restarts.
func lastHash(path string) (string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer func() { _ = f.Close() }()

	var last string
	dec := json.NewDecoder(f)
	for {
		var e AuditEntry
		if err := dec.Decode(&e); err != nil {
			if err == io.EOF {
				break
			}
			return "", fmt.Errorf("decode entry: %w", err)
		}
		last = e.Hash
	}
	return last, nil
}
