// Package mathx is a deliberately-buggy fixture used by the smoke test: Add is
// wrong, so its test fails. The native backend's job is to make the test pass
// by fixing the bug — the smallest possible change.
package mathx

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a - b // BUG: should be a + b
}
