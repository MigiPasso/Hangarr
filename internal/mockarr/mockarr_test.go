package mockarr

import (
	"os"
	"testing"
)

func TestDevExtras(t *testing.T) {
	m := New(os.DirFS("../../testdata"), "k")
	base, err := m.catalog("sonarr")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.item("sonarr", 10); ok {
		t.Error("extras must stay out of the synthetic catalog")
	}
	m.DevExtras = true
	for _, svc := range []string{"sonarr", "radarr"} {
		all, err := m.catalog(svc)
		if err != nil {
			t.Fatal(err)
		}
		ids := map[float64]bool{}
		for _, it := range all {
			id, _ := it["id"].(float64)
			if ids[id] {
				t.Errorf("%s: duplicate id %v", svc, id)
			}
			ids[id] = true
		}
	}
	all, _ := m.catalog("sonarr")
	if len(all) <= len(base) {
		t.Errorf("extras: %d series, base %d", len(all), len(base))
	}
	if it, ok := m.item("sonarr", 10); !ok || it["title"] != "The Last of Us" {
		t.Errorf("extra series by id: %v %v", ok, it["title"])
	}
}
