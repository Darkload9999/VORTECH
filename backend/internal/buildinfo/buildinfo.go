// Package buildinfo exposes version metadata injected at link time.
//
//	go build -ldflags "-X github.com/Darkload9999/VORTECH/backend/internal/buildinfo.Version=v0.1.0 ..."
package buildinfo

// These values are overridden via -ldflags at build time. The defaults identify
// a local, untagged build.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)
