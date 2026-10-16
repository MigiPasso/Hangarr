package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"hangarr/internal/config"
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

func TestAnonymizerPlexFile(t *testing.T) {
	var doc any
	if err := json.Unmarshal([]byte(`{"Media":[{"Part":[{"file":"/home/alice/films/Dune (2021)/Dune.2021.2160p.mkv"}]}]}`), &doc); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(newAnonymizer().walk(doc, false))
	s := string(out)
	if strings.Contains(s, "alice") || !strings.Contains(s, `"file":"/media/Dune.2021.2160p.mkv"`) {
		t.Errorf("Plex part file: %s", s)
	}
}

func TestAnonymizerJellyfin(t *testing.T) {
	var users any
	_ = json.Unmarshal([]byte(`[{"Name":"alice","Id":"aaaa","ServerId":"5e0c","Policy":{"IsDisabled":false}},{"Name":"bob","Id":"bbbb"}]`), &users)
	out, _ := json.Marshal(newAnonymizer().walk(users, true))
	s := string(out)
	if strings.Contains(s, "alice") || strings.Contains(s, "bob") || !strings.Contains(s, `"Name":"user1"`) || !strings.Contains(s, `"Name":"user2"`) {
		t.Errorf("Jellyfin accounts: %s", s)
	}
	var info any
	_ = json.Unmarshal([]byte(`{"ServerName":"my-home","Version":"12.0.1","LocalAddress":"http://192.168.1.20:8096"}`), &info)
	out, _ = json.Marshal(newAnonymizer().walk(info, false))
	s = string(out)
	if strings.Contains(s, "my-home") || strings.Contains(s, "192.168") || !strings.Contains(s, `"ServerName":"redacted"`) ||
		!strings.Contains(s, `"Version":"12.0.1"`) {
		t.Errorf("Jellyfin system info: %s", s)
	}
}

func TestNothingConfigured(t *testing.T) {
	dir := t.TempDir()
	if err := captureAll(&config.Config{}, dir, 30); err == nil {
		t.Fatal("expected an error when no service is configured")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("files written without any configured service: %v", entries)
	}
}

// SEC-15: credentials are masked whatever the field, and Plex identifiers
// are kept.
func TestAnonymizerSecrets(t *testing.T) {
	in := `{"data":{"server_token":"tok-1","pms_token":"tok-2","apiKey":"k-3","api_key":"k-4","password":"p-5",
		"webhook_secret":"s-6","ratingKey":"123","key":"/library/metadata/123","parentKey":"/library/metadata/12"}}`
	var doc any
	if err := json.Unmarshal([]byte(in), &doc); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(newAnonymizer().walk(doc, false))
	s := string(out)
	for _, leak := range []string{"tok-1", "tok-2", "k-3", "k-4", "p-5", "s-6"} {
		if strings.Contains(s, leak) {
			t.Errorf("leak %q in %s", leak, s)
		}
	}
	for _, keep := range []string{`"ratingKey":"123"`, `"key":"/library/metadata/123"`, `"parentKey":"/library/metadata/12"`} {
		if !strings.Contains(s, keep) {
			t.Errorf("missing %s in %s", keep, s)
		}
	}
	for key, ok := range map[string]bool{"123": true, "abc9": true, "../x": false, "1/2": false, "": false} {
		if safeKey.MatchString(key) != ok {
			t.Errorf("safeKey(%q) != %v", key, ok)
		}
	}
}
