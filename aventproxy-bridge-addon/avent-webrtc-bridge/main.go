package main

import (
	"fmt"
	"os"

	"avent-webrtc-bridge/cmd"
	"avent-webrtc-bridge/pkg/core"
)

// version is stamped at build time from the app's config.yaml version, which
// the Supervisor passes in as BUILD_VERSION.
var version = "dev"

func main() {
	core.InitLogger()

	if err := cmd.Execute(version); err != nil {
		fmt.Println("Command execution failed")
		os.Exit(1)
	}
}
