package upstream

import (
	"strings"
	"testing"
	"time"

	"hangarr/internal/clients/httpx"
	"hangarr/internal/clients/sonarr"
)

func TestTrackers(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	at := func(h int) httpx.Time { return httpx.Time{Time: now.Add(time.Duration(h) * time.Hour)} }
	got := trackers([]sonarr.Indexer{
		{ID: 1, Name: "beta", EnableRss: true},
		{ID: 2, Name: "Alpha", EnableAutomaticSearch: true},
		{ID: 3, Name: "off"},
		{ID: 4, Name: "zeta", EnableRss: true},
		{ID: 5, Name: "healed", EnableRss: true},
	}, []sonarr.IndexerStatus{
		{IndexerID: 4, InitialFailure: at(-5), DisabledTill: at(2)},
		{IndexerID: 5, InitialFailure: at(-5), DisabledTill: at(-1)}, // expired: back in use
	}, now)
	var names []string
	for _, tr := range got {
		names = append(names, tr.Name)
	}
	if want := "zeta Alpha beta healed off"; strings.Join(names, " ") != want {
		t.Fatalf("order %q, want %q", strings.Join(names, " "), want)
	}
	if z := got[0]; !z.Failing() || !z.DisabledUntil.Equal(now.Add(2*time.Hour)) {
		t.Errorf("failing tracker %+v", z)
	}
	if got[3].Failing() || got[4].Enabled() {
		t.Errorf("healed / off: %+v %+v", got[3], got[4])
	}
	if !IsIndexerAlert(sonarr.HealthCheck{Source: "IndexerRssCheck"}) || IsIndexerAlert(sonarr.HealthCheck{Source: "DownloadClientCheck"}) {
		t.Error("indexer alerts")
	}
}
