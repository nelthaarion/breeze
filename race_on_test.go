//go:build race

package breeze

// raceEnabled is true when the tests are built with the race detector, under
// which sync.Pool deliberately drops items at random, so allocation counts of
// pooled paths are not meaningful.
const raceEnabled = true
