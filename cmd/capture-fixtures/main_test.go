package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnonymizer(t *testing.T) {
	in := `{"response":{"data":{"data":[
		{"user":"alice","friendly_name":"Alice Martin","ip_address":"82.64.1.2","title":"Dune","path":"/home/alice/movies/Dune (2021)"},
		{"user":"alice","ip_address":"82.64.1.2","email":"alice@gmail.com","user_thumb":"https://plex.tv/users/x/avatar","machine_id":"abc"}
	]}}}`
	var doc any
	if err := json.Unmarshal([]byte(in), &doc); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(newAnonymizer().walk(doc, false))
	s := string(out)
	for _, leak := range []string{"alice", "Alice", "82.64", "gmail", "plex.tv", "/home", `"abc"`} {
		if strings.Contains(s, leak) {
			t.Errorf("leak %q in %s", leak, s)
		}
	}
	// Keys are sorted: friendly_name ("Alice Martin") gets user1, user ("alice") user2.
	for _, keep := range []string{`"title":"Dune"`, `"path":"/media/Dune (2021)"`, `"friendly_name":"user1"`, `"user":"user2"`, `"ip_address":"192.0.2.1"`} {
		if !strings.Contains(s, keep) {
			t.Errorf("missing %s in %s", keep, s)
		}
	}
	if strings.Count(s, `"user":"user2"`) != 2 {
		t.Errorf("pseudonyms must be stable: %s", s)
	}
}

func TestAnonymizerAccounts(t *testing.T) {
	var doc any
	_ = json.Unmarshal([]byte(`{"MediaContainer":{"Account":[{"id":1,"name":"bob"}],"title":"keep"}}`), &doc)
	out, _ := json.Marshal(newAnonymizer().walk(doc, true))
	if strings.Contains(string(out), "bob") || !strings.Contains(string(out), "keep") {
		t.Errorf("accounts: %s", out)
	}
}
