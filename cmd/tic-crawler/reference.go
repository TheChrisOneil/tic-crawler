package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/TheChrisOneil/tic-crawler/internal/extract"
	"github.com/TheChrisOneil/tic-crawler/internal/fetch"
	"github.com/TheChrisOneil/tic-crawler/internal/quality"
	"github.com/TheChrisOneil/tic-crawler/internal/reference"
)

func configPath() string {
	if cfgPath != "" {
		return cfgPath
	}
	if v := os.Getenv("TIC_CONFIG"); v != "" {
		return v
	}
	return "config.toml"
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func referenceCmd() *cobra.Command {
	var outDir, nppesZip string
	cmd := &cobra.Command{Use: "reference", Short: "Download CMS provider reference data (NPPES, Care Compare)"}

	nppes := &cobra.Command{
		Use:   "nppes",
		Short: "Filter the monthly NPPES NPI file to the configured states",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			rc, err := reference.LoadConfig(configPath())
			if err != nil {
				return err
			}
			ctx := context.Background()
			c := fetch.New(s)
			src := nppesZip
			info := map[string]any{"states": rc.States}
			if src == "" {
				page, err := c.GetText(ctx, rc.NPPESIndexURL, 4<<20)
				if err != nil {
					return err
				}
				u, err := reference.FindNPPESZip(page, rc.NPPESIndexURL)
				if err != nil {
					return err
				}
				info["source_url"] = u
				tmp, err := os.CreateTemp("", "nppes-*.zip")
				if err != nil {
					return err
				}
				defer os.Remove(tmp.Name())
				resp, err := c.Get(ctx, u)
				if err != nil {
					tmp.Close()
					return err
				}
				n, err := io.Copy(tmp, &budgetReader{r: resp.Body, limit: rc.NPPESMaxBytes})
				resp.Body.Close()
				tmp.Close()
				if err != nil {
					return fmt.Errorf("NPPES download: %w", err)
				}
				info["bytes_downloaded"] = n
				src = tmp.Name()
			}
			n, inner, err := reference.NPPESFromZip(src, rcStates(rc), filepath.Join(outDir, "nppes.csv.gz"))
			if err != nil {
				return err
			}
			info["file"], info["rows_kept"] = inner, n
			fmt.Fprintf(cmd.OutOrStdout(), "nppes: %d providers in %v from %s\n", n, rc.States, inner)
			return writeJSON(filepath.Join(outDir, "nppes.json"), info)
		},
	}
	nppes.Flags().StringVar(&nppesZip, "zip", "", "use a local NPPES zip instead of downloading")

	pdc := &cobra.Command{
		Use:   "provider-data",
		Short: "Download the configured CMS Provider Data Catalog datasets, filtered to the states",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			rc, err := reference.LoadConfig(configPath())
			if err != nil {
				return err
			}
			ctx := context.Background()
			c := fetch.New(s)
			var metas []reference.DatasetMeta
			// State-filtered datasets first; national files without a state column (facility
			// affiliations) are then filtered to the NPIs found in NPPES and the clinician file.
			ids := reference.SortedKeys(rc.Datasets)
			sort.SliceStable(ids, func(i, j int) bool { return !npiFiltered[ids[i]] && npiFiltered[ids[j]] })
			var npis map[string]bool
			for _, id := range ids {
				if npiFiltered[id] && npis == nil {
					npis = map[string]bool{}
					for _, f := range []string{filepath.Join(outDir, "nppes.csv.gz"), filepath.Join(outDir, "provider-data", dsClinicians+".csv.gz")} {
						set, err := reference.LoadNPISet(f)
						if err == nil {
							for k := range set {
								npis[k] = true
							}
						}
					}
				}
				m, err := reference.DatasetInfo(ctx, c, rc.ProviderDataAPI, id, rc.Datasets[id])
				if err != nil {
					return err
				}
				resp, err := c.Get(ctx, m.DownloadURL)
				if err != nil {
					return fmt.Errorf("%s: %w", id, err)
				}
				m.File = filepath.Join("provider-data", id+".csv.gz")
				m.Rows, err = reference.FilterDataset(&budgetReader{r: resp.Body, limit: s.MaxRunBytes}, rcStates(rc), npis,
					filepath.Join(outDir, m.File))
				resp.Body.Close()
				if err != nil {
					return fmt.Errorf("%s: %w", id, err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %q: %d rows (modified %s)\n", id, m.Title, m.Rows, m.Modified)
				metas = append(metas, m)
			}
			return writeJSON(filepath.Join(outDir, "provider-data.json"), metas)
		},
	}
	var payer, ratesDir string
	lookup := &cobra.Command{
		Use:   "npi-lookup",
		Short: "Look up facility NPIs from extracted institutional rates in the NPI Registry API",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			rc, err := reference.LoadConfig(configPath())
			if err != nil {
				return err
			}
			known, _ := reference.LoadNPISet(filepath.Join(outDir, "nppes.csv.gz"))
			npis, err := quality.InstitutionalNPIs(filepath.Join(ratesDir, payer))
			if err != nil {
				return err
			}
			var todo []string
			for _, n := range npis {
				if !known[n] {
					todo = append(todo, n)
				}
			}
			looked, kept, err := reference.LookupNPIs(context.Background(), fetch.New(s), rc.NPIAPIURL, todo, rcStates(rc),
				rc.NPILookupMax, filepath.Join(outDir, "npi-lookup.csv.gz"))
			fmt.Fprintf(cmd.OutOrStdout(), "npi-lookup: %d institutional NPIs, %d looked up, %d in %v\n", len(npis), looked, kept, rc.States)
			if err != nil {
				return err
			}
			return writeJSON(filepath.Join(outDir, "npi-lookup.json"), map[string]any{"payer_id": payer,
				"institutional_npis": len(npis), "looked_up": looked, "kept": kept, "max": rc.NPILookupMax})
		},
	}
	lookup.Flags().StringVar(&payer, "payer", "", "payer id")
	lookup.Flags().StringVar(&ratesDir, "rates", "out/rates", "extract-rates output folder")
	lookup.MarkFlagRequired("payer")
	cmd.PersistentFlags().StringVar(&outDir, "out", "out/reference", "output folder")
	cmd.AddCommand(nppes, pdc, lookup)
	return cmd
}

func rcStates(rc reference.Config) map[string]bool {
	m := map[string]bool{}
	for _, s := range rc.States {
		m[s] = true
	}
	return m
}

// Dataset ids used by the join; they must also be listed in [reference.datasets].
const (
	dsHospitals    = "xubh-q36u"
	dsAffiliations = "27ea-46a8"
	dsClinicians   = "mj5m-pzi6"
)

// Datasets without a state column, filtered by NPI instead.
var npiFiltered = map[string]bool{dsAffiliations: true}

var measureDatasets = map[string]string{
	"ynj2-r877": "complications_and_deaths",
	"77hc-ibv8": "healthcare_associated_infections",
	"dgck-syfz": "hcahps",
	"632h-zaca": "unplanned_hospital_visits",
	"yv7e-xc69": "timely_and_effective_care",
}

func loadLabels(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cl codeList
	if err := yaml.Unmarshal(b, &cl); err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, c := range cl.Codes {
		m[extract.CodeKey(c.Type, c.Code)] = c.Label
	}
	return m, nil
}

func joinQualityCmd() *cobra.Command {
	var payer, refDir, ratesDir, outDir, codesPath string
	cmd := &cobra.Command{
		Use:   "join-quality",
		Short: "Link providers to CMS hospitals and join extracted prices to Care Compare measures",
		RunE: func(cmd *cobra.Command, _ []string) error {
			pd := filepath.Join(refDir, "provider-data")
			providers, err := quality.LoadProviders(filepath.Join(refDir, "nppes.csv.gz"), filepath.Join(refDir, "npi-lookup.csv.gz"))
			if err != nil {
				return err
			}
			if err := quality.LoadDACProviders(filepath.Join(pd, dsClinicians+".csv.gz"), providers); err != nil {
				return err
			}
			hospitals, err := quality.LoadHospitals(filepath.Join(pd, dsHospitals+".csv.gz"))
			if err != nil {
				return err
			}
			aff, err := quality.LoadAffiliations(filepath.Join(pd, dsAffiliations+".csv.gz"))
			if err != nil {
				return err
			}
			links := quality.BuildCrosswalk(providers, hospitals, aff)
			if err := quality.WriteCrosswalk(filepath.Join(outDir, "crosswalk.csv.gz"), links, providers, hospitals); err != nil {
				return err
			}
			mw, err := reference.CreateGzCSV(filepath.Join(outDir, "measures.csv.gz"), quality.MeasureHeader)
			if err != nil {
				return err
			}
			for _, id := range reference.SortedKeys(measureDatasets) {
				if _, err := quality.AppendMeasures(mw, filepath.Join(pd, id+".csv.gz"), measureDatasets[id], nil); err != nil {
					mw.Close()
					return err
				}
			}
			if err := mw.Close(); err != nil {
				return err
			}
			labels, err := loadLabels(codesPath)
			if err != nil {
				return err
			}
			sum, err := quality.JoinPrices(payer, filepath.Join(ratesDir, payer), filepath.Join(outDir, payer), providers, hospitals, links, labels)
			if err != nil {
				return err
			}
			byMethod := map[string]int{}
			for _, l := range links {
				byMethod[l.Method]++
			}
			fmt.Fprintf(cmd.OutOrStdout(), "crosswalk: %d links %v; %s: %d rows in states, %d providers, %d linked to %d hospitals\n",
				len(links), byMethod, payer, sum.RowsInStates, sum.Providers, sum.ProvidersWithCCN, sum.Hospitals)
			return writeJSON(filepath.Join(outDir, "crosswalk.json"), map[string]any{"links": len(links), "by_method": byMethod,
				"hospitals": len(hospitals), "providers_in_states": len(providers)})
		},
	}
	f := cmd.Flags()
	f.StringVar(&payer, "payer", "", "payer id")
	f.StringVar(&refDir, "reference", "out/reference", "reference data folder")
	f.StringVar(&ratesDir, "rates", "out/rates", "extract-rates output folder")
	f.StringVar(&outDir, "out", "out/quality", "output folder")
	f.StringVar(&codesPath, "codes", "config/codes.yaml", "billing code labels")
	cmd.MarkFlagRequired("payer")
	return cmd
}
