// Command nexus is the entry point for the Nexus Proxy. It loads
// configuration from the environment, constructs the chat handler with its
// collaborators (RAG store, SLM client, formatting regex), and serves
// /v1/chat/completions.
package main

import (
	"context"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/handlers"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
)

const (
	formattingRegexPattern = `(?i)\b(css|format|docstring|lint|typo|boilerplate)\b`
	bootRAGTimeout         = 30 * time.Second
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	emb := rag.NewOllamaEmbedder(cfg.OllamaURL, cfg.EmbeddingModel, nil)
	store := rag.NewStore(emb, cfg.RAGThreshold)
	bootCtx, cancel := context.WithTimeout(context.Background(), bootRAGTimeout)
	defer cancel()
	if err := store.IndexDir(bootCtx, cfg.ExamplesDir); err != nil {
		log.Printf("[BOOT WARN]: RAG index failed: %v", err)
	}

	slm := router.NewSLMClient(cfg.OllamaURL, cfg.RouterModel, cfg.SLMTimeout, nil)
	re := regexp.MustCompile(formattingRegexPattern)

	mux := http.NewServeMux()
	mux.Handle("/v1/chat/completions", handlers.Chat(handlers.Deps{
		Config:          cfg,
		Client:          http.DefaultClient,
		RAG:             store,
		SLM:             slm,
		FormattingRegex: re,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("Starting Nexus Proxy on %s (local=%s frontier=%s)", cfg.Addr, cfg.LocalModel, cfg.FrontierModel)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}