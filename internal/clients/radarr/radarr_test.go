package radarr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRatings(t *testing.T) {
	for in, want := range map[string]float64{
		`{"imdb":{"value":8.1},"tmdb":{"value":7.5}}`: 8.1,
		`{"tmdb":{"value":"7.5"}}`:                    7.5,
		`{"value":6.2}`:                               6.2,
		`[]`:                                          0,
		`{}`:                                          0,
	} {
		var r Ratings
		if err := json.Unmarshal([]byte(in), &r); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if r.Best() != want {
			t.Errorf("%s: %v, want %v", in, r.Best(), want)
		}
	}
}

func TestDeleteMovieAndRawRoundTrip(t *testing.T) {
	var query string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			query = r.URL.RawQuery
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"id":4,"title":"Interstellar","tags":[2],"custom":{"kept":true}}`))
		}
	}))
	defer ts.Close()
	c := New(ts.URL, "k")
	if err := c.DeleteMovie(context.Background(), 4, true, true); err != nil {
		t.Fatal(err)
	}
	if query != "addImportExclusion=true&deleteFiles=true" {
		t.Errorf("query %q", query)
	}
	m, raw, err := c.MovieByIDRaw(context.Background(), 4)
	if err != nil || m.Title != "Interstellar" || len(m.Tags) != 1 || !json.Valid(raw) || string(raw) == "" {
		t.Fatalf("%+v %s %v", m, raw, err)
	}
}

func FuzzRatings(f *testing.F) {
	f.Add([]byte(`{"imdb":{"value":8.1}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		var r Ratings
		_ = r.UnmarshalJSON(b) // tolerant: never fails, never panics
		_ = r.Best()
	})
}
