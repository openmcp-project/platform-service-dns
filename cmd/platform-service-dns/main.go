package main

import (
	"fmt"
	"os"

	"github.com/openmcp-project/platform-service-dns/cmd/platform-service-dns/app"
)

func main() {
	cmd := app.NewPlatformServiceDNSCommand()

	if err := cmd.Execute(); err != nil {
		fmt.Print(err)
		os.Exit(1)
	}
}
