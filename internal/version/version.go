// Package version holds the build-time version stamp shared by the daemon,
// CLI, and tracking reporter. Injected via ldflags:
//
//	-X github.com/eushing/agentwork/internal/version.DaemonVersion=$VERSION
//
// When ldflags are absent (development build), defaults to "0.0.1-beta.1".
package version

// DaemonVersion is the daemon build version, injected via ldflags alongside
// the CLI's main.cliVersion. Both binaries MUST be built with the same stamp:
// the register-time version check between the CLI and the daemon warns on a
// mismatch (protocol drift between an old binary and a new daemon surfaces at
// connect time). Kept in a standalone package so track can import it without
// pulling in daemon (daemon → track → daemon cycle).
var DaemonVersion = "0.0.1-beta.1"
