package agent

import "testing"

// sessionPrefix is unexported, so this white-box test lives in package agent. It is the
// correlation key auto-reattach uses to pair a suspended `<conv>-3` with its resuming
// `<conv>-4`, so its edge behavior (last '-', index-0 guard, no '-') is load-bearing.
func TestSessionPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"conv123-3", "conv123"},
		{"conv123-4", "conv123"}, // the resuming sibling shares the prefix
		{"a-b-c-10", "a-b-c"},    // up to the LAST '-'
		{"t-1751000000", "t"},    // a run task id (prefix "t") — never a session
		{"nodash", ""},           // no '-' ⇒ no session shape
		{"-leading", ""},         // only '-' at index 0 ⇒ nothing to key on
		{"", ""},                 // empty
		{"conv-", "conv"},        // trailing '-' still keys on "conv"
	}
	for _, tc := range cases {
		if got := sessionPrefix(tc.in); got != tc.want {
			t.Errorf("sessionPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A suspended `<conv>-3` and its resuming `<conv>-4` correlate; an unrelated run task
// (prefix "t") never does.
func TestSessionPrefixCorrelation(t *testing.T) {
	if sessionPrefix("conv-3") != sessionPrefix("conv-4") {
		t.Error("a suspended sibling and its wake-resume must share a session prefix")
	}
	if sessionPrefix("conv-4") == sessionPrefix("t-1751000000") {
		t.Error("a session task must not correlate with an unrelated run task")
	}
}
