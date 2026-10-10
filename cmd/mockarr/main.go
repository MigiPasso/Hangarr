// Command mockarr serves fake Sonarr (:8989), Radarr (:7878), Plex (:32400),
// Jellyfin (:8096) and Tautulli (:8181) from fixture files, for development
// and e2e tests. See internal/mockarr.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"hangarr/internal/mockarr"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	key := os.Getenv("MOCK_API_KEY")
	if key == "" {
		key = "mock-api-key"
	}
	dir := os.Getenv("MOCK_FIXTURES")
	if dir == "" {
		dir = "/testdata"
	}
	m := mockarr.New(os.DirFS(dir), key)
	m.DevExtras, _ = strconv.ParseBool(os.Getenv("MOCK_DEV_EXTRAS"))
	m.RealPosters, _ = strconv.ParseBool(os.Getenv("MOCK_REAL_POSTERS"))
	if shift, _ := strconv.ParseBool(os.Getenv("MOCK_SHIFT_DATES")); shift {
		m.ShiftToNow(time.Now())
		log.Info("history dates shifted", "days", m.ShiftSeconds/86400)
	}

	services := []struct {
		name, addr string
		handler    http.Handler
	}{
		{"sonarr", ":8989", m.Sonarr()},
		{"radarr", ":7878", m.Radarr()},
		{"plex", ":32400", m.Plex()},
		{"jellyfin", ":8096", m.Jellyfin()},
		{"tautulli", ":8181", m.Tautulli()},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	var wg sync.WaitGroup
	for _, svc := range services {
		srv := &http.Server{Addr: svc.addr, Handler: svc.handler, ReadHeaderTimeout: 5 * time.Second}
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Info("mock listening", "service", svc.name, "addr", svc.addr, "fixtures", dir)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("mock server failed", "service", svc.name, "err", err)
				stop()
			}
		}()
		go func() {
			<-ctx.Done()
			sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sh)
		}()
	}
	wg.Wait()
}
