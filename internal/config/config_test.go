package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadRepoConfig(t *testing.T) {
	s, err := Load("../../config.toml", map[string]string{"TIC_CONTACT_EMAIL": "a@b.c"})
	if err != nil {
		t.Fatal(err)
	}
	if s.SchemaVersion != "2.2.1" || s.MaxRunBytes != 10737418240 || s.RequestsPerSecondHost != 1 {
		t.Fatalf("%+v", s)
	}
	if s.UserAgent() != "EverBetter-TiC-Crawler/1.0 (+a@b.c)" {
		t.Fatal(s.UserAgent())
	}
	if !s.QuarterlyFrom.Equal(time.Date(2027, 3, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatal(s.QuarterlyFrom)
	}
	if s.StaleDays(time.Date(2027, 3, 6, 0, 0, 0, 0, time.UTC)) != 100 || s.StaleDays(time.Date(2027, 3, 5, 0, 0, 0, 0, time.UTC)) != 35 {
		t.Fatal("stale days")
	}
}

func TestUnknownKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(p, []byte("[http]\nbogus = 1\n"), 0o644)
	if _, err := Load(p, map[string]string{}); err == nil {
		t.Fatal("want error")
	}
}

func TestMissingFileUsesDefaults(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "none.toml"), map[string]string{"TIC_OUTPUT_DIR": "x"})
	if err != nil || s.LocalDir != "x" || s.UserAgent() != "EverBetter-TiC-Crawler/1.0 (+unset)" {
		t.Fatal(err, s.LocalDir)
	}
}
