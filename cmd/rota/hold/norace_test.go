//go:build !race

package hold

// slow is how many times longer a test that times its code allows it, for
// the race detector's overhead.
const slow = 1
