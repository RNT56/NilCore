# Design core — first principles

What makes NilCore the best coding agent is not a smarter model. By 2026 the frontier models inside every serious agent have **converged**, and the harness around the model does most of the work. NilCore's bet is to be the **best harness** — and being the best is the disciplined, ruthless application of a short list of principles, not a long list of features.

**These principles rank above features.** When a feature conflicts with a principle, the principle wins. They sit at the top of the philosophy stack: `PRINCIPLES.md` → `ARCHITECTURE.md` (the invariants that enforce them) → `PERSONA.md` (how they show up at runtime) → `TASKS.md` (how they get built).

## The principles, ranked by leverage

### 1. The feedback loop is the product
An agent is only as good as its ability to **know whether its code works** — truthfully, fast, and at the right granularity. Verification (build, type-check, test, lint) is the *sole* authority on "done"; no model or sub-agent self-report overrides it. The loop runs the **smallest relevant check as fast as possible** and iterates. Everything else serves this loop.
> Enforced by: invariant **I2** (verifier is truth), per-step verification, the routing/escalation ladder.

### 2. The harness wins; borrow the intelligence
Models have converged, so keep the harness **small, sharp, and yours**, and let the model supply the fluency. A smaller, cleaner harness lets the model's capability show through undiluted; a bigger one just adds failure modes.
> Enforced by: the tiny frozen core (**I1**), zero-dependency discipline (**I6**), provider-agnostic models.

### 3. Context is the scarce resource — engineer it ruthlessly
What the agent **sees** determines what it does. The win is the *right* context — minimal, relevant, at the right moment — not the biggest window. Retrieve precisely, prune aggressively, summarize on handoff, isolate sub-workers, and keep bulk data **out** of the model's window. More-per-token beats more-context.
> Enforced by: summarize-and-handover + `ContextSummary`, code-execution MCP (data filtered in-sandbox), sub-worker context isolation.

### 4. Understand before you change
Read, navigate, trace, and **match the codebase's existing conventions** before editing. Semantic navigation — symbols, references, dependency structure, a compact repo-map — beats blind generation. The agent earns the right to edit by understanding first.
> Enforced by: structured read/search/edit tools + **code intelligence / repo-map** (task P3-T09).

### 5. Small, reversible, verified steps
Incremental change beats heroic rewrites. One logical change → verify → checkpoint. Work is **reversible by construction** so the agent can explore without risk and the human gate concentrates only where reversibility ends.
> Enforced by: git worktree per task, per-step verification, auto-reversible / gate-irreversible autonomy.

### 6. Define "done" before you start
For anything non-trivial, establish the **acceptance criteria — ideally the failing test — first**, then make it pass. This is the single best defense against the agent confidently building the plausible-but-wrong thing.
> Enforced by: the planner emits acceptance criteria up front (P3-T01); the verifier confirms them; the persona states assumptions.

### 7. Quality is the bar, not correctness
Passing tests is the **floor**. The output must be what a senior engineer would approve: a **minimal diff**, idiomatic, well-named, convention-matching, no dead code, errors handled, no unrequested scope. Green-but-ugly is not done.
> Enforced by: the terse-senior persona, cross-model review before the gate, lint in the verifier. See *Definition of good* below.

### 8. Recover, don't thrash
Recognize being stuck and **change strategy** — escalate to the advisor, try a different approach, or stop and ask one sharp question — rather than looping blindly. Knowing when to escalate is a strength, not a failure.
> Enforced by: Advisor-Executor escalation, the routing ladder, clarify-when-blocked, per-task budgets.

### 9. Earn improvement from evidence
Measure everything; tune from **evals and the audit trail**, not vibes. Routing, prompts, and skills improve from logged outcomes, never from assumption.
> Enforced by: the eval harness, the append-only audit log, "earn strength-routing from race data."

### 10. Safety is what makes autonomy possible
The sandbox, the gate, the audit, and no ambient authority are **not friction** — they are the reason the agent can be trusted to run unattended. Autonomy rides on guardrails, not trust.
> Enforced by: the full security model (**I3, I4, I5, I7**), the human gate on irreversible actions.

## Anti-principles — the traps that make agents mediocre

- Reaching for a bigger model instead of a better harness.
- Stuffing the context window "to be safe."
- Heroic one-shot rewrites instead of small verified steps.
- Trusting the model's "it works" over an actual check.
- Editing before understanding the codebase.
- Optimizing on vibes instead of evals.
- Bolting on features that dilute the core.

## Definition of *good* (the agent's quality bar)

A change is done only when it is:

1. **Verified** — build, types, tests, lint all green.
2. **Minimal** — the smallest diff that satisfies the goal; no unrequested scope.
3. **Idiomatic** — matches the codebase's existing patterns, structure, and naming.
4. **Legible** — clear names, comments that explain *why*, no dead code.
5. **Robust** — error paths handled; no obvious failure modes left open.
6. **Reviewed** — would pass a senior engineer's review without a second pass.

Correctness gets you to #1. The other five are what make NilCore the best, not merely functional.
