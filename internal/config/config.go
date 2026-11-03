// Package config loads settings from config.toml plus environment variables.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"

	"github.com/BurntSushi/toml"
)

const UserAgentBase = "EverBetter-TiC-Crawler/1.0"

type Settings struct {
	SchemaVersion         string
	MaxErrorsPerTOC       int
	RequestsPerSecondHost float64
	Concurrency           int
	TimeoutSeconds        float64
	MaxRetries            int
	BackoffBaseSeconds    float64
	BackoffMaxSeconds     float64
	MaxTOCBytes           int64
	MaxRunBytes           int64
	QuarterlyFrom         time.Time
	StaleDaysMonthly      int
	StaleDaysQuarterly    int
	RootTxtFrom           time.Time
	MinRecrawlDays        int
	SeedFile              string
	ExchangePUFURL        string
	ExchangePUFMaxBytes   int64
	RootTxtFilename       string
	LocalDir              string
	StateDB               string
	SheetName             string
	ManifestPlanNameCap   int
	ContactEmail          string
	DriveFolderID         string
	RawStore              string
	CredentialsPath       string
}

func Defaults() Settings {
	return Settings{
		SchemaVersion: "2.2.1", MaxErrorsPerTOC: 50,
		RequestsPerSecondHost: 1, Concurrency: 8, TimeoutSeconds: 60, MaxRetries: 5,
		BackoffBaseSeconds: 1, BackoffMaxSeconds: 60,
		MaxTOCBytes: 2 << 30, MaxRunBytes: 10 << 30,
		QuarterlyFrom: date(2027, 3, 6), StaleDaysMonthly: 35, StaleDaysQuarterly: 100,
		RootTxtFrom: date(2027, 9, 6), MinRecrawlDays: 6,
		SeedFile: "seeds/payers.yaml", ExchangePUFMaxBytes: 500 << 20,
		RootTxtFilename: "price-transparency.txt", LocalDir: "out", StateDB: "state/tic.sqlite",
		SheetName: "TiC Payer Registry", ManifestPlanNameCap: 50,
	}
}

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// UserAgent is "EverBetter-TiC-Crawler/1.0 (+contact)".
func (s Settings) UserAgent() string {
	c := s.ContactEmail
	if c == "" {
		c = "unset"
	}
	return fmt.Sprintf("%s (+%s)", UserAgentBase, c)
}

func (s Settings) StaleDays(today time.Time) int {
	if !today.Before(s.QuarterlyFrom) {
		return s.StaleDaysQuarterly
	}
	return s.StaleDaysMonthly
}

// Load reads path (default $TIC_CONFIG or config.toml). env defaults to the process environment.
func Load(path string, env map[string]string) (Settings, error) {
	if env == nil {
		env = map[string]string{}
		for _, k := range []string{"TIC_CONFIG", "TIC_CONTACT_EMAIL", "TIC_DRIVE_FOLDER_ID", "TIC_RAW_STORE",
			"GOOGLE_APPLICATION_CREDENTIALS", "TIC_STATE_DB", "TIC_OUTPUT_DIR"} {
			if v, ok := os.LookupEnv(k); ok {
				env[k] = v
			}
		}
	}
	if path == "" {
		path = env["TIC_CONFIG"]
	}
	if path == "" {
		path = "config.toml"
	}
	s := Defaults()
	raw := map[string]map[string]any{}
	if _, err := toml.DecodeFile(path, &raw); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return s, err
	}
	var unknown []string
	for section, kv := range raw {
		for k, v := range kv {
			if !s.set(section, k, v) {
				unknown = append(unknown, section+"."+k)
			}
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return s, fmt.Errorf("unknown config keys: %v", unknown)
	}
	s.ContactEmail = env["TIC_CONTACT_EMAIL"]
	s.DriveFolderID = env["TIC_DRIVE_FOLDER_ID"]
	s.RawStore = env["TIC_RAW_STORE"]
	s.CredentialsPath = env["GOOGLE_APPLICATION_CREDENTIALS"]
	if v := env["TIC_STATE_DB"]; v != "" {
		s.StateDB = v
	}
	if v := env["TIC_OUTPUT_DIR"]; v != "" {
		s.LocalDir = v
	}
	return s, nil
}

func toF(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

func toD(v any) time.Time {
	switch x := v.(type) {
	case time.Time:
		return x
	case string:
		t, _ := time.Parse("2006-01-02", x)
		return t
	}
	return time.Time{}
}

func (s *Settings) set(section, key string, v any) bool {
	str, _ := v.(string)
	i := int(toF(v))
	i64 := int64(toF(v))
	switch section + "." + key {
	case "schema.version":
		s.SchemaVersion = str
	case "schema.max_errors_per_toc":
		s.MaxErrorsPerTOC = i
	case "http.requests_per_second_per_host":
		s.RequestsPerSecondHost = toF(v)
	case "http.concurrency":
		s.Concurrency = i
	case "http.timeout_seconds":
		s.TimeoutSeconds = toF(v)
	case "http.max_retries":
		s.MaxRetries = i
	case "http.backoff_base_seconds":
		s.BackoffBaseSeconds = toF(v)
	case "http.backoff_max_seconds":
		s.BackoffMaxSeconds = toF(v)
	case "limits.max_toc_bytes":
		s.MaxTOCBytes = i64
	case "limits.max_run_bytes":
		s.MaxRunBytes = i64
	case "schedule.quarterly_from":
		s.QuarterlyFrom = toD(v)
	case "schedule.stale_days_monthly":
		s.StaleDaysMonthly = i
	case "schedule.stale_days_quarterly":
		s.StaleDaysQuarterly = i
	case "schedule.root_txt_from":
		s.RootTxtFrom = toD(v)
	case "schedule.min_recrawl_days":
		s.MinRecrawlDays = i
	case "discovery.seed_file":
		s.SeedFile = str
	case "discovery.exchange_puf_url":
		s.ExchangePUFURL = str
	case "discovery.exchange_puf_max_bytes":
		s.ExchangePUFMaxBytes = i64
	case "discovery.root_txt_filename":
		s.RootTxtFilename = str
	case "output.local_dir":
		s.LocalDir = str
	case "output.state_db":
		s.StateDB = str
	case "output.sheet_name":
		s.SheetName = str
	case "output.manifest_plan_name_cap":
		s.ManifestPlanNameCap = i
	default:
		// Sections owned by the Go-only commands (reference data, extraction) are read elsewhere.
		return section == "reference" || section == "extract"
	}
	return true
}
