// Package verb is the conversational front door's "thinking" microcopy engine: a
// braille spinner frame plus a cycling present-participle verb ("Cogitating…",
// "Recombobulating…") that tells a watching user the agent is alive and working.
//
// It is deterministic by construction — frame and verb are pure functions of the
// elapsed duration and a per-spinner seed, with no shared RNG — so a given
// session renders a reproducible sequence (a hard test property) and two
// concurrent spinners never desync. Stdlib only (invariant I6).
package verb

import (
	"hash/fnv"
	"time"
)

// Frame cadence and verb cadence. The braille frame advances quickly so the
// motion reads as alive; the verb switches slowly so it is readable, not a blur.
const (
	frameEvery = 80 * time.Millisecond
	verbEvery  = 4 * time.Second
)

// frames is the braille spinner cycle — the same ten glyphs that read as a smooth
// rotation in a monospace terminal.
var frames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// Category buckets the verbs so the word fits the work the agent is doing. The
// general bucket (the full list) is the fallback when a category is unset.
type Category int

const (
	General   Category = iota // anything / unknown — the full list
	Native                    // a focused code change (the native loop)
	Supervise                 // coordinating subagents (the supervisor)
	Project                   // building a whole thing (the project loop)
	Chat                      // just answering — no loop, no worktree
)

// Spinner renders the thinking line deterministically from a seed and a category.
// The zero value is a valid General spinner with seed 0.
type Spinner struct {
	seed uint64
	cat  Category
}

// New returns a Spinner seeded by seed (e.g. a hash of the session id + step) so
// different turns and different sessions cycle through different words, while any
// single (seed, elapsed) pair is reproducible.
func New(seed uint64, cat Category) Spinner { return Spinner{seed: seed, cat: cat} }

// Frame returns the braille glyph for the given elapsed time. It advances every
// frameEvery, looping over the cycle.
func (s Spinner) Frame(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	return string(frames[int(elapsed/frameEvery)%len(frames)])
}

// Verb returns the present-participle verb for the given elapsed time (no
// trailing ellipsis — the caller adds "…"). It switches every verbEvery; which
// word a given time bucket maps to is a stable hash of (seed, bucket), so the
// sequence is reproducible and category-appropriate.
func (s Spinner) Verb(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	list := byCategory(s.cat)
	bucket := uint64(elapsed / verbEvery)
	return list[mix(s.seed, bucket)%uint64(len(list))]
}

// mix is a tiny, stable 64-bit hash of (seed, bucket) via FNV-1a (stdlib), so the
// verb sequence is deterministic without a shared/global RNG.
func mix(seed, bucket uint64) uint64 {
	h := fnv.New64a()
	var b [16]byte
	putU64(b[0:8], seed)
	putU64(b[8:16], bucket)
	_, _ = h.Write(b[:])
	return h.Sum64()
}

func putU64(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
}

// byCategory returns the verb list for a category, defaulting to the full list.
func byCategory(c Category) []string {
	switch c {
	case Native:
		return nativeVerbs
	case Supervise:
		return superviseVerbs
	case Project:
		return projectVerbs
	case Chat:
		return chatVerbs
	default:
		return allVerbs
	}
}

// Curated buckets give each route its own flavour; every word is drawn from the
// full list below, so they stay on-brand.
var (
	nativeVerbs    = []string{"Crafting", "Forging", "Tinkering", "Hashing", "Bootstrapping", "Composing", "Whisking", "Kneading", "Tempering", "Wrangling", "Finagling", "Smooshing"}
	superviseVerbs = []string{"Orchestrating", "Herding", "Choreographing", "Harmonizing", "Coalescing", "Marshalling", "Mustering", "Channeling", "Conducting", "Synchronizing"}
	projectVerbs   = []string{"Architecting", "Manifesting", "Cultivating", "Crystallizing", "Germinating", "Incubating", "Scaffolding", "Bootstrapping", "Constructing", "Materializing"}
	chatVerbs      = []string{"Cogitating", "Pondering", "Ruminating", "Considering", "Deliberating", "Musing", "Contemplating", "Mulling", "Inferring", "Reasoning"}
)

// allVerbs is the full present-participle list — the General bucket and the
// canonical source the curated buckets draw from.
var allVerbs = []string{
	"Accomplishing", "Actioning", "Actualizing", "Architecting", "Baking", "Beaming",
	"Befuddling", "Billowing", "Blanching", "Bloviating", "Boondoggling", "Booping",
	"Bootstrapping", "Brewing", "Burrowing", "Calculating", "Caramelizing", "Cascading",
	"Catapulting", "Cerebrating", "Channeling", "Channelling", "Choreographing", "Churning",
	"Clauding", "Coalescing", "Cogitating", "Combobulating", "Composing", "Computing",
	"Concocting", "Considering", "Contemplating", "Cooking", "Crafting", "Creating",
	"Crunching", "Crystallizing", "Cultivating", "Deciphering", "Deliberating", "Determining",
	"Discombobulating", "Doing", "Doodling", "Drizzling", "Effecting", "Elucidating",
	"Embellishing", "Enchanting", "Envisioning", "Evaporating", "Fermenting", "Finagling",
	"Flowing", "Flummoxing", "Fluttering", "Forging", "Forming", "Frolicking", "Frosting",
	"Galloping", "Garnishing", "Generating", "Germinating", "Gesticulating", "Grooving",
	"Harmonizing", "Hashing", "Hatching", "Herding", "Hyperspacing", "Ideating", "Imagining",
	"Improvising", "Incubating", "Inferring", "Infusing", "Ionizing", "Kneading", "Leavening",
	"Levitating", "Manifesting", "Marinating", "Meandering", "Metamorphosing", "Misting",
	"Mulling", "Musing", "Mustering", "Nebulizing", "Nesting", "Noodling", "Nucleating",
	"Orbiting", "Orchestrating", "Osmosing", "Percolating", "Perusing", "Philosophising",
	"Photosynthesizing", "Pollinating", "Pondering", "Pontificating", "Pouncing",
	"Precipitating", "Prestidigitating", "Processing", "Proofing", "Propagating", "Puttering",
	"Puzzling", "Quantumizing", "Recombobulating", "Reticulating", "Roosting", "Ruminating",
	"Sautéing", "Scampering", "Schlepping", "Scurrying", "Seasoning", "Shimmying", "Simmering",
	"Sketching", "Slithering", "Smooshing", "Spelunking", "Spinning", "Sprouting", "Stewing",
	"Sublimating", "Swirling", "Swooping", "Synthesizing", "Tempering", "Thinking", "Tinkering",
	"Transfiguring", "Transmuting", "Twisting", "Undulating", "Unfurling", "Unravelling",
	"Vibing", "Wandering", "Warping", "Whirring", "Whisking", "Working", "Wrangling", "Zesting",
	"Zigzagging",
}
