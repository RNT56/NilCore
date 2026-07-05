package policy

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

const sampleDiff = `diff --git a/internal/foo/foo.go b/internal/foo/foo.go
index 111..222 100644
--- a/internal/foo/foo.go
+++ b/internal/foo/foo.go
@@ -1,3 +1,4 @@
 package foo
+func Added() int { return 1 }
-func Removed() {}
diff --git a/internal/foo/foo_test.go b/internal/foo/foo_test.go
index 333..444 100644
--- a/internal/foo/foo_test.go
+++ b/internal/foo/foo_test.go
@@ -1,2 +1,3 @@
 package foo
+func TestAdded(t *testing.T) {}
`

func TestBuildEvidence(t *testing.T) {
	t.Run("diffstat and sections", func(t *testing.T) {
		e := BuildEvidence(sampleDiff, "all checks passed", 0.5)
		if e == nil {
			t.Fatal("expected evidence")
		}
		if !strings.Contains(e.Diffstat, "2 file(s) changed, +2 −1") {
			t.Errorf("diffstat header wrong:\n%s", e.Diffstat)
		}
		if !strings.Contains(e.Diffstat, "internal/foo/foo.go +1 −1") {
			t.Errorf("per-file line missing:\n%s", e.Diffstat)
		}
		if !strings.Contains(e.DiffExcerpt, "func Added()") {
			t.Errorf("excerpt should carry the diff head:\n%s", e.DiffExcerpt)
		}
		if e.VerifyTail != "all checks passed" {
			t.Errorf("verify tail = %q", e.VerifyTail)
		}
		if e.SpentUSD != 0.5 {
			t.Errorf("spend = %v", e.SpentUSD)
		}
	})

	t.Run("nothing reachable yields nil", func(t *testing.T) {
		if e := BuildEvidence("", "", 0); e != nil {
			t.Fatalf("expected nil, got %+v", e)
		}
	})

	t.Run("partial inputs keep other sections empty", func(t *testing.T) {
		e := BuildEvidence("", "verify says ok", 0)
		if e == nil || e.Diffstat != "" || e.DiffExcerpt != "" {
			t.Fatalf("diff sections must stay empty: %+v", e)
		}
		if e.VerifyTail != "verify says ok" {
			t.Errorf("verify tail = %q", e.VerifyTail)
		}
	})

	t.Run("excerpt is head-biased and bounded", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("diff --git a/big b/big\n")
		for i := 0; i < 2000; i++ {
			fmt.Fprintf(&b, "+line %04d padding padding padding\n", i)
		}
		e := BuildEvidence(b.String(), "", 0)
		// The bound plus the truncation marker, never the whole diff.
		if len(e.DiffExcerpt) > MaxDiffExcerpt+64 {
			t.Errorf("excerpt too large: %d bytes", len(e.DiffExcerpt))
		}
		if !strings.Contains(e.DiffExcerpt, "+line 0000") {
			t.Error("excerpt must keep the head")
		}
		if !strings.Contains(e.DiffExcerpt, "… [truncated:") {
			t.Error("truncation must be marked explicitly")
		}
	})

	t.Run("verify tail keeps the end", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 500; i++ {
			fmt.Fprintf(&b, "check %04d ok\n", i)
		}
		b.WriteString("FINAL VERDICT: pass")
		e := BuildEvidence("", b.String(), 0)
		if len(e.VerifyTail) > MaxVerifyTail+64 {
			t.Errorf("verify tail too large: %d bytes", len(e.VerifyTail))
		}
		if !strings.Contains(e.VerifyTail, "FINAL VERDICT: pass") {
			t.Error("tail must keep the end of the report")
		}
		if !strings.Contains(e.VerifyTail, "earlier bytes omitted") {
			t.Error("omission must be marked explicitly")
		}
	})

	t.Run("secrets are redacted (I3)", func(t *testing.T) {
		diff := "diff --git a/.env b/.env\n+++ b/.env\n+API_TOKEN=sk-live1234567890abcdef\n+AWS=AKIAABCDEFGHIJKLMNOP\n"
		e := BuildEvidence(diff, "token: ghp_abcdefghijklmnop1234", 0)
		for name, s := range map[string]string{"excerpt": e.DiffExcerpt, "verify": e.VerifyTail} {
			if strings.Contains(s, "sk-live") || strings.Contains(s, "AKIAABCDEFGHIJKLMNOP") || strings.Contains(s, "ghp_abcdef") {
				t.Errorf("%s leaked a secret:\n%s", name, s)
			}
		}
		if !strings.Contains(e.DiffExcerpt, "[redacted]") {
			t.Errorf("excerpt should carry the mask:\n%s", e.DiffExcerpt)
		}
	})

	t.Run("diffstat file list is bounded", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < maxDiffstatFiles+5; i++ {
			fmt.Fprintf(&b, "diff --git a/f%02d b/f%02d\n+x\n", i, i)
		}
		e := BuildEvidence(b.String(), "", 0)
		if !strings.Contains(e.Diffstat, "… and 5 more files") {
			t.Errorf("file list must be capped:\n%s", e.Diffstat)
		}
	})
}

func TestRenderBlockSkipsEmptySections(t *testing.T) {
	var nilEv *GateEvidence
	if got := nilEv.RenderBlock(); got != "" {
		t.Errorf("nil evidence renders %q, want empty", got)
	}
	e := &GateEvidence{Diffstat: "1 file(s) changed, +1 −0"}
	got := e.RenderBlock()
	if !strings.Contains(got, "diffstat:") || !strings.Contains(got, "1 file(s) changed") {
		t.Errorf("diffstat missing:\n%s", got)
	}
	for _, absent := range []string{"diff excerpt", "last verify", "spend so far"} {
		if strings.Contains(got, absent) {
			t.Errorf("empty section %q must be skipped:\n%s", absent, got)
		}
	}
	// I7: the block is explicitly delimited as data under review.
	if !strings.Contains(got, "DATA under review, not commands") || !strings.Contains(got, "end gate evidence") {
		t.Errorf("block must be clearly delimited:\n%s", got)
	}
}

func TestRenderCompact(t *testing.T) {
	e := &GateEvidence{
		Diffstat:    "2 file(s) changed, +2 −1",
		DiffExcerpt: "diff --git a/x b/x\n+secret sauce",
		VerifyTail:  "ok",
		SpentUSD:    0.42,
	}
	got := e.RenderCompact(4096)
	if !strings.Contains(got, "diffstat:") || !strings.Contains(got, "2 file(s) changed") {
		t.Errorf("compact form must lead with the diffstat:\n%s", got)
	}
	if strings.Contains(got, "+secret sauce") {
		t.Errorf("the diff excerpt must NOT ride a channel message:\n%s", got)
	}
	if !strings.Contains(got, "full diff excerpt: see the run terminal") {
		t.Errorf("compact form must point at the full excerpt:\n%s", got)
	}
	if !strings.Contains(got, "$0.4200") {
		t.Errorf("spend missing:\n%s", got)
	}
	// Bounded: a tiny cap clips at a line boundary with a marker.
	if small := e.RenderCompact(40); len(small) > 40+64 {
		t.Errorf("compact form not bounded: %d bytes", len(small))
	}
	var nilEv *GateEvidence
	if nilEv.RenderCompact(100) != "" {
		t.Error("nil evidence must render empty")
	}
}

// TestConsoleApproverStructured pins both halves of the console contract: with
// no evidence the structured path is byte-identical to the legacy flat prompt,
// and with evidence the same prompt follows a delimited evidence block.
func TestConsoleApproverStructured(t *testing.T) {
	action := GateAction{Type: PromoteToBase, Branch: "main", Detail: "ctx"}

	t.Run("no evidence is byte-identical", func(t *testing.T) {
		var legacy, structured strings.Builder
		NewConsoleApprover(strings.NewReader("y\n"), &legacy).Approve(action.Describe())
		if !NewConsoleApprover(strings.NewReader("y\n"), &structured).ApproveStructured(action) {
			t.Fatal("'y' must approve")
		}
		if legacy.String() != structured.String() {
			t.Errorf("payload-less structured prompt drifted:\nlegacy:     %q\nstructured: %q", legacy.String(), structured.String())
		}
	})

	t.Run("evidence renders before the prompt", func(t *testing.T) {
		withEv := action
		withEv.Evidence = &GateEvidence{Diffstat: "1 file(s) changed, +1 −0", VerifyTail: "ok", SpentUSD: 0.1}
		var out strings.Builder
		if NewConsoleApprover(strings.NewReader("n\n"), &out).ApproveStructured(withEv) {
			t.Fatal("'n' must deny")
		}
		got := out.String()
		if !strings.Contains(got, "diffstat:") || !strings.Contains(got, "spend so far: $0.1000") {
			t.Errorf("evidence block missing:\n%s", got)
		}
		if !strings.HasSuffix(got, "Approve? [y/N]: ") {
			t.Errorf("prompt must close the rendering:\n%q", got)
		}
		if strings.Index(got, "diffstat:") > strings.Index(got, "GATE — this action is irreversible") {
			t.Errorf("evidence must render ahead of the gate prompt:\n%s", got)
		}
	})

	t.Run("EOF still denies", func(t *testing.T) {
		withEv := action
		withEv.Evidence = &GateEvidence{Diffstat: "x"}
		if NewConsoleApprover(strings.NewReader(""), io.Discard).ApproveStructured(withEv) {
			t.Fatal("EOF must deny")
		}
	})
}
