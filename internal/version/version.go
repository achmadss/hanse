// Package version holds the build version of the Hanse binaries.
// Release builds inject the real version via
// -ldflags "-X github.com/achmadss/hanse/internal/version.Version=...".
package version

// Version is the version string reported by the Hanse binaries.
var Version = "dev"
