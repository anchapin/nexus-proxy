# **Product Requirements Document (PRD): Nexus Proxy**

## **1\. Vision and Objective**

**Vision:** To build the ultimate, hardware-aware AI routing gateway for local developers, seamlessly blending the speed and privacy of local Small Language Models (SLMs) with the power of cloud-based Frontier models.

**Objective:** Create a zero-dependency, harness-agnostic Go proxy that intercepts OpenAI-compatible API requests from AI coding agents (OpenCode, Aider, OpenHands), optimizes the prompt context, and intelligently routes the request to the most efficient model based on heuristic rules, VRAM constraints, and semantic complexity.

## **2\. Target Audience**

* Software engineers using AI coding agents locally.  
* Developers with constrained local hardware (e.g., 8GB-12GB VRAM GPUs like the AMD 6600 XT).  
* Teams looking to reduce their OpenAI/Anthropic API bills without sacrificing "time to first token" (TTFT) or coding accuracy.

## **3\. Core Features (MVP)**

* **Harness Agnosticism:** Fully supports the OpenAI API spec (/v1/chat/completions) and SSE streaming.  
* **Hardware Guardrails:** Calculates token budget heuristically and prevents local Out-Of-Memory (OOM) crashes by force-routing oversized prompts to cloud APIs.  
* **Context Compression (TOON):** Automatically detects massive JSON arrays in agent outputs and compresses them into Token-Oriented Object Notation, saving up to 40% in context tokens.  
* **In-Memory RAG:** Local vector store mapping of "golden" code snippets injected seamlessly into the system prompt.  
* **Dynamic Meta-Prompting:** Statically compiles role assignments and Chain-of-Thought (CoT) instructions into requests to prevent lazy AI outputs.  
* **Multi-Tier Routing:**  
  * *Tier 1 (DSL):* Regex and rule-based fast routing (e.g., simple formatting \-\> Local).  
  * *Tier 2 (SLM):* Semantic routing utilizing a highly quantized local model (e.g., Qwen3-Coder-4B) to judge complexity.  
  * *Tier 3 (Fusion):* Parallel execution of local and frontier models with a final Arbiter synthesis for high-complexity architectural queries.

## **4\. Non-Goals**

* We are *not* building an AI coding harness. Nexus Proxy assumes the user is bringing their own agent (OpenCode, etc.).  
* We are *not* building an LLM inference engine. We rely on Ollama/vLLM for local model execution.

# **Technical Specification**

## **1\. Tech Stack**

* **Language:** Go (1.21+) \- chosen for native concurrency, low memory footprint, and high-performance I/O streaming.  
* **Local Inference:** Ollama API (Vulkan backend optimized for AMD).  
* **Upstream APIs:** OpenAI API standard format.  
* **Embeddings:** nomic-embed-text via Ollama for local, zero-cost vector math.

## **2\. Architecture Flow**

1. **Ingress:** Harness sends POST /v1/chat/completions.  
2. **Middleware Pipeline:**  
   * ContextInterceptor: Applies TOON compression to JSON arrays.  
   * RAGInjector: Embeds prompt, finds Cosine Similarity in memory, injects few-shot examples.  
   * PromptCompiler: Appends system constraints (PromptWizard principles).  
3. **Guardrail Check:** If Estimated Tokens \> Hardware VRAM Limit \-\> Route to Frontier.  
4. **Routing Engine:**  
   * Evaluate RoutingDSL (Regex/Keywords).  
   * If no DSL match, POST to Qwen3-Coder for JSON routing decision.  
5. **Execution & Streaming:**  
   * Proxy opens connection to target (Local or Frontier).  
   * Reads bytes via bufio.Reader and flushes natively back to the Harness.

# **Roadmap**

## **Phase 1: Foundation & Refactor (Weeks 1-2)**

* \[x\] Validate core logic in single router\_proxy.go.  
* \[ \] Break single file into standard Go module structure (see below).  
* \[ \] Extract hardcoded configurations (API keys, ports, models) into a config.yaml and .env system.

## **Phase 2: Observability & Metrics (Weeks 3-4)**

* \[ \] Add structured logging (slog).  
* \[ \] Implement a lightweight SQLite local database to track:  
  * Tokens saved via TOON compression.  
  * Requests routed locally vs. cloud.  
  * Estimated API cost savings ($$).  
* \[ \] Create a simple CLI output to display the "Daily Savings Dashboard".

## **Phase 3: Advanced Hardware Awareness (Weeks 5-6)**

* \[ \] Implement dynamic VRAM checking (pinging the OS/Ollama to see exact available VRAM rather than using a static token heuristic).  
* \[ \] Add "Graceful Degradation": If Ollama is down, automatically fallback all routing to the Frontier API so the developer's flow is never interrupted.

## **Phase 4: Open Source Release (Future)**

* \[ \] Package as Docker container.  
* \[ \] Write comprehensive README.md with setup instructions for OpenCode and Aider.

# **Recommended Go Project Structure**

To move beyond the single-file prototype, we will adopt the standard Go project layout. This separates concerns, makes testing easy, and prepares the codebase for open-source contribution.

nexus-proxy/  
├── cmd/  
│   └── nexus/  
│       └── main.go                 \# Entry point: wires up config, routes, and starts server  
├── internal/  
│   ├── config/  
│   │   └── config.go               \# Parses YAML/.env (Models, API keys, Hardware limits)  
│   ├── handlers/  
│   │   └── chat.go                 \# HTTP handler for /v1/chat/completions  
│   ├── middleware/  
│   │   ├── toon.go                 \# JSON-to-TOON compression logic  
│   │   ├── prompt\_engine.go        \# PromptWizard static injections  
│   │   └── rag.go                  \# Vector store and Cosine Similarity logic  
│   ├── router/  
│   │   ├── dsl.go                  \# Regex/Heuristic fast-pass logic  
│   │   ├── slm.go                  \# Qwen3-Coder routing logic  
│   │   └── guardrails.go           \# VRAM/Token budget math  
│   └── upstream/  
│       ├── stream.go               \# bufio byte flushing logic  
│       └── fusion.go               \# Goroutine parallel execution & arbiter logic  
├── few\_shot\_examples/              \# User's golden code snippets (ignored by git)  
│   ├── example\_1.go  
│   └── style\_guide.md  
├── go.mod  
├── go.sum  
├── .env.example  
├── config.yaml                     \# Easy setup file for the user  
└── README.md  
