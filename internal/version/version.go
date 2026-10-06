package version

// Version is the semantic version of niclane. Release builds override it via
// -ldflags "-X github.com/CallMeHFK/niclane/internal/version.Version=...".
var Version = "0.3.0"
