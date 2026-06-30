package benchmark

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/sandbox"
)

// seqBox is a hermetic sandbox.Sandbox stand-in for the RE-MEASURE loop: it returns a
// SCRIPTED SEQUENCE of canned Results (one per Exec call), records the command and the
// exact number of Exec calls, and is safe for the -race detector. Once the scripted
// sequence is exhausted it repeats the last entry, so a test can script "first K runs"
// without enumerating every clamp. No real benchmark, sed, grep, or network ever runs.
type seqBox struct {
	mu      sync.Mutex
	seq     []sandbox.Result // canned outputs, consumed in order
	errSeq  []error          // optional per-call Go errors (nil-padded)
	calls   int
	lastCmd string
}

func (b *seqBox) Exec(ctx context.Context, cmd string) (sandbox.Result, error) {
	return b.ExecWithEnv(ctx, cmd, nil)
}

func (b *seqBox) ExecWithEnv(_ context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	idx := b.calls
	b.calls++
	b.lastCmd = cmd
	var err error
	if idx < len(b.errSeq) {
		err = b.errSeq[idx]
	}
	if len(b.seq) == 0 {
		return sandbox.Result{}, err
	}
	if idx >= len(b.seq) {
		idx = len(b.seq) - 1 // repeat the last scripted result
	}
	return b.seq[idx], err
}

func (b *seqBox) Workdir() string { return "/work" }

func (b *seqBox) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// goBench builds a single Go-bench-style stdout line yielding the given ns/op value.
func goBench(nsop float64) sandbox.Result {
	line := "BenchmarkX-8\t   100\t   " + ftoa(nsop) + " ns/op"
	return sandbox.Result{Stdout: line + "\n", ExitCode: 0}
}

func ftoa(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// specValue marshals a boundSpec-shaped value the way a worker's Evidence.Value would
// carry it. Helper so tests read declaratively.
func specValue(t *testing.T, metric string, bound float64, op string, runs int, cvMax float64, samples []float64) string {
	t.Helper()
	m := map[string]any{"metric": metric, "bound": bound, "op": op, "runs": runs, "cv_max": cvMax}
	if samples != nil {
		m["samples"] = samples
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	return string(b)
}

func claim(verifier, value, method string) artifact.Claim {
	return artifact.Claim{
		ID:    "c1",
		Field: "perf",
		Evidence: artifact.Evidence{
			Value:            value,
			ExtractionMethod: method,
			Verifier:         verifier,
		},
	}
}

func TestRegisterAll(t *testing.T) {
	r := evverify.New()
	for _, id := range []string{IDScriptThreshold, IDVarianceBounded} {
		if _, ok := r.Lookup(id); ok {
			t.Fatalf("%s present before RegisterAll", id)
		}
	}
	RegisterAll(r)
	for _, id := range []string{IDScriptThreshold, IDVarianceBounded} {
		if _, ok := r.Lookup(id); !ok {
			t.Fatalf("%s absent after RegisterAll", id)
		}
	}
	if _, ok := r.Lookup("benchmark.nope"); ok {
		t.Fatal("RegisterAll registered an unexpected id")
	}
}

func TestHostsAndSchemas(t *testing.T) {
	if Hosts() != nil {
		t.Errorf("Hosts() = %v, want nil (this pack reaches no host)", Hosts())
	}
	got := Schemas()
	if len(got) != 1 || got[0] != artifact.KindBenchmark {
		t.Errorf("Schemas() = %v, want [%q]", got, artifact.KindBenchmark)
	}
}

// TestScriptThreshold drives the re-measure loop with a scripted sequence of K bench
// stdouts and asserts both the verdict AND that the stub recorded exactly K Exec calls.
func TestScriptThreshold(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		seq      []sandbox.Result
		errSeq   []error
		runs     int
		bound    float64
		op       string
		cvMax    float64
		want     artifact.Status
		wantOnce int // expected Exec call count (the re-measure loop length)
	}{
		{
			name:     "within bound and low CV => Pass",
			seq:      []sandbox.Result{goBench(100), goBench(101), goBench(99), goBench(100)},
			runs:     4,
			bound:    200,
			op:       opLE,
			cvMax:    0.10,
			want:     artifact.StatusPass,
			wantOnce: 4,
		},
		{
			name:     "mean outside bound (<=) => Fail",
			seq:      []sandbox.Result{goBench(300), goBench(305), goBench(295)},
			runs:     3,
			bound:    200,
			op:       opLE,
			cvMax:    0.50,
			want:     artifact.StatusFail,
			wantOnce: 3,
		},
		{
			name:     "mean outside bound (>=) => Fail",
			seq:      []sandbox.Result{goBench(100), goBench(101), goBench(99)},
			runs:     3,
			bound:    500,
			op:       opGE,
			cvMax:    0.50,
			want:     artifact.StatusFail,
			wantOnce: 3,
		},
		{
			name:     "high CV (within bound) => Fail",
			seq:      []sandbox.Result{goBench(10), goBench(200), goBench(10), goBench(200)},
			runs:     4,
			bound:    1000, // mean ~105 <= 1000, so the bound passes; CV must be what fails
			op:       opLE,
			cvMax:    0.05,
			want:     artifact.StatusFail,
			wantOnce: 4,
		},
		{
			name:     "unparseable output => Unverifiable",
			seq:      []sandbox.Result{{Stdout: "no metric here\n", ExitCode: 0}, {Stdout: "still nothing\n", ExitCode: 0}},
			runs:     2,
			bound:    200,
			op:       opLE,
			cvMax:    0.50,
			want:     artifact.StatusUnverifiable,
			wantOnce: 2,
		},
		{
			name:     "non-zero exit => Unverifiable",
			seq:      []sandbox.Result{{Stderr: "boom", ExitCode: 1}, {Stderr: "boom", ExitCode: 1}},
			runs:     2,
			bound:    200,
			op:       opLE,
			cvMax:    0.50,
			want:     artifact.StatusUnverifiable,
			wantOnce: 2,
		},
		{
			name:     "only one parseable run => Unverifiable (<2 samples)",
			seq:      []sandbox.Result{goBench(100), {Stdout: "garbage\n", ExitCode: 0}},
			runs:     2,
			bound:    200,
			op:       opLE,
			cvMax:    0.50,
			want:     artifact.StatusUnverifiable,
			wantOnce: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			box := &seqBox{seq: tc.seq, errSeq: tc.errSeq}
			c := claim(IDScriptThreshold, specValue(t, "ns/op", tc.bound, tc.op, tc.runs, tc.cvMax, nil), runnerGoBench)
			got, d := checkScriptThreshold(ctx, box, c)
			if got != tc.want {
				t.Errorf("status = %q, want %q (detail: %s)", got, tc.want, d)
			}
			if n := box.callCount(); n != tc.wantOnce {
				t.Errorf("re-measure loop ran %d Exec calls, want %d", n, tc.wantOnce)
			}
			// The detail must never leak the runner's raw stdout body (I7).
			if strings.Contains(d, "BenchmarkX") {
				t.Errorf("detail leaked raw runner stdout: %q", d)
			}
		})
	}
}

// TestScriptThresholdRunsTheRunner asserts the FIXED command shape and that the loop
// actually re-runs (the I2-erosion fix: the verifier measures, never trusts samples).
func TestScriptThresholdReMeasures(t *testing.T) {
	ctx := context.Background()
	box := &seqBox{seq: []sandbox.Result{goBench(100), goBench(100), goBench(100)}}
	// A hostile worker supplies a samples[] array claiming a perfect series; the verifier
	// must IGNORE it and re-measure regardless.
	val := specValue(t, "ns/op", 200, opLE, 3, 0.10, []float64{1, 1, 1})
	c := claim(IDScriptThreshold, val, runnerGoBench)
	got, _ := checkScriptThreshold(ctx, box, c)
	if got != artifact.StatusPass {
		t.Fatalf("status = %q, want Pass", got)
	}
	if box.callCount() != 3 {
		t.Fatalf("verifier ran %d times, want 3 re-measure runs (must not trust worker samples)", box.callCount())
	}
	if !strings.Contains(box.lastCmd, "go test -bench") {
		t.Errorf("runner command = %q, want the fixed go-bench shape", box.lastCmd)
	}
}

// TestScriptThresholdIgnoresWorkerSamples is the DISCRIMINATING I2 guard: the worker's
// supplied samples[] and the box's own re-runs DISAGREE about the verdict, so the test
// can only pass if the verifier ignores the worker array and trusts its own measurement.
//
// The worker swears samples=[1,1,1] and claims a tight fast bound (mean<=50). If the
// verifier ever (mis)read mean(spec.Samples)=1, that would satisfy 1<=50 => Pass — a
// laundered regression slipping through. But the box re-measures ~100 ns/op each run,
// and 100 is NOT <= 50, so the only honest verdict is Fail. Asserting Fail here proves
// the box's own measurement governs and the worker samples are discarded; if this case
// ever reported Pass, the re-measure guarantee (I2) would be broken.
func TestScriptThresholdIgnoresWorkerSamples(t *testing.T) {
	ctx := context.Background()
	box := &seqBox{seq: []sandbox.Result{goBench(100), goBench(100), goBench(100)}}
	// Worker-claimed fast samples [1,1,1] under a fast bound of 50; box re-runs measure
	// ~100 (over the bound). Worker samples => Pass; honest re-measure => Fail.
	val := specValue(t, "ns/op", 50, opLE, 3, 0.10, []float64{1, 1, 1})
	c := claim(IDScriptThreshold, val, runnerGoBench)
	got, d := checkScriptThreshold(ctx, box, c)
	if got != artifact.StatusFail {
		t.Fatalf("status = %q, want Fail (the box re-measured ~100 > bound 50; "+
			"a Pass here means the verifier trusted the worker's [1,1,1] samples) detail: %s", got, d)
	}
	if box.callCount() != 3 {
		t.Fatalf("verifier ran %d times, want 3 re-measure runs (must not short-circuit on worker samples)", box.callCount())
	}
}

func TestScriptThresholdNilBox(t *testing.T) {
	c := claim(IDScriptThreshold, specValue(t, "ns/op", 200, opLE, 3, 0.1, nil), runnerGoBench)
	got, _ := checkScriptThreshold(context.Background(), nil, c)
	if got != artifact.StatusUnverifiable {
		t.Fatalf("nil box status = %q, want Unverifiable", got)
	}
}

func TestScriptThresholdRunnerNotAllowlisted(t *testing.T) {
	box := &seqBox{seq: []sandbox.Result{goBench(100)}}
	c := claim(IDScriptThreshold, specValue(t, "ns/op", 200, opLE, 3, 0.1, nil), "rm -rf /")
	got, _ := checkScriptThreshold(context.Background(), box, c)
	if got != artifact.StatusUnverifiable {
		t.Fatalf("free-command runner status = %q, want Unverifiable", got)
	}
	if box.callCount() != 0 {
		t.Fatalf("a non-allowlisted runner must reach the box 0 times, got %d", box.callCount())
	}
}

func TestScriptThresholdBadSpec(t *testing.T) {
	box := &seqBox{seq: []sandbox.Result{goBench(100)}}
	cases := []struct {
		name, value string
	}{
		{"empty value", ""},
		{"not json", "not json at all"},
		{"unknown op", `{"metric":"ns/op","bound":1,"op":"!=","runs":3,"cv_max":0.1}`},
		{"unknown field", `{"metric":"ns/op","bound":1,"op":"<=","runs":3,"cv_max":0.1,"evil":1}`},
		{"trailing data", `{"metric":"ns/op","bound":1,"op":"<=","runs":3,"cv_max":0.1} junk`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			box.calls = 0
			c := claim(IDScriptThreshold, tc.value, runnerGoBench)
			got, _ := checkScriptThreshold(context.Background(), box, c)
			if got != artifact.StatusUnverifiable {
				t.Fatalf("status = %q, want Unverifiable", got)
			}
			if box.callCount() != 0 {
				t.Fatalf("a malformed spec must reach the box 0 times, got %d", box.callCount())
			}
		})
	}
}

// TestScriptThresholdClampsRuns proves K is clamped to [minSamples, maxRuns]: a worker
// asking for 1000 runs cannot turn the verifier into a fork bomb, and a 0/1 request
// still re-measures at least minSamples times.
func TestScriptThresholdClampsRuns(t *testing.T) {
	ctx := context.Background()
	t.Run("huge K clamped to maxRuns", func(t *testing.T) {
		box := &seqBox{seq: []sandbox.Result{goBench(100)}}
		c := claim(IDScriptThreshold, specValue(t, "ns/op", 200, opLE, 1000, 0.1, nil), runnerGoBench)
		checkScriptThreshold(ctx, box, c)
		if box.callCount() != maxRuns {
			t.Fatalf("K=1000 ran %d times, want clamp to maxRuns=%d", box.callCount(), maxRuns)
		}
	})
	t.Run("zero K floored to minSamples", func(t *testing.T) {
		box := &seqBox{seq: []sandbox.Result{goBench(100)}}
		c := claim(IDScriptThreshold, specValue(t, "ns/op", 200, opLE, 0, 0.1, nil), runnerGoBench)
		checkScriptThreshold(ctx, box, c)
		if box.callCount() != minSamples {
			t.Fatalf("K=0 ran %d times, want floor to minSamples=%d", box.callCount(), minSamples)
		}
	})
}

// TestVarianceBounded covers the pure secondary check with a golden CV table.
func TestVarianceBounded(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		samples []float64
		cvMax   float64
		want    artifact.Status
	}{
		{"identical samples, zero CV => Pass", []float64{100, 100, 100}, 0.01, artifact.StatusPass},
		{"low spread within ceiling => Pass", []float64{100, 102, 98, 101, 99}, 0.05, artifact.StatusPass},
		{"CV exactly at ceiling => Pass (inclusive)", []float64{90, 110}, 0.1, artifact.StatusPass},
		{"high spread over ceiling => Fail", []float64{10, 200}, 0.05, artifact.StatusFail},
		// A non-finite sample can never arrive through legitimate JSON (encoding/json
		// rejects Inf/NaN on the worker's side), so the non-finite guard is exercised at
		// the numeric layer in TestNumericGoldenCV, not here.
		{"single sample => Unverifiable", []float64{100}, 0.5, artifact.StatusUnverifiable},
		{"no samples => Unverifiable", []float64{}, 0.5, artifact.StatusUnverifiable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := claim(IDVarianceBounded, specValue(t, "ns/op", 0, opLE, 0, tc.cvMax, tc.samples), "")
			got, d := checkVarianceBounded(ctx, nil, c)
			if got != tc.want {
				t.Errorf("status = %q, want %q (detail: %s)", got, tc.want, d)
			}
		})
	}
}

// TestVarianceBoundedNeverReachesBox proves the secondary check is pure (box-free): even
// a non-nil box must never be touched.
func TestVarianceBoundedNeverReachesBox(t *testing.T) {
	box := &seqBox{seq: []sandbox.Result{goBench(100)}}
	c := claim(IDVarianceBounded, specValue(t, "ns/op", 0, opLE, 0, 0.5, []float64{100, 101, 99}), "")
	checkVarianceBounded(context.Background(), box, c)
	if box.callCount() != 0 {
		t.Fatalf("variance_bounded reached the box %d times, want 0 (pure check)", box.callCount())
	}
}

// TestParseMetric covers the host-side parser across runner output shapes.
func TestParseMetric(t *testing.T) {
	tests := []struct {
		name, stdout, metric string
		wantVal              float64
		wantOK               bool
	}{
		{"go bench ns/op", "BenchmarkX-8\t100\t456 ns/op\n", "ns/op", 456, true},
		{"go bench MB/s", "BenchmarkX-8   100   12.5 MB/s\n", "MB/s", 12.5, true},
		{"key=value", "score=42\n", "score", 42, true},
		{"key: value", "score: 42.5\n", "score", 42.5, true},
		{"key space value", "latency 7\n", "latency", 7, true},
		{"metric absent", "BenchmarkX-8 100 456 ns/op\n", "MB/s", 0, false},
		{"empty metric", "anything 1\n", "", 0, false},
		{"non-numeric neighbor", "ns/op here\n", "ns/op", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := parseMetric(tc.stdout, tc.metric)
			if ok != tc.wantOK || (ok && v != tc.wantVal) {
				t.Errorf("parseMetric(%q,%q) = (%v,%v), want (%v,%v)", tc.stdout, tc.metric, v, ok, tc.wantVal, tc.wantOK)
			}
		})
	}
}

// TestResolveRunner asserts the allowlist maps kinds to fixed shapes and rejects free
// commands and traversal.
func TestResolveRunner(t *testing.T) {
	tests := []struct {
		name, method string
		wantErr      bool
		wantContains string
	}{
		{"go-bench", runnerGoBench, false, "go test -bench"},
		{"go-bench scoped", "go-bench:internal/foo", false, "'./internal/foo/...'"},
		{"go-bench scoped keeps shape", "go-bench:internal/foo", false, "go test -bench=. -run='^$' -benchmem"},
		{"go-bench scoped traversal", "go-bench:../../etc", true, ""},
		{"go-bench scoped absolute", "go-bench:/etc", true, ""},
		{"go-bench scoped metachar", "go-bench:foo;rm -rf /", true, ""},
		{"go-bench scoped empty path", "go-bench:", true, ""},
		{"make-bench", runnerMakeBench, false, "make bench"},
		{"script ok", "script:bench/run.sh", false, "bench/run.sh"},
		{"script traversal", "script:../../etc/passwd", true, ""},
		{"script absolute", "script:/etc/passwd", true, ""},
		{"script metachar", "script:run.sh;rm -rf /", true, ""},
		{"free command", "rm -rf /", true, ""},
		{"empty", "", true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := resolveRunner(tc.method, "/work")
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && !strings.Contains(cmd, tc.wantContains) {
				t.Errorf("cmd = %q, want it to contain %q", cmd, tc.wantContains)
			}
		})
	}
}

// TestNumericGoldenCV is the golden coefficient-of-variation table for the pure helpers.
func TestNumericGoldenCV(t *testing.T) {
	const eps = 1e-9
	tests := []struct {
		name    string
		xs      []float64
		wantCV  float64
		wantInf bool
		wantNaN bool
	}{
		{"identical", []float64{5, 5, 5, 5}, 0, false, false},
		{"two symmetric", []float64{90, 110}, 10.0 / 100.0, false, false},
		{"mean zero nonzero spread", []float64{-1, 1}, 0, true, false},
		{"all zero", []float64{0, 0, 0}, 0, false, false},
		{"non-finite", []float64{1, math.NaN()}, 0, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cv(tc.xs)
			switch {
			case tc.wantNaN:
				if !math.IsNaN(got) {
					t.Errorf("cv = %v, want NaN", got)
				}
			case tc.wantInf:
				if !math.IsInf(got, 1) {
					t.Errorf("cv = %v, want +Inf", got)
				}
			default:
				if math.Abs(got-tc.wantCV) > eps {
					t.Errorf("cv = %v, want %v", got, tc.wantCV)
				}
			}
		})
	}
}

func TestCompareBound(t *testing.T) {
	tests := []struct {
		agg  float64
		op   string
		b    float64
		want bool
	}{
		{100, opLE, 200, true},
		{300, opLE, 200, false},
		{300, opGE, 200, true},
		{100, opGE, 200, false},
		{100, "!=", 200, false}, // unknown op fails closed
		{math.NaN(), opLE, 200, false},
		{100, opLE, math.Inf(1), false},
	}
	for _, tc := range tests {
		if got := compareBound(tc.agg, tc.op, tc.b); got != tc.want {
			t.Errorf("compareBound(%v,%q,%v) = %v, want %v", tc.agg, tc.op, tc.b, got, tc.want)
		}
	}
}

// sanity: the two checks satisfy the CheckFunc signature (also enforced by the compile
// guards in benchmark.go; here we exercise them through the Registry to mirror real use).
func TestThroughRegistry(t *testing.T) {
	r := evverify.New()
	RegisterAll(r)
	box := &seqBox{seq: []sandbox.Result{goBench(100), goBench(100), goBench(100)}}
	c := claim(IDScriptThreshold, specValue(t, "ns/op", 200, opLE, 3, 0.1, nil), runnerGoBench)
	st, _ := r.Resolve(context.Background(), box, c)
	if st != artifact.StatusPass {
		t.Fatalf("Resolve status = %q, want Pass", st)
	}
}
