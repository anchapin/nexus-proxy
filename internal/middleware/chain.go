// Package middleware contains the prompt-transformation passes that run
// before routing: meta-prompt injection, TOON compression, and RAG lookup.
//
// Each pass takes and returns []interface{} (the heterogeneous OpenAI
// message shape) so they can be chained. Passes must not depend on global
// state — they receive their configuration through their constructor or
// per-call arguments.
package middleware

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anchapin/nexus-proxy/internal/rag"
)

// Middleware transforms the messages slice. Implementations must be safe
// to call concurrently from many goroutines; the chat handler invokes
// Transform on the request goroutine so middleware should not block for
// long (a network call is not acceptable without a timeout).
//
// The issue #224 interface signature is intentionally minimal:
//   - ([]interface{}) ([]interface{}, error)
//
// Stateful middleware (e.g. RAG lookup) should implement ContextMiddleware
// so the handler can supply the request context at call time.
type Middleware interface {
	Name() string
	Transform([]interface{}) ([]interface{}, error)
}

// ContextMiddleware is an optional extension for middleware that needs
// the request context (e.g. RAG retrieval). The handler calls this when
// the middleware also implements ContextMiddleware; otherwise it calls
// the plain Middleware.Transform.
//
// Embedding Middleware in ContextMiddleware is the canonical adapter
// pattern so existing Middleware implementations that don't need context
// don't need to change.
type ContextMiddleware interface {
	Middleware
	TransformContext(ctx context.Context, messages []interface{}) ([]interface{}, error)
}

// MiddlewareFunc adapts a plain function to the Middleware interface.
type MiddlewareFunc struct {
	name string
	fn   func([]interface{}) ([]interface{}, error)
}

func (f MiddlewareFunc) Name() string { return f.name }
func (f MiddlewareFunc) Transform(m []interface{}) ([]interface{}, error) {
	return f.fn(m)
}

// NewMiddleware creates a named Middleware from a plain transform function.
func NewMiddleware(name string, fn func([]interface{}) ([]interface{}, error)) Middleware {
	return MiddlewareFunc{name: name, fn: fn}
}

// ragMiddleware implements ContextMiddleware for RAG retrieval.
type ragMiddleware struct {
	rag       rag.RAGStore
	threshold float64
}

func (r ragMiddleware) Name() string { return "rag" }

func (r ragMiddleware) Transform(m []interface{}) ([]interface{}, error) {
	return m, nil // RAG needs context; caller must use TransformContext
}

func (r ragMiddleware) TransformContext(ctx context.Context, msgs []interface{}) ([]interface{}, error) {
	prompt := ExtractLatestUserPrompt(msgs)
	ex, _, err := r.rag.Retrieve(ctx, prompt)
	if err != nil || ex == nil {
		return msgs, nil
	}
	return InjectRAG(msgs, rag.FormatInjection(ex)), nil
}

// NewRAGMiddleware creates a RAG middleware that uses the provided store.
func NewRAGMiddleware(store rag.RAGStore, threshold float64) ContextMiddleware {
	return &ragMiddleware{store, threshold}
}

// Named middleware registry. Built-in transforms are registered at
// package init; operators can override individual entries with
// Register or RegisterFactory.
var registry = make(map[string]Middleware)

// registeredContext is the set of names that are ContextMiddleware.
// Used by BuildChain to return the correct interface type.
var registeredContext = make(map[string]bool)

// MiddlewareConfig holds the dependencies needed to construct middleware
// via a factory function (issue #370). Factories receive this at
// construction time so they can embed runtime dependencies (e.g. RAG store).
type MiddlewareConfig struct {
	// RAG store for the rag middleware.
	RAGStore rag.RAGStore
	// Meta prompt string for promptEngineering.
	MetaPrompt string
	// TOON notice string for appendSystemNote.
	TOONNotice string
	// Whether to use isolated (off) mode for prompt injection.
	Isolated bool
	// RAG similarity threshold.
	RAGThreshold float64
}

// MiddlewareFactory creates a Middleware instance from the given config.
// Registered via RegisterFactory to allow operator-extensible middleware
// that need constructor-time dependencies.
type MiddlewareFactory func(cfg MiddlewareConfig) Middleware

// factoryRegistry maps middleware name to its factory.
// When BuildChain finds a factory it calls it to construct the instance.
var factoryRegistry = make(map[string]MiddlewareFactory)

// Register adds m to the global registry under m.Name(). Registering
// the same name twice panics. If m also implements ContextMiddleware,
// register it as such.
func Register(m Middleware) {
	if _, ok := registry[m.Name()]; ok {
		panic("middleware: duplicate name: " + m.Name())
	}
	registry[m.Name()] = m
	if _, ok := m.(ContextMiddleware); ok {
		registeredContext[m.Name()] = true
	}
}

// RegisterFactory registers factory as the constructor for the named
// middleware. When BuildChain is called, if a factory is registered
// for a name it is called to produce the instance (otherwise the
// pre-constructed instance in the registry is used). Panics if name
// is already registered as a factory or an instance.
func RegisterFactory(name string, factory MiddlewareFactory) {
	if _, ok := registry[name]; ok {
		panic("middleware: name already registered as instance: " + name)
	}
	if _, ok := factoryRegistry[name]; ok {
		panic("middleware: factory already registered: " + name)
	}
	factoryRegistry[name] = factory
}

// Get returns the registered middleware with the given name, or nil if
// not found.
func Get(name string) Middleware {
	return registry[name]
}

// IsContextAware reports whether the named middleware is a ContextMiddleware.
func IsContextAware(name string) bool {
	return registeredContext[name]
}

// BuildChain parses chainSpec (comma-separated names) and returns the
// ordered Middleware slice. Unknown names cause an error. Empty spec
// returns the default chain. The default chain is
// "promptEngineering,rag,compressJSONBlocks,appendSystemNote".
//
// When a factory is registered for a name (via RegisterFactory), BuildChain
// calls it with cfg to construct the instance. This allows middleware with
// constructor-time dependencies (e.g. rag store) to be built via the registry.
func BuildChain(chainSpec string, cfg MiddlewareConfig) ([]Middleware, error) {
	if chainSpec == "" {
		return DefaultChain(cfg), nil
	}
	names := strings.Split(chainSpec, ",")
	out := make([]Middleware, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if factory, ok := factoryRegistry[n]; ok {
			out = append(out, factory(cfg))
			continue
		}
		m := registry[n]
		if m == nil {
			return nil, fmt.Errorf("middleware: unknown name %q in chain", n)
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("middleware: empty chain")
	}
	return out, nil
}

// DefaultChain returns the default middleware chain: promptEngineering,
// rag, compressJSONBlocks, appendSystemNote. Uses cfg to construct any
// registered factories; falls back to pre-constructed instances.
func DefaultChain(cfg MiddlewareConfig) []Middleware {
	chain := []string{"promptEngineering", "rag", "compressJSONBlocks", "appendSystemNote"}
	out := make([]Middleware, 0, len(chain))
	for _, n := range chain {
		if factory, ok := factoryRegistry[n]; ok {
			out = append(out, factory(cfg))
		} else {
			out = append(out, registry[n])
		}
	}
	return out
}

// Init registers the four built-in transforms under their canonical
// names. Called automatically via package initialization; exported for
// tests that need to re-init with different Config values.
func Init(metaPrompt string, toonNotice string, isolated bool) {
	registry = make(map[string]Middleware)
	registeredContext = make(map[string]bool)

	// promptEngineering
	Register(MiddlewareFunc{
		name: "promptEngineering",
		fn: func(msgs []interface{}) ([]interface{}, error) {
			if isolated {
				return ApplyPromptEngineeringIsolated(msgs, metaPrompt), nil
			}
			return ApplyPromptEngineering(msgs, metaPrompt), nil
		},
	})

	// rag — registered as a ContextMiddleware placeholder; the real
	// RAG middleware is constructed via NewRAGMiddleware at startup
	// so it captures the store reference. We register a no-op here
	// so BuildChain doesn't fail when "rag" is in the chain spec.
	// The real work is done by the ContextAwareRAG field in Deps.
	Register(MiddlewareFunc{
		name: "rag",
		fn: func(msgs []interface{}) ([]interface{}, error) {
			return msgs, nil
		},
	})

	// compressJSONBlocks
	Register(MiddlewareFunc{
		name: "compressJSONBlocks",
		fn: func(msgs []interface{}) ([]interface{}, error) {
			CompressJSONBlocks(msgs)
			return msgs, nil
		},
	})

	// appendSystemNote
	Register(MiddlewareFunc{
		name: "appendSystemNote",
		fn: func(msgs []interface{}) ([]interface{}, error) {
			if isolated {
				return AppendSystemNoteIsolated(msgs, toonNotice), nil
			}
			return AppendSystemNote(msgs, toonNotice), nil
		},
	})
}

func init() {
	// Sensible defaults for init; main.go re-initializes with real
	// config values before building the chain.
	Init("", "", false)
}
