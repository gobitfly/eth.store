package version

import "runtime"

// Build information. Populated at build-time
var (
	GitDate   = "undefined"
	GitCommit = "undefined"
	Version   = "undefined"
	GoVersion = runtime.Version()
)
