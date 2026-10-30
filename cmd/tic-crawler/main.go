// Command tic-crawler (Go). Commands are added here as they are ported from the Python tool.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/TheChrisOneil/tic-crawler/internal/config"
	"github.com/TheChrisOneil/tic-crawler/internal/fetch"
)

var cfgPath string

// newClient builds the HTTP client for every command; tests swap it for one without a network.
var newClient = fetch.New

func settings() (config.Settings, error) { return config.Load(cfgPath, nil) }

func main() {
	root := &cobra.Command{
		Use:           "tic-crawler",
		Short:         "Find, catalog and filter Transparency in Coverage machine-readable files",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&cfgPath, "config", "", "config file (default $TIC_CONFIG or config.toml)")
	root.AddCommand(extractRatesCmd(), referenceCmd(), joinQualityCmd(), exportDashboardCmd(),
		discoverCmd(), crawlCmd(), verifySeedsCmd(), auditAccessCmd(), probeCmd(), syncDriveCmd(), reportCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
