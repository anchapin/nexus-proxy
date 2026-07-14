package tokenizer

import (
	"testing"
)

func TestCountTokens(t *testing.T) {
	cases := map[string]struct {
		min, max int // acceptable range (within 15% of actual)
	}{
		"":                      {min: 0, max: 0},

		// English prose: tiktoken gives 1 token for short strings of single chars
		"Hello, world!":        {min: 3, max: 5},
		"The quick brown fox jumps over the lazy dog.": {min: 9, max: 12},

		// Code: tiktoken counts keywords efficiently
		"func main() { println(\"hello\") }": {min: 8, max: 12},

		// CJK characters: tiktoken encodes these as ~1-2 tokens each
		"你好世界":               {min: 4, max: 6},
		"日本語のテスト":           {min: 6, max: 9},

		// Whitespace-heavy: tiktoken is accurate
		"   \n\t  word   \n  ":   {min: 4, max: 6},

		// Conversation history inflation: repeated role markers
		"[{\"role\":\"user\",\"content\":\"hi\"},{\"role\":\"assistant\",\"content\":\"hello\"},{\"role\":\"user\",\"content\":\"hi\"},{\"role\":\"assistant\",\"content\":\"hello\"}]": {min: 30, max: 40},
	}
	for input, tc := range cases {
		got := CountTokens(input)
		if got < tc.min || got > tc.max {
			t.Errorf("CountTokens(%q) = %d, want [%d, %d]", input, got, tc.min, tc.max)
		}
	}
}

func TestCountTokens_Empty(t *testing.T) {
	if got := CountTokens(""); got != 0 {
		t.Errorf("CountTokens(\"\") = %d, want 0", got)
	}
}
