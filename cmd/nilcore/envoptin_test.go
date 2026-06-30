package main

import "testing"

func TestEnvOptIn(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},      // unset/empty ⇒ off
		{"0", false},     // the footgun: must read as off, not on
		{"false", false}, // explicit negatives ⇒ off
		{"no", false},
		{"off", false},
		{" 0 ", false}, // trimmed
		{"FALSE", false},
		{"1", true},
		{"true", true},
		{"on", true},
		{"yes", true},
	}
	for _, c := range cases {
		t.Setenv("NILCORE_TEST_OPTIN", c.val)
		if got := envOptIn("NILCORE_TEST_OPTIN"); got != c.want {
			t.Errorf("envOptIn(%q) = %v, want %v", c.val, got, c.want)
		}
	}
}
