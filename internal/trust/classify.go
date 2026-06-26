package trust

// classify.go — deterministic task-class bucketing (Phase 16, RTE-T01).
//
// Classify folds a free-text task goal into one of a small, fixed set of
// task-class labels by ordered keyword matching. It is the key the per-class
// trust cell (a later RTE task) keys its routing evidence on: "how has this
// backend/model done on REFACTOR tasks" needs a stable, model-free bucket for
// every incoming goal. So Classify is deliberately:
//
//   - PURE: no IO, no model call, no state — stdlib `strings` only. The same
//     goal always maps to the same label, so the routing cell is reproducible
//     and replayable from the log (it never has to re-ask a model what class a
//     past task was).
//   - FIRST-MATCH-WINS over an ORDERED table: a goal mentioning several class
//     words resolves to the FIRST class in table order, so the bucket is a
//     single deterministic function of the goal, not order-of-keyword-in-goal.
//     Order the table most-specific/most-actionable first.
//   - NON-DECIDING: a class is only a routing hint. It never judges work, never
//     gates, never decides "done" — the verifier still does all of that (I2).
//     Classify carries no authority; mis-bucketing only biases attempt order.
//
// The label set is intentionally TINY and closed (documented on ClassOther and
// the table below). Adding a label is a deliberate, reviewed change, because the
// per-class cell dimension keys on exactly these strings.

import "strings"

// The closed set of task-class labels Classify can return. Keep this set small
// and stable: each label is a routing-cell key, so renaming or removing one
// orphans accumulated evidence. "other" is the catch-all default for any goal
// that matches no keyword.
const (
	ClassRefactor   = "refactor"   // restructure/clean up existing code, no behaviour change intended
	ClassBugfix     = "bugfix"     // fix a defect / regression / broken behaviour
	ClassTest       = "test"       // add or repair tests / coverage
	ClassDocs       = "docs"       // documentation, comments, README, changelog
	ClassGreenfield = "greenfield" // brand-new project/module from scratch
	ClassFeature    = "feature"    // add new behaviour to existing code
	ClassResearch   = "research"   // investigate / analyse / explore, no committed change
	ClassOther      = "other"      // default: matched no keyword above
)

// classRule pairs a class label with the lower-cased keyword substrings that
// select it. A goal selects the rule if it contains ANY of the rule's keywords.
type classRule struct {
	class    string
	keywords []string
}

// classTable is the ORDERED keyword table; Classify returns the class of the
// FIRST rule whose keyword the (normalized) goal contains. Order matters and is
// chosen for determinism + intent:
//
//   - refactor / bugfix / test / docs first: these are the most specific,
//     "act on existing code" intents and read as the dominant verb of a goal
//     ("refactor X by adding a test" is a refactor, not a test task).
//   - greenfield before feature: "build a new X from scratch" is greenfield,
//     even though "build"/"new" also smell like feature work.
//   - feature last among change-classes: it is the broadest "add behaviour"
//     bucket, so it only wins when no narrower class matched.
//   - research last: it is the no-committed-change bucket; a goal that also
//     names a concrete change above is classified by that change.
//
// Keywords are lower-cased; Classify lower-cases and whitespace-normalizes the
// goal before matching, so matching is case-insensitive and whitespace-robust.
var classTable = []classRule{
	{ClassRefactor, []string{"refactor", "restructure", "clean up", "cleanup", "rename", "simplify", "deduplicate", "tidy"}},
	{ClassBugfix, []string{"bugfix", "bug fix", "fix the bug", "fix bug", "fix a bug", "regression", "broken", "crash", "defect", "hotfix", "patch the", "fix the", "fixes ", "fix "}},
	{ClassTest, []string{"unit test", "integration test", "add test", "add a test", "write test", "write a test", "test coverage", "coverage for", "tests for", " tests", "testing"}},
	{ClassDocs, []string{"document", "documentation", "docstring", "readme", "changelog", "comment", "javadoc", "godoc", " docs", "docs ", "tutorial"}},
	{ClassGreenfield, []string{"greenfield", "from scratch", "brand new", "bootstrap", "scaffold", "new project", "new repo", "new repository", "initial commit", "set up a new", "create a new project"}},
	{ClassFeature, []string{"feature", "implement", "add support", "add a ", "add an ", "introduce", "new endpoint", "new command", "build a", "build an", "create a", "support for"}},
	{ClassResearch, []string{"research", "investigate", "analyze", "analyse", "explore", "evaluate", "compare", "spike", "find out", "look into", "understand"}},
}

// Classify buckets a free-text task goal into one of the fixed task-class labels
// above by ordered, case-insensitive keyword matching. It is pure and
// deterministic: identical goals always map to identical labels with no model
// call, no IO, and no state. Matching is FIRST-RULE-WINS over classTable, so a
// goal that smells of several classes resolves to the first matching rule in
// table order. A goal that matches no keyword — including the empty string —
// returns ClassOther.
//
// The returned label is a routing HINT only: it keys the per-class trust cell,
// it never judges work or decides shipping (I2). A wrong bucket only biases
// which backend is tried first; the verifier still governs "done".
func Classify(goal string) string {
	g := normalize(goal)
	if g == "" {
		return ClassOther
	}
	for _, rule := range classTable {
		for _, kw := range rule.keywords {
			if strings.Contains(g, kw) {
				return rule.class
			}
		}
	}
	return ClassOther
}

// normalize lower-cases the goal and collapses every run of whitespace to a
// single space, so matching is case-insensitive and robust to tabs/newlines and
// irregular spacing. strings.Fields splits on any Unicode whitespace, which also
// trims leading/trailing whitespace.
func normalize(goal string) string {
	return strings.ToLower(strings.Join(strings.Fields(goal), " "))
}
