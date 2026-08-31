package main

import (
	"context"
	"fmt"
	"os"

	"github.com/openmcp-project/platform-service-dns/cmd/platform-service-dns/app"

	"github.com/openmcp-project/controller-utils/pkg/fips"
)

func main() {
	fips.Verify(context.Background())

	cmd := app.NewPlatformServiceDNSCommand()

	if err := cmd.Execute(); err != nil {
		fmt.Print(err)
		os.Exit(1)
	}
}
