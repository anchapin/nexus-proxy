package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// Core Configuration
	Port           = ":8000"
	LocalOllamaURL = "http://localhost:11434"
	
	// Model Definitions (2026 Standards)
	RouterModel    = "qwen3-coder:4b"      // Local SLM for fast, accurate JSON routing decisions
	LocalModel     = "qwen3-coder:8b"      // Local fallback for basic coding/formatting tasks
	FrontierModel  = "gpt-4o"              // Upstream model for deep complexity
	EmbeddingModel = "nomic-embed-text"    // Lightweight embedding model for fast local RAG
	
	// Frontier API Configuration (Replace with your actual keys/endpoints)
	FrontierAPIURL = "https://api.openai.com/v1/chat/completions"
	FrontierAPIKey = "your-api-key-here"

	// Directories
	ExamplesDir    = "./few_shot_examples" // Drop your perfect code snippets here for RAG
)

var (
	// Pre-compile regex for maximum performance during DSL evaluation
	// Bypasses the SLM entirely for obvious formatting tasks
	formattingRegex = regexp.MustCompile(`(?i)\b(css|format|docstring|lint|typo|boilerplate)\b`)
	
	// Looks for markdown JSON blocks that contain an array of objects to trigger TOON compression
	jsonArrayBlockRegex = regexp.MustCompile(`(?s)` + "```" + `json\s*(\[\s*\{.*?\}\s*\])\s*` + "```")

	// In-memory vector store for RAG few-shot examples
	fewShotStore []FewShotExample
)

type FewShotExample struct {
	Filename  string
	Content   string
	Embedding []float64
}

// cosineSimilarity calculates the mathematical closeness between two vectors for our custom in-memory RAG
func cosineSimilarity(a, b []float64) float64 {
	var dotProduct, normA, normB float64
	for i := range a {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}

func getEmbedding(text string) ([]float64, error) {
	payload := map[string]interface{}{
		"model":  EmbeddingModel,
		"prompt": text,
	}
	jsonPayload, _ := json.Marshal(payload)
	
	resp, err := http.Post(LocalOllamaURL+"/api/embeddings", "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	embInterface, ok := result["embedding"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("no embedding returned")
	}

	embedding := make([]float64, len(embInterface))
	for i, v := range embInterface {
		embedding[i] = v.(float64)
	}
	return embedding, nil
}

func initRAGIndex() {
	log.Println("[RAG INDEXER]: Scanning for few-shot examples...")
	
	if _, err := os.Stat(ExamplesDir); os.IsNotExist(err) {
		os.Mkdir(ExamplesDir, 0755)
		log.Printf("[RAG INDEXER]: Created %s directory. Drop golden code snippets here!", ExamplesDir)
		return
	}

	files, err := os.ReadDir(ExamplesDir)
	if err != nil {
		log.Printf("[RAG ERROR]: Failed reading directory: %v", err)
		return
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}
		path := filepath.Join(ExamplesDir, file.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		emb, err := getEmbedding(string(content))
		if err != nil {
			log.Printf("[RAG ERROR]: Failed to embed %s. Is nomic-embed-text installed? Error: %v", file.Name(), err)
			continue
		}

		fewShotStore = append(fewShotStore, FewShotExample{
			Filename:  file.Name(),
			Content:   string(content),
			Embedding: emb,
		})
		log.Printf("[RAG INDEXER]: Indexed %s successfully.", file.Name())
	}
}

func applyRetrievalAugmentation(messages []interface{}, latestPrompt string) []interface{} {
	if len(fewShotStore) == 0 || latestPrompt == "" {
		return messages // Nothing to retrieve or no prompt to match against
	}

	promptEmb, err := getEmbedding(latestPrompt)
	if err != nil {
		return messages
	}

	var bestMatch *FewShotExample
	var highestScore float64 = -1.0

	// Lightning-fast brute force search across the in-memory vectors
	for _, example := range fewShotStore {
		score := cosineSimilarity(promptEmb, example.Embedding)
		if score > highestScore {
			highestScore = score
			bestMatch = &example
		}
	}

	// Only inject if the similarity is reasonably high (threshold 0.55)
	if bestMatch != nil && highestScore > 0.55 {
		ragContext := fmt.Sprintf("\n\n[PROXY RETRIEVAL CONTEXT]: Here is a highly relevant, validated few-shot example from the local codebase (%s):\n```\n%s\n```\nAnalyze its architecture and apply its patterns if relevant to this task.", bestMatch.Filename, bestMatch.Content)
		
		// Inject into the latest user prompt
		for i := len(messages) - 1; i >= 0; i-- {
			if msg, ok := messages[i].(map[string]interface{}); ok {
				if role, _ := msg["role"].(string); role == "user" {
					content, _ := msg["content"].(string)
					msg["content"] = content + ragContext
					log.Printf("[RAG HIT]: Injected %s (Score: %.2f)", bestMatch.Filename, highestScore)
					break
				}
			}
		}
	}
	return messages
}

// applyPromptEngineering injects proven prompt structures inspired by Microsoft PromptWizard
// and Meta-Prompt generators directly into the system context before routing.
func applyPromptEngineering(messages []interface{}) []interface{} {
	systemPromptIndex := -1
	var systemContent string

	for i, msgIntf := range messages {
		if msg, ok := msgIntf.(map[string]interface{}); ok {
			if role, _ := msg["role"].(string); role == "system" {
				systemPromptIndex = i
				systemContent, _ = msg["content"].(string)
				break
			}
		}
	}

	// Apply structured optimization: Role Assignment, Chain-of-Thought enforcement, and Constraints.
	// This static compilation saves TTFT compared to dynamic meta-prompt generation.
	enhancements := `
[PROXY METADATA ENHANCEMENT]: 
- ROLE: You are an elite, autonomous Principal AI Software Engineer.
- REASONING (Chain-of-Thought): You must ALWAYS think step-by-step. Analyze the requirements, edge cases, and architectural impact before generating a single line of code.
- CONSTRAINTS: Prioritize modularity, memory efficiency, and strict security patterns. Do not silently ignore errors or swallow exceptions.
- FORMATTING: Provide clean, well-commented code. Do not use generic pleasantries.`

	if systemPromptIndex != -1 {
		sysMsg := messages[systemPromptIndex].(map[string]interface{})
		sysMsg["content"] = systemContent + "\n" + enhancements
	} else {
		newSysMsg := map[string]interface{}{
			"role":    "system",
			"content": enhancements,
		}
		// Prepend the new system message
		messages = append([]interface{}{newSysMsg}, messages...)
	}
	
	log.Println("[PROMPT COMPILER]: Meta-Prompt Engineering Applied.")
	return messages
}

// serializeToTOON compresses a raw JSON array of objects into Token-Oriented Object Notation
func serializeToTOON(jsonBytes []byte) (string, error) {
	var data []map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &data); err != nil {
		return "", err
	}

	if len(data) == 0 {
		return "items[0]{}:\n", nil
	}

	// Dynamically extract schema from the first object
	var keys []string
	for k := range data[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys) // Ensure predictable column ordering

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("items[%d]{%s}:\n", len(data), strings.Join(keys, ",")))

	for _, item := range data {
		var vals []string
		for _, k := range keys {
			v := fmt.Sprintf("%v", item[k])
			v = strings.ReplaceAll(v, ",", "，") // Sanitize commas to protect CSV structure
			v = strings.ReplaceAll(v, "\n", " ")
			vals = append(vals, v)
		}
		sb.WriteString("  " + strings.Join(vals, ",") + "\n")
	}

	return sb.String(), nil
}

// optimizePromptContext scans all messages, compresses JSON arrays, and injects TOON instructions
func optimizePromptContext(messages []interface{}) bool {
	compressionApplied := false
	systemPromptIndex := -1

	for i, msgIntf := range messages {
		msg, ok := msgIntf.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)
		content, _ := msg["content"].(string)

		if role == "system" {
			systemPromptIndex = i
		}

		if role == "user" || role == "assistant" {
			// Find all standard JSON arrays provided by the harness tools
			matches := jsonArrayBlockRegex.FindAllStringSubmatch(content, -1)
			for _, match := range matches {
				if len(match) == 2 {
					jsonStr := match[1]
					toonStr, err := serializeToTOON([]byte(jsonStr))
					if err == nil {
						newBlock := "```text\n" + toonStr + "```"
						content = strings.Replace(content, match[0], newBlock, 1)
						compressionApplied = true
					}
				}
			}
			if compressionApplied {
				msg["content"] = content
			}
		}
	}

	if compressionApplied {
		toonInstruction := "\n\n[PROXY SYSTEM NOTE]: Data arrays have been compressed using Token-Oriented Object Notation (TOON). The format is `object_name[count]{key1,key2}:\n  val1,val2`. Read the schema header to map the comma-separated rows."
		
		if systemPromptIndex != -1 {
			sysMsg := messages[systemPromptIndex].(map[string]interface{})
			sysContent, _ := sysMsg["content"].(string)
			sysMsg["content"] = sysContent + toonInstruction
		}
		log.Println("[TOON COMPRESSOR]: Successfully compressed JSON data arrays.")
	}

	return compressionApplied
}

// evaluateDSL serves as a fast-pass heuristic engine to bypass SLM routing for obvious queries
func evaluateDSL(prompt string) string {
	promptLower := strings.ToLower(prompt)

	// NEW: code2prompt-inspired Token Guardrail for 8GB VRAM constraint
	// 1 token is roughly 4 bytes/characters. 
	// A 6000 token prompt will eat significant KV cache on an 8GB GPU.
	estimatedTokens := len(prompt) / 4
	if estimatedTokens > 6000 {
		log.Printf("[ROUTER]: DSL Match (Context too large: ~%d tokens) -> Force routing to FRONTIER to prevent VRAM OOM\n", estimatedTokens)
		return "frontier"
	}

	// High-complexity triggers Fusion immediately
	if strings.Contains(promptLower, "architectural design") || strings.Contains(promptLower, "system architecture") {
		log.Println("[ROUTER]: DSL Match (High Complexity) -> Routing to FUSION")
		return "fusion"
	}

	// Low-complexity triggers Local immediately
	if formattingRegex.MatchString(promptLower) {
		log.Println("[ROUTER]: DSL Match (Basic Formatting) -> Routing to LOCAL")
		return "local"
	}

	return "" // No rules matched, fallback to the AI Router
}

type SLMDecision struct {
	Route string `json:"route"`
}

func extractLatestPrompt(body map[string]interface{}) string {
	messages, ok := body["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return ""
	}

	// Find the most recent 'user' message
	for i := len(messages) - 1; i >= 0; i-- {
		if msg, ok := messages[i].(map[string]interface{}); ok {
			if role, _ := msg["role"].(string); role == "user" {
				if content, ok := msg["content"].(string); ok {
					return content
				}
			}
		}
	}
	return ""
}

// getSLMRoutingDecision leverages Qwen3-Coder to determine task complexity
func getSLMRoutingDecision(prompt string) string {
	systemPrompt := `You are an intelligent routing assistant for a coding agent proxy. 
    Analyze the user's prompt. 
    - If it is a simple task (boilerplate, styling, small isolated functions), output {"route": "local"}. 
    - If it is a complex task (deep debugging, multi-file refactoring), output {"route": "frontier"}. 
    - If it requires extreme architectural deliberation and planning, output {"route": "fusion"}.
    Respond ONLY in valid JSON. No explanations.`

	payload := map[string]interface{}{
		"model": RouterModel,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": prompt},
		},
		"format":  "json",
		"stream":  false,
		"options": map[string]float64{"temperature": 0.1}, // Low temp for deterministic JSON
	}

	jsonPayload, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second) // Fast timeout
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", LocalOllamaURL+"/api/chat", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "frontier"
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[SLM ERROR]: Routing request failed: %v. Defaulting to Frontier.\n", err)
		return "frontier"
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "frontier"
	}

	msg, ok := result["message"].(map[string]interface{})
	if !ok {
		return "frontier"
	}

	content, ok := msg["content"].(string)
	if !ok {
		return "frontier"
	}

	var decision SLMDecision
	if err := json.Unmarshal([]byte(content), &decision); err != nil {
		log.Printf("[SLM ERROR]: Failed to parse JSON response: %v. Content: %s", err, content)
		return "frontier"
	}

	route := strings.ToLower(decision.Route)
	if route == "local" || route == "fusion" {
		return route
	}
	return "frontier" // Default safe route
}

// fetchPanelMember gets a full response from a specific model to pass to the arbiter
func fetchPanelMember(targetURL string, apiKey string, modelName string, body map[string]interface{}, ch chan<- string) {
	// Clone payload and override specifics for the panel member
	payload := make(map[string]interface{})
	for k, v := range body {
		payload[k] = v
	}
	payload["model"] = modelName
	payload["stream"] = false // Must be false so we can capture full text for synthesis

	jsonPayload, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewBuffer(jsonPayload))
	if err != nil {
		ch <- fmt.Sprintf("[%s failed to build request]", modelName)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		ch <- fmt.Sprintf("[%s failed to respond: Status %d]", modelName, resp.StatusCode)
		return
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	choices, ok := result["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		ch <- fmt.Sprintf("[%s returned empty choice]", modelName)
		return
	}
	
	firstChoice, ok := choices[0].(map[string]interface{})
	if !ok {
		ch <- "Error parsing choice"
		return
	}
	message, ok := firstChoice["message"].(map[string]interface{})
	if !ok {
		ch <- "Error parsing message"
		return
	}
	
	content, _ := message["content"].(string)
	ch <- content
}

// streamUpstream handles the standard proxying, flushing chunks natively as they arrive
func streamUpstream(w http.ResponseWriter, targetURL string, apiKey string, payload map[string]interface{}) {
	jsonPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", targetURL, bytes.NewBuffer(jsonPayload))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "Gateway Timeout", http.StatusGatewayTimeout)
		return
	}
	defer resp.Body.Close()

	// Pass original headers
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Zero-overhead streaming back to the harness
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			w.Write(line)
			flusher.Flush()
		}
		if err != nil {
			break
		}
	}
}

func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
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

	// Ensure messages exist
	var messages []interface{}
	var ok bool
	if messages, ok = body["messages"].([]interface{}); !ok {
		http.Error(w, "Invalid or missing messages array", http.StatusBadRequest)
		return
	}

	// --- MIDDLEWARE LAYER ---

	// 1. Meta-Prompt Compilation
	messages = applyPromptEngineering(messages)

	// 2. RAG Few-Shot Injection
	latestPrompt := extractLatestPrompt(body)
	messages = applyRetrievalAugmentation(messages, latestPrompt)

	// 3. TOON Data Compression
	if optimizePromptContext(messages) {
		body["messages"] = messages // Update body if compression occurred
	} else {
		body["messages"] = messages // Update body to reflect prompt engineering/RAG anyway
	}

	// Re-extract prompt in case it was modified
	latestPrompt = extractLatestPrompt(body)

	// --- ROUTING LAYER ---
	
	route := evaluateDSL(latestPrompt)
	if route == "" {
		log.Println("[ROUTER]: DSL bypassed, asking Qwen3-Coder for analysis...")
		route = getSLMRoutingDecision(latestPrompt)
		log.Printf("[ROUTER]: SLM Decision -> %s\n", strings.ToUpper(route))
	}

	// --- EXECUTION LAYER ---

	switch route {
	case "fusion":
		log.Println("[FUSION]: Spinning up model panel...")
		
		localCh := make(chan string, 1)
		frontierCh := make(chan string, 1)
		var wg sync.WaitGroup
		wg.Add(2)

		// Fire off concurrent requests
		go func() { defer wg.Done(); fetchPanelMember(LocalOllamaURL+"/v1/chat/completions", "", LocalModel, body, localCh) }()
		go func() { defer wg.Done(); fetchPanelMember(FrontierAPIURL, FrontierAPIKey, FrontierModel, body, frontierCh) }()

		wg.Wait()
		localRes := <-localCh
		frontierRes := <-frontierCh

		log.Println("[FUSION]: Synthesis in progress...")
		synthesizePrompt := fmt.Sprintf(`You are a Master Synthesis Arbiter AI. Synthesize the strongest final answer from these candidates.

User Prompt: %s

Candidate 1 (Local Model - Fast execution):
%s

Candidate 2 (Frontier Model - Deep reasoning):
%s`, latestPrompt, localRes, frontierRes)

		// Clone body for the Arbiter
		synthBody := make(map[string]interface{})
		for k, v := range body { synthBody[k] = v }
		synthBody["model"] = FrontierModel
		synthBody["messages"] = []map[string]string{
			{"role": "system", "content": "You are a master synthesis AI. Deliver only the final synthesized response. Do not mention that you are an arbiter."},
			{"role": "user", "content": synthesizePrompt},
		}

		// Stream Arbiter's result directly back to the harness
		streamUpstream(w, FrontierAPIURL, FrontierAPIKey, synthBody)

	case "local":
		body["model"] = LocalModel 
		streamUpstream(w, LocalOllamaURL+"/v1/chat/completions", "", body)

	default: 
		// Frontier fallback / explicitly routed
		streamUpstream(w, FrontierAPIURL, FrontierAPIKey, body)
	}
}

func main() {
	// Initialize in-memory Vector store at boot
	initRAGIndex() 

	http.HandleFunc("/v1/chat/completions", chatCompletionsHandler)
	
	log.Printf("Starting High-Performance Go Router on %s...\n", Port)
	if err := http.ListenAndServe(Port, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}