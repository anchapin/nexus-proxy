// Package tokenizer provides tiktoken-based token counting for accurate
// prompt-length estimation. It uses the cl100k_base encoding which is the
// same base GPT-4 tokenizer used by OpenAI models.
package tokenizer

import (
	"github.com/pkoukk/tiktoken-go"
)

// CountTokens returns the number of tokens in s using the cl100k_base
// encoding (GPT-4 / ChatGPT tokenizer). This is accurate to within ~15%
// for English prose, code, CJK, whitespace-heavy, and conversation-history
// prompts — far superior to the naive len(s)/4 heuristic it replaces.
func CountTokens(s string) int {
	if s == "" {
		return 0
	}
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		// Fallback: the heuristic is better than zero.
		return len(s) / 4
	}
	tokens := enc.Encode(s, nil, nil)
	return len(tokens)
}
