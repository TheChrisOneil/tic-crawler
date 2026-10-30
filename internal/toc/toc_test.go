package toc

import (
	"bytes"
	"compress/gzip"
	"os"
	"testing"
)

func TestReadSampleTOC(t *testing.T) {
	b, err := os.ReadFile("../../testdata/toc/cms-sample.json")
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(b)
	w.Close()
	for _, src := range [][]byte{b, gz.Bytes()} {
		r, err := MaybeGunzip(bytes.NewReader(src))
		if err != nil {
			t.Fatal(err)
		}
		c := NewCollector(50)
		h, err := Read(r, func(it Item) error { c.Add(it); return nil })
		if err != nil {
			t.Fatal(err)
		}
		if h.ReportingEntityName == "" || len(c.Entries()) == 0 {
			t.Fatalf("%+v %d", h, len(c.Entries()))
		}
		for _, e := range c.ByPlanCount() {
			if e.FileType != "in-network" || e.PlanCount == 0 {
				t.Fatalf("%+v", e)
			}
		}
	}
}

func TestCollectorDedupesAndCaps(t *testing.T) {
	c := NewCollector(1)
	loc := FileLoc{Description: "d", Location: "https://x/a.json"}
	c.Add(Item{ReportingPlans: []Plan{{PlanName: "A", PlanID: "1"}, {PlanName: "B", PlanID: "2"}}, InNetworkFiles: []FileLoc{loc}})
	c.Add(Item{ReportingPlans: []Plan{{PlanName: "C", PlanID: "3"}}, InNetworkFiles: []FileLoc{loc, {Location: "https://x/b.json"}},
		AllowedAmountFile: &FileLoc{Location: "https://x/aa.json"}})
	es := c.Entries()
	if len(es) != 3 || es[0].PlanCount != 3 || len(es[0].PlanNames) != 1 || len(es[0].PlanIDs) != 3 {
		t.Fatalf("%+v", es[0])
	}
	if got := c.ByPlanCount(); len(got) != 2 || got[0].URL != "https://x/a.json" {
		t.Fatalf("%+v", got)
	}
}

func TestReadRejectsNonObject(t *testing.T) {
	if _, err := Read(bytes.NewReader([]byte(`[1]`)), func(Item) error { return nil }); err == nil {
		t.Fatal("want error")
	}
}
