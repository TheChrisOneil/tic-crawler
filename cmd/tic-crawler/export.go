package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/TheChrisOneil/tic-crawler/internal/config"
	"github.com/TheChrisOneil/tic-crawler/internal/discovery"
	"github.com/TheChrisOneil/tic-crawler/internal/export"
	"github.com/TheChrisOneil/tic-crawler/internal/model"
	"github.com/TheChrisOneil/tic-crawler/internal/state"
)

func today() time.Time { return time.Now().UTC().Truncate(24 * time.Hour) }

// knownPayers returns the saved payer list, or the seed file when nothing has been discovered yet.
func knownPayers(st *state.Store, s config.Settings) ([]model.Payer, error) {
	payers, err := st.LoadPayers()
	if err != nil {
		return nil, err
	}
	if len(payers) == 0 {
		if _, err := os.Stat(s.SeedFile); err == nil {
			return discovery.LoadSeedFile(s.SeedFile)
		}
	}
	return payers, nil
}

func exportDashboardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export-dashboard",
		Short: "Write dashboard JSON (data.json and per-payer detail) under out/dashboard/",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			st, err := state.Open(s.StateDB)
			if err != nil {
				return err
			}
			defer st.Close()
			list, err := knownPayers(st, s)
			if err != nil {
				return err
			}
			payers := map[string]model.Payer{}
			for _, p := range list {
				payers[p.PayerID] = p
			}
			dest, err := export.Build(s, st, payers, today())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "dashboard data written to %s\n", dest)
			return nil
		},
	}
}
