//go:build !race

package cli

// raceDetector reports whether the tests run under -race, which makes each
// subprocess several times slower.
const raceDetector = false
