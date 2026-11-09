//go:build race

package tacdb

// raceDetector is set when the race detector runs, which makes the
// oracle ten times slower: it then checks fewer graphs.
const raceDetector = true
