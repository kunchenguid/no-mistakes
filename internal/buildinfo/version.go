package buildinfo

import "runtime/debug"

// Set via ldflags at build time:
//
//	-ldflags "-X github.com/kunchenguid/no-mistakes/internal/buildinfo.Version=v1.0.0
//	          -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Commit=abc1234
//	          -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Date=2024-01-01"
var (
	Version            = "dev"
	Commit             = "unknown"
	Date               = "unknown"
	TelemetryWebsiteID = ""
)

func CurrentVersion() string {
	if Version != "" && Version != "dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
	}
	return "dev"
}

func String() string {
	return CurrentVersion() + " (" + Commit + ") " + Date
}
