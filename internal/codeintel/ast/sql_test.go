package ast

import "testing"

// A SQL fixture exercising multiple CREATE kinds: TABLE, VIEW, MATERIALIZED VIEW, FUNCTION,
// PROCEDURE, TRIGGER, INDEX, TYPE, SCHEMA — with `OR REPLACE`, `IF NOT EXISTS`, and a
// schema-qualified name (kept verbatim). FUNCTION/PROCEDURE map to KindFunc, the rest to
// KindType. Decoys in `--`/`/* */` comments and string literals must NOT register. The
// intra-file function call graph counts only calls resolving to a defined FUNCTION/PROCEDURE.
const sqlSample = `-- a comment with CREATE TABLE decoy (x int) that must not register
/* block comment
   CREATE VIEW decoy_view AS SELECT 1;
*/

CREATE TABLE public.users (
    id   INTEGER PRIMARY KEY,
    name TEXT DEFAULT 'CREATE TABLE not_a_table (x int)'
);

CREATE OR REPLACE VIEW active_users AS
    SELECT id FROM public.users;

CREATE MATERIALIZED VIEW user_stats AS
    SELECT count(*) FROM public.users;

CREATE OR REPLACE FUNCTION compute_total(n int) RETURNS int AS $$
    SELECT n * 2;
$$ LANGUAGE sql;

CREATE PROCEDURE refresh_stats() AS $$
    SELECT compute_total(5);
$$ LANGUAGE sql;

CREATE TRIGGER audit_users AFTER INSERT ON public.users
    FOR EACH ROW EXECUTE FUNCTION refresh_stats();

CREATE INDEX IF NOT EXISTS idx_users_name ON public.users (name);

CREATE TYPE mood AS ENUM ('happy', 'sad');

CREATE SCHEMA reporting;
`

func TestSQLSymbols(t *testing.T) {
	syms, err := Symbols(writeSrc(t, "sample.sql", sqlSample))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Symbol{}
	for _, s := range syms {
		got[s.Name] = s
	}

	tests := []struct {
		name string
		kind Kind
	}{
		{"public.users", KindType}, // schema-qualified name kept verbatim
		{"active_users", KindType},
		{"user_stats", KindType}, // MATERIALIZED VIEW
		{"compute_total", KindFunc},
		{"refresh_stats", KindFunc}, // PROCEDURE
		{"audit_users", KindType},   // TRIGGER
		{"idx_users_name", KindType},
		{"mood", KindType},
		{"reporting", KindType}, // SCHEMA
	}
	for _, tc := range tests {
		s, ok := got[tc.name]
		if !ok {
			t.Errorf("%s: not extracted; got %+v", tc.name, got)
			continue
		}
		if s.Kind != tc.kind {
			t.Errorf("%s: kind = %q, want %q", tc.name, s.Kind, tc.kind)
		}
		if s.Recv != "" {
			t.Errorf("%s: recv = %q, want empty (SQL has no receivers)", tc.name, s.Recv)
		}
	}

	// Decoys inside comments and a string default must NOT register.
	for _, bad := range []string{"decoy", "decoy_view", "not_a_table"} {
		if _, ok := got[bad]; ok {
			t.Errorf("decoy %q leaked into symbols: %+v", bad, got)
		}
	}
}

func TestSQLCalls(t *testing.T) {
	calls, err := Calls(writeSrc(t, "sample.sql", sqlSample))
	if err != nil {
		t.Fatal(err)
	}
	// refresh_stats calls compute_total (a defined FUNCTION) — the only intra-file function
	// call. The trigger's `EXECUTE FUNCTION refresh_stats()` also resolves, but it is at file
	// scope (not inside a function body), so it is dropped from the per-function graph.
	if !contains(calls["refresh_stats"], "compute_total") {
		t.Errorf("refresh_stats should call compute_total; got %+v", calls["refresh_stats"])
	}
	// Both defined functions appear as keys.
	for _, fn := range []string{"compute_total", "refresh_stats"} {
		if _, ok := calls[fn]; !ok {
			t.Errorf("%s should be a key; got keys %v", fn, keys(calls))
		}
	}
}

func TestSQLReferences(t *testing.T) {
	refs, err := References(writeSrc(t, "sample.sql", sqlSample))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	// Only calls resolving to a defined FUNCTION/PROCEDURE survive.
	for _, want := range []string{"compute_total", "refresh_stats"} {
		if !names[want] {
			t.Errorf("expected a reference to defined function %q; got %+v", want, refs)
		}
	}
	// Built-ins and table references must NOT register as calls.
	for _, bad := range []string{"count", "users", "SELECT"} {
		if names[bad] {
			t.Errorf("non-function token %q leaked into references: %+v", bad, refs)
		}
	}
}
