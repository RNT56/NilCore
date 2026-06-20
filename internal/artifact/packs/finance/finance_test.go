package finance

import (
	"context"
	"strings"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/sandbox"
)

// fakeBox is a hermetic sandbox.Sandbox stand-in: it records the last command and the
// last per-run env, and returns a canned body/exit, so every network branch is driven
// without a real network. No host-side request ever leaves the test.
type fakeBox struct {
	lastCmd string
	lastEnv map[string]string
	calls   int
	exec    func(cmd string, env map[string]string) (sandbox.Result, error)
}

func (b *fakeBox) Exec(ctx context.Context, cmd string) (sandbox.Result, error) {
	return b.ExecWithEnv(ctx, cmd, nil)
}
func (b *fakeBox) ExecWithEnv(_ context.Context, cmd string, env map[string]string) (sandbox.Result, error) {
	b.lastCmd = cmd
	b.lastEnv = env
	b.calls++
	if b.exec != nil {
		return b.exec(cmd, env)
	}
	return sandbox.Result{}, nil
}
func (b *fakeBox) Workdir() string { return "/work" }

// okBody returns a fake box that responds 2xx with the given body.
func okBody(body string) *fakeBox {
	return &fakeBox{exec: func(string, map[string]string) (sandbox.Result, error) {
		return sandbox.Result{Stdout: body, ExitCode: 0}, nil
	}}
}

func claim(verifier, field, value, source string) artifact.Claim {
	return artifact.Claim{
		ID:    "c1",
		Field: field,
		Evidence: artifact.Evidence{
			Value:     value,
			SourceURL: source,
			Verifier:  verifier,
		},
	}
}

// TestFinancePack is the suite named in the Verify line.
func TestFinancePack(t *testing.T) {
	ctx := context.Background()

	t.Run("RegisterAll registers exactly the five ids", func(t *testing.T) {
		r := evverify.New()
		ids := []string{IDSecFact, IDWorldBankIndicator, IDIMFSeries, IDFREDSeries, IDMarketQuote}
		for _, id := range ids {
			if _, ok := r.Lookup(id); ok {
				t.Fatalf("%s present before RegisterAll", id)
			}
		}
		RegisterAll(r)
		for _, id := range ids {
			if _, ok := r.Lookup(id); !ok {
				t.Fatalf("%s absent after RegisterAll", id)
			}
		}
		// Nothing else: a finance id outside the five must not resolve.
		if _, ok := r.Lookup("finance.nope"); ok {
			t.Fatal("RegisterAll registered an unexpected id")
		}
	})

	t.Run("nil Box => Unverifiable on every check, no host call", func(t *testing.T) {
		cases := []artifact.Claim{
			claim(IDSecFact, "Revenues", "100", "https://data.sec.gov/api/xbrl/companyfacts/CIK0000320193.json"),
			claim(IDWorldBankIndicator, "gdp", "100", "https://api.worldbank.org/v2/country/US/indicator/NY.GDP.MKTP.CD?format=json"),
			claim(IDIMFSeries, "infl", "100", "https://www.imf.org/series?x=1"),
			claim(IDFREDSeries, "gdp", "100", "https://api.stlouisfed.org/fred/series/observations?series_id=GDP"),
			claim(IDMarketQuote, "price", "100", "https://financialmodelingprep.com/api/v3/quote/AAPL"),
		}
		fns := map[string]evverify.CheckFunc{
			IDSecFact: checkSECFact, IDWorldBankIndicator: checkWorldBankIndicator,
			IDIMFSeries: checkIMFSeries, IDFREDSeries: checkFREDSeries, IDMarketQuote: checkMarketQuote,
		}
		// keyed checks need a key present to reach the nil-box guard inside fetchKeyedBody.
		t.Setenv(EnvFREDKey, "SECRET-FRED")
		t.Setenv(EnvMarketKey, "SECRET-MKT")
		for _, c := range cases {
			st, _ := fns[c.Evidence.Verifier](ctx, nil, c)
			if st != artifact.StatusUnverifiable {
				t.Fatalf("%s nil box status = %q, want unverifiable", c.Evidence.Verifier, st)
			}
		}
	})
}

func TestFinanceSECFact(t *testing.T) {
	ctx := context.Background()
	const cik = "https://data.sec.gov/api/xbrl/companyfacts/CIK0000320193.json"

	// companyfacts shape: facts.<taxonomy>.<concept>.units.<unit>[].{val,end}
	const body = `{"cik":320193,"facts":{"us-gaap":{"Revenues":{"units":{"USD":[
		{"val":300000000,"end":"2022-12-31"},
		{"val":383285000000,"end":"2024-09-28"}]}}}}}`

	t.Run("exact int match => Pass (latest end wins)", func(t *testing.T) {
		box := okBody(body)
		st, _ := checkSECFact(ctx, box, claim(IDSecFact, "Revenues", "383285000000", cik))
		if st != artifact.StatusPass {
			t.Fatalf("status = %q, want pass", st)
		}
		// data.sec.gov requires a User-Agent — it must be present in the command.
		if !strings.Contains(box.lastCmd, "-A '") {
			t.Fatalf("sec command lacks a User-Agent: %q", box.lastCmd)
		}
	})

	t.Run("mismatch => Fail", func(t *testing.T) {
		st, _ := checkSECFact(ctx, okBody(body), claim(IDSecFact, "Revenues", "999", cik))
		if st != artifact.StatusFail {
			t.Fatalf("status = %q, want fail", st)
		}
	})

	t.Run("fact absent => Unverifiable", func(t *testing.T) {
		st, _ := checkSECFact(ctx, okBody(body), claim(IDSecFact, "NotAFact", "1", cik))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("status = %q, want unverifiable", st)
		}
	})

	t.Run("non-2xx => Unverifiable", func(t *testing.T) {
		box := &fakeBox{exec: func(string, map[string]string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 22, Stderr: "HTTP 404"}, nil
		}}
		st, _ := checkSECFact(ctx, box, claim(IDSecFact, "Revenues", "1", cik))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("status = %q, want unverifiable", st)
		}
	})

	t.Run("parse error => Unverifiable", func(t *testing.T) {
		st, _ := checkSECFact(ctx, okBody("not json {"), claim(IDSecFact, "Revenues", "1", cik))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("status = %q, want unverifiable", st)
		}
	})

	// Float tolerance: just-inside and just-outside 1e-6 relative.
	t.Run("float tolerance boundary", func(t *testing.T) {
		const fbody = `{"facts":{"us-gaap":{"Ratio":{"units":{"pure":[{"val":1.000000,"end":"2024-01-01"}]}}}}}`
		// fetched=1.0; tolerance is 1e-6*max(1,|1|)=1e-6.
		inside := "1.0000005"  // diff 5e-7 < 1e-6 => Pass
		outside := "1.0000020" // diff 2e-6 > 1e-6 => Fail
		if st, _ := checkSECFact(ctx, okBody(fbody), claim(IDSecFact, "Ratio", inside, cik)); st != artifact.StatusPass {
			t.Fatalf("just-inside tolerance status = %q, want pass", st)
		}
		if st, _ := checkSECFact(ctx, okBody(fbody), claim(IDSecFact, "Ratio", outside, cik)); st != artifact.StatusFail {
			t.Fatalf("just-outside tolerance status = %q, want fail", st)
		}
	})
}

func TestFinanceWorldBankAndIMF(t *testing.T) {
	ctx := context.Background()

	t.Run("worldbank latest non-null => Pass", func(t *testing.T) {
		body := `[{"page":1},[{"value":null,"date":"2024"},{"value":25462700000000,"date":"2023"}]]`
		st, _ := checkWorldBankIndicator(ctx, okBody(body),
			claim(IDWorldBankIndicator, "gdp", "25462700000000", "https://api.worldbank.org/v2/country/US/indicator/NY.GDP.MKTP.CD?format=json"))
		if st != artifact.StatusPass {
			t.Fatalf("status = %q, want pass", st)
		}
	})

	t.Run("worldbank mismatch => Fail", func(t *testing.T) {
		body := `[{"page":1},[{"value":1.0,"date":"2023"}]]`
		st, _ := checkWorldBankIndicator(ctx, okBody(body),
			claim(IDWorldBankIndicator, "gdp", "2.0", "https://api.worldbank.org/x?format=json"))
		if st != artifact.StatusFail {
			t.Fatalf("status = %q, want fail", st)
		}
	})

	t.Run("imf latest obs => Pass", func(t *testing.T) {
		body := `{"CompactData":{"DataSet":{"Series":{"Obs":[
			{"@TIME_PERIOD":"2022","@OBS_VALUE":"3.1"},
			{"@TIME_PERIOD":"2023","@OBS_VALUE":"4.2"}]}}}}`
		st, _ := checkIMFSeries(ctx, okBody(body),
			claim(IDIMFSeries, "infl", "4.2", "https://www.imf.org/series?x=1"))
		if st != artifact.StatusPass {
			t.Fatalf("status = %q, want pass", st)
		}
	})

	t.Run("imf single obj obs => Pass", func(t *testing.T) {
		body := `{"CompactData":{"DataSet":{"Series":{"Obs":{"@TIME_PERIOD":"2023","@OBS_VALUE":"7"}}}}}`
		st, _ := checkIMFSeries(ctx, okBody(body),
			claim(IDIMFSeries, "infl", "7", "https://www.imf.org/series?x=1"))
		if st != artifact.StatusPass {
			t.Fatalf("status = %q, want pass", st)
		}
	})
}

// TestFinanceKeyed is the I3 keystone: the literal key never appears in the command
// string, the env map carries it, the persisted SourceURL stays key-free, and no key
// => Unverifiable.
func TestFinanceKeyed(t *testing.T) {
	ctx := context.Background()
	const fredSrc = "https://api.stlouisfed.org/fred/series/observations?series_id=GDP"
	const mktSrc = "https://financialmodelingprep.com/api/v3/quote/AAPL"

	t.Run("fred: key in env map, NOT in command; SourceURL key-free", func(t *testing.T) {
		const secret = "SUPERSECRETFREDKEY123"
		t.Setenv(EnvFREDKey, secret)
		body := `{"observations":[{"date":"2024-01-01","value":"27000.5"}]}`
		box := okBody(body)
		c := claim(IDFREDSeries, "gdp", "27000.5", fredSrc)
		st, _ := checkFREDSeries(ctx, box, c)
		if st != artifact.StatusPass {
			t.Fatalf("status = %q, want pass", st)
		}
		// The literal key value MUST NOT appear in the command string — only $NAME.
		if strings.Contains(box.lastCmd, secret) {
			t.Fatalf("literal key leaked into command: %q", box.lastCmd)
		}
		if !strings.Contains(box.lastCmd, "$"+EnvFREDKey) {
			t.Fatalf("command should reference $%s: %q", EnvFREDKey, box.lastCmd)
		}
		// The env map must carry the value for box-side $VAR expansion.
		if box.lastEnv[EnvFREDKey] != secret {
			t.Fatalf("env map missing key value, got %v", box.lastEnv)
		}
		// The persisted SourceURL (on the claim) stays key-free — it is never rewritten.
		if strings.Contains(c.Evidence.SourceURL, secret) || strings.Contains(c.Evidence.SourceURL, "api_key=") {
			t.Fatalf("persisted SourceURL is not key-free: %q", c.Evidence.SourceURL)
		}
	})

	t.Run("fred: no key supplied => Unverifiable, never Pass", func(t *testing.T) {
		t.Setenv(EnvFREDKey, "")
		box := okBody(`{"observations":[{"value":"1"}]}`)
		st, _ := checkFREDSeries(ctx, box, claim(IDFREDSeries, "gdp", "1", fredSrc))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("no-key status = %q, want unverifiable", st)
		}
		if box.calls != 0 {
			t.Fatal("no-key path must not reach the box")
		}
	})

	t.Run("market_quote: key in env, not command; price match => Pass", func(t *testing.T) {
		const secret = "SUPERSECRETMARKETKEY"
		t.Setenv(EnvMarketKey, secret)
		body := `[{"symbol":"AAPL","price":195.12}]`
		box := okBody(body)
		c := claim(IDMarketQuote, "price", "195.12", mktSrc)
		st, _ := checkMarketQuote(ctx, box, c)
		if st != artifact.StatusPass {
			t.Fatalf("status = %q, want pass", st)
		}
		if strings.Contains(box.lastCmd, secret) {
			t.Fatalf("literal key leaked into command: %q", box.lastCmd)
		}
		if box.lastEnv[EnvMarketKey] != secret {
			t.Fatalf("env map missing key value")
		}
	})

	t.Run("market_quote: no key => Unverifiable", func(t *testing.T) {
		t.Setenv(EnvMarketKey, "")
		st, _ := checkMarketQuote(ctx, okBody(`[{"price":1}]`), claim(IDMarketQuote, "price", "1", mktSrc))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("status = %q, want unverifiable", st)
		}
	})

	t.Run("keyed: detail tail never contains the key", func(t *testing.T) {
		const secret = "LEAKYKEY999"
		t.Setenv(EnvFREDKey, secret)
		// Force a non-2xx so the failure detail is exercised.
		box := &fakeBox{exec: func(string, map[string]string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 22, Stderr: "HTTP 403"}, nil
		}}
		st, d := checkFREDSeries(ctx, box, claim(IDFREDSeries, "gdp", "1", fredSrc))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("status = %q, want unverifiable", st)
		}
		if strings.Contains(d, secret) {
			t.Fatalf("detail leaked the key: %q", d)
		}
	})
}

// TestFinanceTolerance pins the documented numeric rule directly.
func TestFinanceTolerance(t *testing.T) {
	if floatTolerance != 1e-6 {
		t.Fatalf("floatTolerance = %g, want 1e-6 (documented constant)", floatTolerance)
	}
	cases := []struct {
		claimed  string
		fetched  float64
		isInt    bool
		wantPass bool
	}{
		{"100", 100, true, true},         // exact int
		{"100", 101, true, false},        // int mismatch, no tolerance
		{"1.0000005", 1.0, false, true},  // float just inside
		{"1.0000020", 1.0, false, false}, // float just outside
		{"abc", 1.0, false, false},       // non-numeric claim
		{"", 1.0, false, false},          // empty claim
	}
	for _, tc := range cases {
		got, _ := numericMatch(tc.claimed, tc.fetched, tc.isInt)
		if got != tc.wantPass {
			t.Errorf("numericMatch(%q, %g, int=%v) = %v, want %v", tc.claimed, tc.fetched, tc.isInt, got, tc.wantPass)
		}
	}
}

// TestFinanceHosts guards the egress co-design contract (P11-T35 cross-check input).
func TestFinanceHosts(t *testing.T) {
	want := map[string]bool{"data.sec.gov": false, "api.stlouisfed.org": false}
	for _, h := range Hosts {
		if _, ok := want[h]; ok {
			want[h] = true
		}
	}
	for h, seen := range want {
		if !seen {
			t.Errorf("Hosts missing %q", h)
		}
	}
}
