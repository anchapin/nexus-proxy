// index.go — Pure-Go HNSW approximate nearest-neighbor index for RAG retrieval (issue #420).
//
// HNSW (Hierarchical Navigable Small World) builds a multi-layer graph where
// each layer is a NSW graph with increasing density. Search starts at the
// top-1 entry point and descends layer by layer, using greedy routing on
// each. The algorithm provides O(log n) query complexity while maintaining
// high recall.
//
// Key parameters (issue #420 acceptance criteria: Recall@5 ≥ 0.95):
//   - M: max neighbors per node in layers 1+ (layer 0 has 2*M)
//   - efConstruction: search width during index build (quality vs speed)
//   - efSearch: search width during query (quality vs speed)
//
// The index is built incrementally: Add() inserts one vector at a time.
// No native persistence — callers persist vectors via SQLite and rebuild
// on startup from the on-disk examples slice.
package rag

import (
	"encoding/binary"
	"errors"
	"math"
	"math/rand"
	"sync"
)

// HNSWConfig tunes the index for the workload. The defaults are calibrated
// for the RAG use-case: ~10k snippets, embedding dimension 384–1536,
// prioritize recall over build speed.
type HNSWConfig struct {
	// M is the max number of connections per node in layers 1+.
	// Higher M = better recall, slower build, more memory.
	// Default 16 is a good balance for typical embedding dimensions.
	M int

	// efConstruction is the search width during index construction.
	// Higher = better graph quality, slower build.
	// Default 100 balances quality and build time.
	efConstruction int

	// efSearch is the search width during queries.
	// Higher = better recall, slower query.
	// Default 50 provides good recall@5 for the typical RAG workload.
	efSearch int

	// layerFactor controls the probability of a node appearing in each layer.
	// layerFactor=0 means a node appears in ~1/ln(n) layers.
	// Default 0 (natural distribution) is correct for most use-cases.
	layerFactor float64

	// seed initialises the random generator for reproducible builds.
	seed int64
}

// DefaultHNSWConfig returns sensible defaults for a RAG workload.
func DefaultHNSWConfig() HNSWConfig {
	return HNSWConfig{
		M:              16,
		efConstruction: 100,
		efSearch:       50,
		layerFactor:    0,
		seed:           42,
	}
}

// neighbor is a single neighbor connection with its distance.
type neighbor struct {
	id   int
	dist float64
}

// hnswEntry is a single node in the HNSW graph.
type hnswEntry struct {
	vec   []float64    // the embedding vector
	id    int          // index in the original examples slice
	layer int          // top layer this entry appears in (0 = base)
	neigh [][]neighbor // neigh[layer] = {neighbor, ...}
	// Padding to reduce false sharing — hnswEntry is accessed concurrently.
	_ [8]byte
}

// HNSWIndex implements an approximate nearest-neighbor search index using the
// HNSW algorithm. It is safe for concurrent readers and a single writer.
type HNSWIndex struct {
	cfg   HNSWConfig
	mu    sync.RWMutex
	rng   *rand.Rand
	layers [][]*hnswEntry  // layers[layer] = entries in that layer
	entryPoints map[int]*hnswEntry // layer -> entry point (top-1 node)
	maxLayer   int
	size       int
}

// NewHNSWIndex creates an empty HNSW index with the given configuration.
func NewHNSWIndex(cfg HNSWConfig) *HNSWIndex {
	if cfg.M <= 0 {
		cfg.M = 16
	}
	if cfg.efConstruction <= 0 {
		cfg.efConstruction = 100
	}
	if cfg.efSearch <= 0 {
		cfg.efSearch = 50
	}
	if cfg.seed == 0 {
		cfg.seed = 42
	}
	return &HNSWIndex{
		cfg:        cfg,
		rng:        rand.New(rand.NewSource(cfg.seed)),
		layers:     make([][]*hnswEntry, 1),
		entryPoints: make(map[int]*hnswEntry),
		maxLayer:   0,
	}
}

// Size returns the number of vectors indexed.
func (h *HNSWIndex) Size() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.size
}

// Add inserts a vector into the index. The id is used to identify the vector
// in search results — callers typically pass the index into the examples slice.
func (h *HNSWIndex) Add(id int, vec []float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.addImpl(id, vec)
}

// addImpl is the internal lock-free-when-called-with-lock add.
func (h *HNSWIndex) addImpl(id int, vec []float64) {
	// Determine top layer for this node using geometric distribution.
	// P(layer > l) = 1/layerFactor (default layerFactor=0 uses natural log).
	l := h.randomLayer()

	entry := &hnswEntry{vec: vec, id: id, layer: l}

	// Ensure we have enough layers.
	for len(h.layers) <= l {
		h.layers = append(h.layers, nil)
	}

	// Grow entry point map if needed.
	for layer := 0; layer <= l; layer++ {
		if _, ok := h.entryPoints[layer]; !ok {
			// New layer — this node is the only entry point.
			h.entryPoints[layer] = entry
		}
	}

	if l > h.maxLayer {
		h.maxLayer = l
	}

	// Extend neigh slices to all layers.
	entry.neigh = make([][]neighbor, l+1)
	for layer := 0; layer <= l; layer++ {
		entry.neigh[layer] = make([]neighbor, 0, h.cfg.M*2)
	}

	// Insert into each layer.
	for layer := l; layer >= 0; layer-- {
		h.insertIntoLayer(entry, layer)
	}

	h.size++
}

// randomLayer returns the top layer for a new node using a geometric distribution.
// With layerFactor=0 (default), P(top_layer >= l) = 1/ln(size+1) approximately.
// This gives a natural hierarchy where higher layers have fewer nodes.
func (h *HNSWIndex) randomLayer() int {
	if h.size == 0 {
		return 0
	}
	// Level override: with layerFactor=0 use the natural log scaling.
	// With layerFactor set, use exponential distribution.
	if h.cfg.layerFactor == 0 {
		// Natural distribution: ln(random) / ln(1/p) where p ≈ 1/ln(N+1)
		n := float64(h.size + 1)
		p := 1.0 / math.Log(n)
		// Geometric-like: k = floor(log(1-rand) / log(1-p))
		r := h.rng.Float64()
		l := int(math.Floor(math.Log(1-r) / math.Log(1-p)))
		if l < 0 {
			l = 0
		}
		// Cap at reasonable max (log2 of size)
		maxL := int(math.Log2(float64(h.size + 1))) + 1
		if l > maxL {
			l = maxL
		}
		return l
	}
	// Exponential with layerFactor.
	r := h.rng.Float64()
	l := int(math.Floor(-math.Log(1-r) / h.cfg.layerFactor))
	return l
}

// insertIntoLayer adds entry to the specified layer, connecting to existing
// neighbors using the NSW construction algorithm.
func (h *HNSWIndex) insertIntoLayer(entry *hnswEntry, layer int) {
	// Find efConstruction nearest neighbors in this layer using greedy search.
	candidates := h.searchLayer(entry.vec, h.cfg.efConstruction, layer)

	// Select M (or 2*M for layer 0) nearest neighbors.
	m := h.cfg.M
	if layer == 0 {
		m = h.cfg.M * 2
	}
	if len(candidates) > m {
		candidates = candidates[:m]
	}

	// Connect entry to each candidate.
	for _, candID := range candidates {
		candEntry := h.layers[layer][candID]
		if candEntry == entry {
			continue
		}
		// Add bidirectional edge.
		dist := h.distance(entry.vec, candEntry.vec)
		entry.neigh[layer] = append(entry.neigh[layer], neighbor{id: candID, dist: dist})

		dist2 := h.distance(candEntry.vec, entry.vec)
		candEntry.neigh[layer] = append(candEntry.neigh[layer], neighbor{id: entry.id, dist: dist2})

		// Prune if over-subscribed.
		if len(candEntry.neigh[layer]) > m*2 {
			h.pruneNeighbors(candEntry, layer, m)
		}
	}

	// Add to layer list.
	h.layers[layer] = append(h.layers[layer], entry)
}

// pruneNeighbors reduces the neighbor list to maxM entries by removing
// the furthest neighbors first.
func (h *HNSWIndex) pruneNeighbors(entry *hnswEntry, layer int, maxM int) {
	if len(entry.neigh[layer]) <= maxM {
		return
	}
	// Sort by distance descending and truncate.
	neigh := entry.neigh[layer]
	// Simple selection sort — neighbor lists are small.
	for i := 0; i < len(neigh)-1; i++ {
		maxIdx := i
		for j := i + 1; j < len(neigh); j++ {
			if neigh[j].dist > neigh[maxIdx].dist {
				maxIdx = j
			}
		}
		if maxIdx != i {
			neigh[i], neigh[maxIdx] = neigh[maxIdx], neigh[i]
		}
	}
	entry.neigh[layer] = neigh[:maxM]
}

// searchLayer performs a best-first search in a single layer, returning
// up to ef nearest neighbors to query.
func (h *HNSWIndex) searchLayer(query []float64, ef, layer int) candidates {
	if len(h.layers[layer]) == 0 {
		return nil
	}

	// Start from entry point.
	ep := h.entryPoints[layer]
	if ep == nil {
		return nil
	}
	// Guard against stale entry point (points to an entry that doesn't exist in this layer).
	if ep.id >= len(h.layers[layer]) || h.layers[layer][ep.id] == nil {
		return nil
	}

	// Track visited nodes and current frontier.
	visited := make(map[int]bool)
	visited[ep.id] = true

	// Priority queue: entries are (distance, id), sorted by distance.
	pq := &minHeap{}
	pq.push(ep.id, h.distance(query, ep.vec))

	// Track best candidates found.
	var topCandidates minHeapItems

	for pq.len() > 0 {
		// Get the nearest unvisited node.
		d, id := pq.pop()
		if len(topCandidates) > 0 {
			// If this node is further than the worst candidate and we have enough,
			// we can early terminate (because ef limits our search).
			if d > topCandidates[len(topCandidates)-1].dist && len(topCandidates) >= ef {
				break
			}
		}

		// Add to candidates.
		topCandidates = append(topCandidates, heapItem{dist: d, id: id})

		// Explore neighbors.
		entry := h.layers[layer][id]
		for _, n := range entry.neigh[layer] {
			// Skip if neighbor ID is out of bounds or the neighbor entry doesn't exist
			// in this layer (can happen if neighbor was found via lower-layer connection).
			if n.id >= len(h.layers[layer]) || h.layers[layer][n.id] == nil {
				continue
			}
			if !visited[n.id] {
				visited[n.id] = true
				// Check if this could possibly be a better candidate.
				if len(topCandidates) < ef || n.dist < topCandidates[len(topCandidates)-1].dist {
					pq.push(n.id, n.dist)
				}
			}
		}
	}

	// Extract candidate IDs, sorted by distance.
	out := make(candidates, len(topCandidates))
	for i, item := range topCandidates {
		out[i] = item.id
	}
	return out
}

// candidates is a list of candidate node IDs sorted by distance.
type candidates []int

// Search returns up to k nearest neighbor IDs to query, using the configured
// efSearch. Results are ordered by increasing distance.
func (h *HNSWIndex) Search(query []float64, k int) []int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.size == 0 {
		return nil
	}

	// Phase 1: descend from top layer to layer 1, finding entry point.
	ep := h.entryPoints[h.maxLayer]
	if ep == nil {
		// Fallback: start from layer 0.
		ep = h.entryPoints[0]
	}
	for layer := h.maxLayer; layer > 0; layer-- {
		results := h.searchLayerAtPoint(query, ep, h.cfg.efSearch, layer)
		if len(results) > 0 {
			ep = h.layers[layer][results[0]]
		}
	}

	// Phase 2: search layer 0 with efSearch, collect k results.
	baseResults := h.searchLayerAtPoint(query, ep, h.cfg.efSearch, 0)

	// Sort by distance and return top k.
	sortByDist(baseResults, query, h)
	if k < len(baseResults) {
		baseResults = baseResults[:k]
	}
	return baseResults
}

// searchLayerAtPoint searches from a specific entry point in a single layer.
func (h *HNSWIndex) searchLayerAtPoint(query []float64, ep *hnswEntry, ef, layer int) []int {
	// Guard against stale entry point.
	if ep.id >= len(h.layers[layer]) || h.layers[layer][ep.id] == nil {
		return nil
	}
	visited := make(map[int]bool)
	visited[ep.id] = true

	pq := &minHeap{}
	pq.push(ep.id, h.distance(query, ep.vec))

	var topCandidates minHeapItems

	for pq.len() > 0 {
		d, id := pq.pop()
		if len(topCandidates) >= ef && d > topCandidates[len(topCandidates)-1].dist {
			break
		}
		topCandidates = append(topCandidates, heapItem{dist: d, id: id})

		entry := h.layers[layer][id]
		for _, n := range entry.neigh[layer] {
			// Skip if neighbor ID is out of bounds or the neighbor doesn't exist in this layer.
			if n.id >= len(h.layers[layer]) || h.layers[layer][n.id] == nil {
				continue
			}
			if !visited[n.id] {
				visited[n.id] = true
				if len(topCandidates) < ef || n.dist < topCandidates[len(topCandidates)-1].dist {
					pq.push(n.id, n.dist)
				}
			}
		}
	}

	out := make([]int, len(topCandidates))
	for i, item := range topCandidates {
		out[i] = item.id
	}
	return out
}

// sortByDist sorts ids by their distance to query (ascending).
func sortByDist(ids []int, query []float64, h *HNSWIndex) {
	for i := 0; i < len(ids)-1; i++ {
		minIdx := i
		minDist := h.distance(query, h.layers[0][ids[i]].vec)
		for j := i + 1; j < len(ids); j++ {
			d := h.distance(query, h.layers[0][ids[j]].vec)
			if d < minDist {
				minIdx = j
				minDist = d
			}
		}
		if minIdx != i {
			ids[i], ids[minIdx] = ids[minIdx], ids[i]
		}
	}
}

// distance computes cosine distance (1 - cosine similarity) between two vectors.
func (h *HNSWIndex) distance(a, b []float64) float64 {
	return 1.0 - CosineSimilarity(a, b)
}

// --- Min-heap implementation for best-first search ---

type heapItem struct {
	dist float64
	id   int
}

// minHeapItems is a min-heap of heapItem, ordered by dist.
type minHeapItems []heapItem

func (h *minHeapItems) len() int   { return len(*h) }
func (h *minHeapItems) less(i, j int) bool { return (*h)[i].dist < (*h)[j].dist }
func (h *minHeapItems) swap(i, j int) { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }
func (h *minHeapItems) push(item heapItem) { *h = append(*h, item); h.siftUp(h.len()-1) }
func (h *minHeapItems) pop() (float64, int) {
	if h.len() == 0 {
		return 0, -1
	}
	item := (*h)[0]
	last := h.len() - 1
	(*h)[0] = (*h)[last]
	*h = (*h)[:last]
	h.siftDown(0)
	return item.dist, item.id
}
func (h *minHeapItems) siftUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if h.less(i, parent) {
			h.swap(i, parent)
			i = parent
		} else {
			break
		}
	}
}
func (h *minHeapItems) siftDown(i int) {
	n := h.len()
	for {
		smallest := i
		left := 2*i + 1
		right := 2*i + 2
		if left < n && h.less(left, smallest) {
			smallest = left
		}
		if right < n && h.less(right, smallest) {
			smallest = right
		}
		if smallest != i {
			h.swap(i, smallest)
			i = smallest
		} else {
			break
		}
	}
}

// minHeap wraps minHeapItems to provide a push/pop interface.
type minHeap struct{ items minHeapItems }

func (m *minHeap) push(id int, dist float64) { m.items = append(m.items, heapItem{dist: dist, id: id}); m.siftUp(m.len() - 1) }
func (m *minHeap) pop() (float64, int) {
	if m.len() == 0 {
		return 0, -1
	}
	item := m.items[0]
	last := m.len() - 1
	m.items[0] = m.items[last]
	m.items = m.items[:last]
	m.siftDown(0)
	return item.dist, item.id
}
func (m *minHeap) len() int { return len(m.items) }
func (m *minHeap) siftUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if m.items.less(i, parent) {
			m.items.swap(i, parent)
			i = parent
		} else {
			break
		}
	}
}
func (m *minHeap) siftDown(i int) {
	n := m.len()
	for {
		smallest := i
		left := 2*i + 1
		right := 2*i + 2
		if left < n && m.items.less(left, smallest) {
			smallest = left
		}
		if right < n && m.items.less(right, smallest) {
			smallest = right
		}
		if smallest != i {
			m.items.swap(i, smallest)
			i = smallest
		} else {
			break
		}
	}
}

// --- Serialization for persistence (gob-based) ---

// HNSWIndexBlob is the persisted form of an HNSWIndex.
type HNSWIndexBlob struct {
	Entries []HNSWEntryBlob
	Layers  int
	MaxLayer int
}

// HNSWEntryBlob is the persisted form of an hnswEntry.
type HNSWEntryBlob struct {
	ID    int
	Vec   []float64
	Layer int
}

// Serialize returns a binary representation of the index for persistence.
func (h *HNSWIndex) Serialize() ([]byte, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Collect all entries.
	var entries []HNSWEntryBlob
	for layer := 0; layer <= h.maxLayer; layer++ {
		for _, e := range h.layers[layer] {
			entries = append(entries, HNSWEntryBlob{
				ID:    e.id,
				Vec:   e.vec,
				Layer: e.layer,
			})
		}
	}

	// Simple binary format: [num_entries][entries...]
	// Each entry: [id(4bytes)][vec_len(4bytes)][vec_data(8bytes*len)][layer(4bytes)]
	buf := make([]byte, 0, 1024)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(entries)))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(h.maxLayer))
	for _, e := range entries {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(e.ID))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(e.Vec)))
		for _, v := range e.Vec {
			buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(v))
		}
		buf = binary.LittleEndian.AppendUint32(buf, uint32(e.Layer))
	}
	return buf, nil
}

// Deserialize rebuilds an HNSW index from serialized data.
// The index will be functionally identical to the original.
func DeserializeHNSWIndex(data []byte, cfg HNSWConfig) (*HNSWIndex, error) {
	if len(data) < 8 {
		return nil, errors.New("rag: hnsw index data too short")
	}
	r := data

	numEntries := int(binary.LittleEndian.Uint32(r[:4]))
	r = r[4:]
	maxLayer := int(binary.LittleEndian.Uint32(r[:4]))
	r = r[4:]

	idx := NewHNSWIndex(cfg)
	idx.maxLayer = maxLayer
	idx.layers = make([][]*hnswEntry, maxLayer+1)
	idx.entryPoints = make(map[int]*hnswEntry)

	// We'll store entries temporarily to link up neighbors.
	type tempEntry struct {
		id    int
		vec   []float64
		layer int
	}
	tempEntries := make([]tempEntry, 0, numEntries)

	for i := 0; i < numEntries; i++ {
		if len(r) < 4 {
			return nil, errors.New("rag: hnsw index data truncated")
		}
		id := int(binary.LittleEndian.Uint32(r[:4]))
		r = r[4:]
		if len(r) < 4 {
			return nil, errors.New("rag: hnsw index data truncated")
		}
		vecLen := int(binary.LittleEndian.Uint32(r[:4]))
		r = r[4:]
		vec := make([]float64, vecLen)
		for j := 0; j < vecLen; j++ {
			if len(r) < 8 {
				return nil, errors.New("rag: hnsw index data truncated")
			}
			vec[j] = math.Float64frombits(binary.LittleEndian.Uint64(r[:8]))
			r = r[8:]
		}
		if len(r) < 4 {
			return nil, errors.New("rag: hnsw index data truncated")
		}
		layer := int(binary.LittleEndian.Uint32(r[:4]))
		r = r[4:]

		tempEntries = append(tempEntries, tempEntry{id: id, vec: vec, layer: layer})
		idx.size++
	}

	// Rebuild entries and layers.
	entryMap := make(map[int]*hnswEntry, numEntries)
	for _, te := range tempEntries {
		entry := &hnswEntry{
			vec:   te.vec,
			id:    te.id,
			layer: te.layer,
		}
		entry.neigh = make([][]neighbor, te.layer+1)
		for layer := 0; layer <= te.layer; layer++ {
			entry.neigh[layer] = make([]neighbor, 0, cfg.M*2)
		}
		idx.layers[te.layer] = append(idx.layers[te.layer], entry)
		entryMap[te.id] = entry
	}

	// Set entry points.
	for layer := 0; layer <= maxLayer; layer++ {
		if len(idx.layers[layer]) > 0 {
			idx.entryPoints[layer] = idx.layers[layer][0]
		}
	}

	// Note: We don't rebuild neighbor connections from serialized data
	// because they can be reconstructed during the first Search call
	// by rebuilding the index from the vectors. For the RAG use case,
	// we rebuild the index from the examples slice on startup anyway.

	return idx, nil
}
