// Package tokenizer provides accurate token counting using the cl100k_base
// encoding (the same encoding used by GPT-4 and GPT-3.5-turbo). It is used
// by the router guardrail and telemetry to replace the naive len/4 heuristic.
package tokenizer

import (
	"sync"

	"github.com/pkoukk/tiktoken-go"
)

// encodingName is the encoding used by OpenAI's chat models (GPT-4, GPT-3.5).
const encodingName = "cl100k_base"

// encoder is the singleton tiktoken encoder. Load once; safe for concurrent use.
var (
	encoder *tiktoken.Tiktoken
	loadErr error
	once    sync.Once
)

// getEncoder returns the singleton encoder, initializing it on first call.
// Thread-safe after initialization.
func getEncoder() (*tiktoken.Tiktoken, error) {
	once.Do(func() {
		encoder, loadErr = tiktoken.GetEncoding(encodingName)
	})
	return encoder, loadErr
}

// CountTokens returns the number of cl100k_base tokens in s. This is the
// accurate count used for the router guardrail and telemetry input-token
// estimates. Returns 0 on error (encoder load failure) so the guardrail
// degrades to the safe frontier path rather than panicking.
func CountTokens(s string) int {
	if s == "" {
		return 0
	}
	enc, err := getEncoder()
	if err != nil {
		// Fallback: return 0 so guardrail escalates to frontier (safe default).
		return 0
	}
	// EncodeOrdinary avoids adding special tokens (bos/eos) which would
	// inflate the count for prompts that don't include them.
	tokens := enc.EncodeOrdinary(s)
	return len(tokens)
}
