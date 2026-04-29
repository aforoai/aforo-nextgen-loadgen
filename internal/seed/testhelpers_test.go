package seed

import (
	"fmt"
	"os"
)

// openTrunc opens path for writing, creating + truncating if it exists.
// Used by manifest_test.go to write fixture files.
func openTrunc(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
}

// pad renders an int with a width-N zero-padded suffix appended to prefix.
func pad(prefix string, n, width int) string {
	return fmt.Sprintf("%s%0*d", prefix, width, n)
}
