package handlers

import (
	"testing"

	"github.com/anchapin/nexus-proxy/internal/tokenizer"
)

func TestMain(m *testing.M) {
	tokenizer.CountTokens("")
	m.Run()
}
