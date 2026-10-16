package tautulli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCall(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") != "k" {
			_, _ = w.Write([]byte(`{"response":{"result":"error","message":"Invalid apikey","data":{}}}`))
			return
		}
		switch r.URL.Query().Get("cmd") {
		case "get_history":
			if r.URL.Query().Get("start") != "10" || r.URL.Query().Get("length") != "5" {
				t.Errorf("paging: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"response":{"result":"success","data":{"recordsFiltered":1,"data":[
				{"id":7,"reference_id":7,"date":1790000000,"user_id":3,"user":"bob","watched_status":1,"duration":"3600"}]}}}`))
		case "get_tautulli_info":
			_, _ = w.Write([]byte(`{"response":{"result":"success","data":{"tautulli_version":"v2.15.0"}}}`))
		}
	}))
	defer ts.Close()
	c := New(ts.URL, "k")
	p, err := c.History(context.Background(), 10, 5)
	if err != nil || len(p.Rows) != 1 || !p.Rows[0].Watched() || int64(p.Rows[0].ID) != 7 {
		t.Fatalf("history: %+v %v", p, err)
	}
	if v, err := c.Info(context.Background()); err != nil || v != "v2.15.0" {
		t.Errorf("info: %q %v", v, err)
	}
	_, err = New(ts.URL, "wrong").Info(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Message != "Invalid apikey" || strings.Contains(err.Error(), "wrong") {
		t.Errorf("api error: %v", err)
	}
}
