// Package handlers contains the HTTP entry points for the proxy. The chat
// handler is the only public endpoint; it owns the request lifecycle:
// middleware chain -> routing decision -> upstream execution.
package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/middleware"
	"github.com/anchapin/nexus-proxy/internal/rag"
	"github.com/anchapin/nexus-proxy/internal/router"
	"github.com/anchapin/nexus-proxy/internal/upstream"
)

// Deps bundles the collaborators the chat handler needs. Wiring them
// explicitly makes the handler trivial to unit-test with stubs.
type Deps struct {
	Config          config.Config
	Client          upstream.Client // http.Client satisfies this interface
	RAG             *rag.Store
	SLM             *router.SLMClient
	FormattingRegex *regexp.Regexp
}

// Chat returns an http.Handler that runs the middleware chain, picks a
// route, and streams the chosen upstream's response back to the harness.
//
// Middleware order (do not reorder casually):
//  1. applyPromptEngineering    — inject role/CoT/constraints into system
//  2. applyRetrievalAugmentation — embed latest prompt, inject best match
//  3. optimizePromptContext     — TOON-compress JSON arrays in user msgs
//  4. evaluateDSL -> getSLMRoutingDecision
//  5. Stream (local/frontier) or Panel (fusion)
func Chat(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read request", http.StatusBadRequest)
			return
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}
		rawMessages, ok := body["messages"].([]interface{})
		if !ok {
			http.Error(w, "Invalid or missing messages array", http.StatusBadRequest)
			return
		}

		messages := middleware.ApplyPromptEngineering(rawMessages, d.Config.MetaPrompt)
		latestPrompt := middleware.ExtractLatestUserPrompt(messages)
		if ex, score, err := d.RAG.Retrieve(r.Context(), latestPrompt); err == nil && ex != nil {
			messages = middleware.InjectRAG(messages, rag.FormatInjection(ex))
			log.Printf("[RAG HIT]: Injected %s (Score: %.2f)", ex.Filename, score)
		}
		if middleware.CompressJSONBlocks(messages) {
			messages = middleware.AppendSystemNote(messages, d.Config.TOONNotice)
			log.Println("[TOON COMPRESSOR]: Successfully compressed JSON data arrays.")
		}
		body["messages"] = messages
		latestPrompt = middleware.ExtractLatestUserPrompt(messages)

		var route router.Route
		if g, hit := router.Guardrail(latestPrompt, d.Config.TokenGuardrail); hit {
			log.Printf("[ROUTER]: DSL Match (Context too large: ~%d tokens) -> Force routing to FRONTIER", len(latestPrompt)/4)
			route = g
		} else if r2, hit := router.DSL(latestPrompt, d.FormattingRegex); hit {
			log.Printf("[ROUTER]: DSL Match -> %s", r2)
			route = r2
		} else {
			log.Println("[ROUTER]: DSL bypassed, asking SLM for analysis...")
			dec, err := d.SLM.Decide(r.Context(), latestPrompt)
			if err != nil {
				log.Printf("[SLM ERROR]: %v. Defaulting to Frontier.", err)
				dec = router.RouteFrontier
			}
			log.Printf("[ROUTER]: SLM Decision -> %s", dec)
			route = dec
		}

		switch route {
		case router.RouteFusion:
			log.Println("[FUSION]: Spinning up model panel...")
			err := upstream.Panel(
				w, d.Client,
				d.Config.OllamaURL, d.Config.LocalModel,
				d.Config.FrontierURL, d.Config.FrontierModel,
				d.Config.FrontierURL, d.Config.FrontierKey, d.Config.FrontierModel,
				body, latestPrompt, d.Config.FusionTimeout,
			)
			if err != nil {
				log.Printf("[FUSION ERROR]: %v", err)
				http.Error(w, "Upstream error", http.StatusBadGateway)
			}

		case router.RouteLocal:
			// Cascade: try local Ollama first, fall back to configured
			// frontier endpoints (frontier, then z.ai) on retryable
			// failures. Cascade is rebuilt per request so config changes
			// take effect without restarting the process (issue #14).
			cas := upstream.BuildLocalCascade(upstream.CascadeConfig{
				LocalURL:      d.Config.OllamaURL,
				LocalModel:    d.Config.LocalModel,
				FrontierURL:   d.Config.FrontierURL,
				FrontierModel: d.Config.FrontierModel,
				FrontierKey:   d.Config.FrontierKey,
				ZAIURL:        d.Config.ZAIURL,
				ZAIModel:      d.Config.ZAIModel,
				ZAIKey:        d.Config.ZAIKey,
				Timeout:       d.Config.CascadeTimeout,
			})
			res, err := cas.Run(w, d.Client, body)
			logCascadeTelemetry(res, err)
			if err != nil {
				log.Printf("[UPSTREAM ERROR]: %v", err)
				http.Error(w, "Upstream error", http.StatusBadGateway)
			}

		default:
			if err := upstream.Stream(w, d.Client,
				d.Config.FrontierURL, d.Config.FrontierKey, body); err != nil {
				log.Printf("[UPSTREAM ERROR]: %v", err)
				http.Error(w, "Upstream error", http.StatusBadGateway)
			}
		}
	})
}

// logCascadeTelemetry emits the route_attempted / attempts line for the
// cascade. Once issue #16 wires a real metrics store this becomes the
// place to publish a structured event; for now it's plain log.Println.
func logCascadeTelemetry(res upstream.CascadeResult, err error) {
	log.Printf("[CASCADE TELEMETRY]: route_attempted=%s attempts=%d served_by=%s success=%v err=%v",
		res.RouteAttempted, res.Attempts, res.ServedBy, res.Succeeded, err)
}
