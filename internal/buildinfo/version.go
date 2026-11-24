package buildinfo

// Version is set for release builds with -ldflags. Development builds identify as dev.
var Version = "dev"

// Commit is the git revision injected at build time with -ldflags. It is
// "unknown" for plain go build/go test runs.
var Commit = "unknown"
