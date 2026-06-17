// HNSW (Hierarchical Navigable Small World) is the approximate nearest-neighbour
// index that replaces the brute-force linear scan over stored vectors (D2-T02).
//
// WHY a hand-rolled graph: the project core takes no new module dependencies
// (CLAUDE.md §2 invariant 6) and must stay cgo-free, so we cannot pull in an
// off-the-shelf ANN library. HNSW is the standard, well-understood choice — it
// gives logarithmic-ish search by layering progressively sparser "navigable
// small world" graphs over the points, so a query greedily descends from a
// sparse top layer to the dense base layer, visiting far fewer than N nodes.
//
// Distance is cosine *distance* (1 - cosine similarity); smaller is nearer. The
// graph is built entirely from the vectors SQLite hands us, in memory; it is
// deterministic given the same insertion order and a fixed RNG seed so tests are
// hermetic and reproducible.
package semantic

import (
	"container/heap"
	"math"
	"math/rand"
	"sort"
)

// HNSW tuning. These are conventional defaults: M neighbours per node on the
// upper layers (2*M on the base layer), with construction/search beam widths
// efConstruction/efSearch wide enough to keep recall high on the small-to-medium
// corpora this index sees. mL is the level-generation normalization 1/ln(M).
const (
	hnswM              = 16
	hnswMmax0          = 2 * hnswM // base-layer neighbour cap
	hnswEfConstruction = 200
	hnswEfSearch       = 64
)

// hnswNode is one indexed vector: its id, its coordinates, and its adjacency
// lists keyed by layer (neighbors[l] is the node's edges on layer l).
type hnswNode struct {
	id        string
	vec       []float32
	neighbors [][]int // neighbors[layer] = indices into graph.nodes
}

// hnswGraph is an in-memory HNSW over a fixed set of vectors. It is built once
// (Open/rebuild) and then queried read-only, so it needs no internal locking;
// the owning Index serializes rebuild vs. search.
type hnswGraph struct {
	nodes      []*hnswNode
	entryPoint int // index of the entry node, -1 when empty
	maxLayer   int
	mL         float64
	rng        *rand.Rand
}

// newHNSW builds a graph from parallel id/vector slices (same length, same
// order). The RNG is seeded deterministically so a given corpus always yields
// the same graph — important for reproducible tests and stable search results.
func newHNSW(ids []string, vecs [][]float32) *hnswGraph {
	g := &hnswGraph{
		entryPoint: -1,
		mL:         1.0 / math.Log(float64(hnswM)),
		rng:        rand.New(rand.NewSource(1)),
	}
	for i := range ids {
		g.insert(ids[i], vecs[i])
	}
	return g
}

// Len reports how many vectors the graph holds.
func (g *hnswGraph) Len() int { return len(g.nodes) }

// randomLayer draws an exponentially-decaying layer assignment: most nodes live
// only on layer 0, a few reach higher, which is what makes the upper layers
// sparse and the descent cheap.
func (g *hnswGraph) randomLayer() int {
	return int(-math.Log(g.rng.Float64()) * g.mL)
}

// insert adds one vector to the graph, wiring it into each layer up to its
// randomly-drawn top layer using the standard HNSW neighbour-selection routine.
func (g *hnswGraph) insert(id string, vec []float32) {
	nodeLayer := g.randomLayer()
	node := &hnswNode{
		id:        id,
		vec:       vec,
		neighbors: make([][]int, nodeLayer+1),
	}
	idx := len(g.nodes)
	g.nodes = append(g.nodes, node)

	// First node becomes the entry point; nothing to connect to.
	if g.entryPoint == -1 {
		g.entryPoint = idx
		g.maxLayer = nodeLayer
		return
	}

	// Phase 1: greedily descend from the top layer down to nodeLayer+1, using a
	// beam of width 1, just to find a good entry point for the layers we insert into.
	curr := g.entryPoint
	for l := g.maxLayer; l > nodeLayer; l-- {
		curr = g.greedyClosest(vec, curr, l)
	}

	// Phase 2: for each layer from min(maxLayer, nodeLayer) down to 0, search for
	// efConstruction candidates and connect the new node to its M best neighbours.
	entry := []int{curr}
	for l := min(g.maxLayer, nodeLayer); l >= 0; l-- {
		candidates := g.searchLayer(vec, entry, hnswEfConstruction, l, nil)
		mMax := hnswM
		if l == 0 {
			mMax = hnswMmax0
		}
		selected := g.selectNeighbors(vec, candidates, hnswM)
		node.neighbors[l] = selected

		// Add reciprocal edges and prune the neighbour back to its cap if needed.
		for _, n := range selected {
			g.nodes[n].neighbors[l] = append(g.nodes[n].neighbors[l], idx)
			if len(g.nodes[n].neighbors[l]) > mMax {
				g.nodes[n].neighbors[l] = g.selectNeighbors(
					g.nodes[n].vec, g.nodes[n].neighbors[l], mMax)
			}
		}
		// The candidate set seeds the next (denser) layer's search.
		entry = candidates
		if len(entry) == 0 {
			entry = []int{curr}
		}
	}

	if nodeLayer > g.maxLayer {
		g.maxLayer = nodeLayer
		g.entryPoint = idx
	}
}

// greedyClosest walks layer l from start, always hopping to the strictly closer
// neighbour, until no neighbour improves on the current node. Beam width 1.
func (g *hnswGraph) greedyClosest(query []float32, start, layer int) int {
	curr := start
	currDist := cosineDist(query, g.nodes[curr].vec)
	for {
		improved := false
		if layer < len(g.nodes[curr].neighbors) {
			for _, n := range g.nodes[curr].neighbors[layer] {
				d := cosineDist(query, g.nodes[n].vec)
				if d < currDist {
					currDist = d
					curr = n
					improved = true
				}
			}
		}
		if !improved {
			return curr
		}
	}
}

// searchLayer runs the core ef-beam best-first search on a single layer: it
// expands the closest unexplored candidate, tracking the ef nearest found so
// far, and stops once the frontier can no longer improve that set. When visited
// is non-nil every distinct node examined is recorded there (the test uses this
// to prove the search is sub-linear). Returns node indices, nearest first.
func (g *hnswGraph) searchLayer(query []float32, entry []int, ef, layer int, visited map[int]struct{}) []int {
	seen := make(map[int]bool, ef*2)

	// candidates: min-heap by distance (closest to expand next).
	// result: max-heap by distance (so we can evict the current farthest).
	cand := &distHeap{min: true}
	res := &distHeap{min: false}
	heap.Init(cand)
	heap.Init(res)

	for _, e := range entry {
		if seen[e] {
			continue
		}
		seen[e] = true
		if visited != nil {
			visited[e] = struct{}{}
		}
		d := cosineDist(query, g.nodes[e].vec)
		heap.Push(cand, distItem{idx: e, dist: d})
		heap.Push(res, distItem{idx: e, dist: d})
	}

	for cand.Len() > 0 {
		c := heap.Pop(cand).(distItem)
		// If the nearest remaining candidate is farther than the worst result and
		// we already have ef results, the frontier cannot improve — stop.
		if res.Len() >= ef && c.dist > res.peek().dist {
			break
		}
		if layer < len(g.nodes[c.idx].neighbors) {
			for _, n := range g.nodes[c.idx].neighbors[layer] {
				if seen[n] {
					continue
				}
				seen[n] = true
				if visited != nil {
					visited[n] = struct{}{}
				}
				d := cosineDist(query, g.nodes[n].vec)
				if res.Len() < ef || d < res.peek().dist {
					heap.Push(cand, distItem{idx: n, dist: d})
					heap.Push(res, distItem{idx: n, dist: d})
					if res.Len() > ef {
						heap.Pop(res)
					}
				}
			}
		}
	}

	// Drain the result max-heap and return nearest-first.
	out := make([]int, res.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(res).(distItem).idx
	}
	return out
}

// selectNeighbors picks the m closest of the candidate node indices to vec. This
// is the simple "select-by-distance" heuristic — adequate and deterministic for
// the corpus sizes this index serves.
func (g *hnswGraph) selectNeighbors(vec []float32, candidates []int, m int) []int {
	if len(candidates) <= m {
		// Copy so callers never alias the heap-owned backing array.
		out := make([]int, len(candidates))
		copy(out, candidates)
		return out
	}
	type cd struct {
		idx  int
		dist float64
	}
	scored := make([]cd, len(candidates))
	for i, c := range candidates {
		scored[i] = cd{idx: c, dist: cosineDist(vec, g.nodes[c].vec)}
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].dist != scored[j].dist {
			return scored[i].dist < scored[j].dist
		}
		return scored[i].idx < scored[j].idx
	})
	out := make([]int, m)
	for i := 0; i < m; i++ {
		out[i] = scored[i].idx
	}
	return out
}

// search returns up to k nearest neighbours to query as Hits (cosine similarity
// score, higher = nearer), and the count of distinct nodes the search visited.
// The visited count is returned so tests can assert sub-linearity vs. N.
func (g *hnswGraph) search(query []float32, k int) ([]Hit, int) {
	if g.entryPoint == -1 || k <= 0 {
		return nil, 0
	}
	visited := make(map[int]struct{})

	// Descend the sparse upper layers with a width-1 beam to a good base entry.
	curr := g.entryPoint
	for l := g.maxLayer; l > 0; l-- {
		visited[curr] = struct{}{}
		curr = g.greedyClosest(query, curr, l)
	}

	ef := hnswEfSearch
	if ef < k {
		ef = k
	}
	found := g.searchLayer(query, []int{curr}, ef, 0, visited)

	if len(found) > k {
		found = found[:k]
	}
	hits := make([]Hit, len(found))
	for i, idx := range found {
		// Convert distance back to the similarity score the public API reports.
		hits[i] = Hit{ID: g.nodes[idx].id, Score: 1 - cosineDist(query, g.nodes[idx].vec)}
	}
	return hits, len(visited)
}

// cosineDist is the cosine *distance*: 1 - cosine similarity, so 0 is identical
// and 2 is opposite. Mismatched or zero-magnitude vectors are treated as maximal
// distance (1) — they cannot be meaningfully ranked.
func cosineDist(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 1
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}

// distItem / distHeap is a small index+distance priority queue used by
// searchLayer. min=true makes it a min-heap (closest first); min=false a
// max-heap (farthest first).
type distItem struct {
	idx  int
	dist float64
}

type distHeap struct {
	items []distItem
	min   bool
}

func (h *distHeap) Len() int { return len(h.items) }
func (h *distHeap) Less(i, j int) bool {
	if h.min {
		return h.items[i].dist < h.items[j].dist
	}
	return h.items[i].dist > h.items[j].dist
}
func (h *distHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *distHeap) Push(x any)    { h.items = append(h.items, x.(distItem)) }
func (h *distHeap) Pop() any {
	old := h.items
	n := len(old)
	it := old[n-1]
	h.items = old[:n-1]
	return it
}

// peek returns the heap root without removing it.
func (h *distHeap) peek() distItem { return h.items[0] }
