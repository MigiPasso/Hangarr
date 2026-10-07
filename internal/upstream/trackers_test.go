package upstream

import (
	"strings"
	"testing"

	"hangarr/internal/clients/sonarr"
)

func failing(ts []Tracker) string {
	var names []string
	for _, t := range ts {
		if t.Failing {
			names = append(names, t.Name)
		}
	}
	return strings.Join(names, " | ")
}

func TestTrackersFromHealthChecks(t *testing.T) {
	indexers := []sonarr.Indexer{
		{ID: 1, Name: "Orion", EnableRss: true},
		{ID: 2, Name: "Orion - S (Prowlarr)", EnableRss: true},
		{ID: 3, Name: "Kestrel - S (Prowlarr)", EnableRss: true, EnableAutomaticSearch: true},
		{ID: 4, Name: "Nimbus (Prowlarr)", EnableInteractiveSearch: true},
		{ID: 5, Name: "off"},
	}
	cases := []struct {
		name   string
		alerts []sonarr.HealthCheck
		want   string
	}{
		{"none", nil, ""},
		// Messages of a real Sonarr (screenshot of the user's instance).
		{"long term", []sonarr.HealthCheck{{Source: "IndexerLongTermStatusCheck",
			Message: "Indexers unavailable due to failures for more than 6 hours: Kestrel - S (Prowlarr)"}}, "Kestrel - S (Prowlarr)"},
		// "Orion" is a prefix of another name: only exact items match.
		{"several, French", []sonarr.HealthCheck{{Source: "IndexerStatusCheck",
			Message: "Indexeurs indisponibles en raison d'échecs : Orion - S (Prowlarr), Nimbus (Prowlarr)"}},
			"Nimbus (Prowlarr) | Orion - S (Prowlarr)"},
		{"all unavailable", []sonarr.HealthCheck{{Source: "IndexerStatusCheck",
			Message: "All indexers are unavailable due to failures"}},
			"Kestrel - S (Prowlarr) | Nimbus (Prowlarr) | Orion | Orion - S (Prowlarr)"},
		{"other checks ignored", []sonarr.HealthCheck{{Source: "IndexerRssCheck",
			Message: "No indexers available with RSS sync enabled: Orion"}}, ""},
		// Full-width colon without space (Chinese interface).
		{"full-width colon", []sonarr.HealthCheck{{Source: "IndexerStatusCheck",
			Message: "由于故障，索引器不可用：Kestrel - S (Prowlarr)"}}, "Kestrel - S (Prowlarr)"},
		// A name is there but not as a list item: never "all failing".
		{"unrecognized list", []sonarr.HealthCheck{{Source: "IndexerStatusCheck",
			Message: "Indexers unavailable (Orion - S (Prowlarr)) due to failures"}}, ""},
	}
	for _, c := range cases {
		got := trackers(indexers, c.alerts)
		if f := failing(got); f != c.want {
			t.Errorf("%s: failing %q, want %q", c.name, f, c.want)
		}
		if len(got) != len(indexers) || got[len(got)-1].Name != "off" || got[len(got)-1].Failing {
			t.Errorf("%s: a disabled indexer comes last and never fails: %+v", c.name, got[len(got)-1])
		}
	}
	if !IsIndexerAlert(sonarr.HealthCheck{Source: "IndexerRssCheck"}) || IsIndexerAlert(sonarr.HealthCheck{Source: "DownloadClientCheck"}) {
		t.Error("indexer alerts")
	}
}

func TestNamedIn(t *testing.T) {
	msg := "Indexers unavailable due to failures: Orion - S (Prowlarr), Lumen"
	for name, want := range map[string]bool{"Orion - S (Prowlarr)": true, "Lumen": true, "Orion": false, "S (Prowlarr)": false,
		"seed": false, "": false} {
		if got := namedIn(msg, name); got != want {
			t.Errorf("%q: %v", name, got)
		}
	}
	if !namedIn("Indexers unavailable: A.", "A") {
		t.Error("a final period ends the list")
	}
}
