package model

import (
	"reflect"
	"testing"
	"time"
)

func TestRenderTocURLs(t *testing.T) {
	today := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	got := RenderTocURLs("https://x/{date:%Y-%m}-01_index.json", today)
	want := []string{"https://x/2026-01-01_index.json", "https://x/2025-12-01_index.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if got := RenderTocURLs("https://x/a.json", today); !reflect.DeepEqual(got, []string{"https://x/a.json"}) {
		t.Fatal(got)
	}
	if RenderTocURLs("", today) != nil {
		t.Fatal("empty")
	}
}
