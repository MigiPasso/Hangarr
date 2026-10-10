#!/bin/sh
# Prepares the Jellyfin lab: startup wizard, libraries, users,
# playback state of testdata/jellyfin/ (without audio) and an API key for
# hangarr, written to /out/api-key. Safe to run again.
set -eu

url=${JELLYFIN_URL:-http://jellyfin:8096}
device='Client="hangarr-lab", Device="seed", DeviceId="hangarr-lab-seed", Version="1.0"'
auth="MediaBrowser $device"
admin_password=hangarr-lab-admin
user_password=hangarr-lab-user
fail() { echo "seed: $*" >&2; exit 1; }

apk add -q curl jq

# api METHOD PATH [JSON]: prints the response body, fails on HTTP errors.
api() {
  if [ $# -gt 2 ]; then
    curl -fsS -X "$1" "$url$2" -H "Authorization: $auth" -H 'Content-Type: application/json' --data-raw "$3"
  else
    curl -fsS -X "$1" "$url$2" -H "Authorization: $auth"
  fi
}

# Startup wizard (first start only).
if [ "$(api GET /System/Info/Public | jq -r .StartupWizardCompleted)" = false ]; then
  api POST /Startup/Configuration '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}'
  api GET /Startup/User > /dev/null
  api POST /Startup/User "{\"Name\":\"admin\",\"Password\":\"$admin_password\"}"
  api POST /Startup/RemoteAccess '{"EnableRemoteAccess":true,"EnableAutomaticPortMapping":false}'
  api POST /Startup/Complete
fi

login=$(api POST /Users/AuthenticateByName "{\"Username\":\"admin\",\"Pw\":\"$admin_password\"}")
token=$(printf '%s' "$login" | jq -r .AccessToken)
[ -n "$token" ] && [ "$token" != null ] || fail "admin login failed"
auth="MediaBrowser $device, Token=\"$token\""

# Local metadata (NFO) only, no internet providers: the result is deterministic.
options='{"LibraryOptions":{"EnableInternetProviders":false,"TypeOptions":[{"Type":"Movie","MetadataFetchers":[],"ImageFetchers":[]},{"Type":"Series","MetadataFetchers":[],"ImageFetchers":[]},{"Type":"Season","MetadataFetchers":[],"ImageFetchers":[]},{"Type":"Episode","MetadataFetchers":[],"ImageFetchers":[]}]}}'
folders=$(api GET /Library/VirtualFolders)
# library NAME COLLECTION PATH: created unless a library has that name.
library() {
  if printf '%s' "$folders" | jq -e --arg n "$1" 'any(.[]; .Name == $n)' > /dev/null; then
    return 0
  fi
  api POST "/Library/VirtualFolders?name=$1&collectionType=$2&paths=$3&refreshLibrary=false" "$options" > /dev/null
}
library Movies movies /media/movies
library Shows tvshows /media/tv
api POST /Library/Refresh > /dev/null

# total TYPE: number of items of one type in the libraries.
total() {
  api GET "/Items?recursive=true&includeItemTypes=$1&limit=1" | jq -r .TotalRecordCount
}
i=0
until [ "$(total Movie)" = 5 ] && [ "$(total Series)" = 5 ] && [ "$(total Episode)" = 21 ]; do
  i=$((i + 1))
  [ "$i" -le 60 ] || fail "library scan incomplete after 5 minutes"
  sleep 5
done

# user_id NAME: the id of an account, empty if there is none.
user_id() {
  api GET /Users | jq -r --arg n "$1" '.[] | select(.Name == $n) | .Id'
}
for name in alice bob carol; do
  if [ -z "$(user_id "$name")" ]; then
    api POST /Users/New "{\"Name\":\"$name\",\"Password\":\"$user_password\"}" > /dev/null
  fi
done
carol=$(user_id carol)
policy=$(api GET "/Users/$carol" | jq -c .Policy)
if [ "$(printf '%s' "$policy" | jq .IsDisabled)" != true ]; then
  api POST "/Users/$carol/Policy" "$(printf '%s' "$policy" | jq -c '.IsDisabled = true')" > /dev/null
fi

# Items of the scenario, with their ids and runtimes.
items=$(api GET '/Items?recursive=true&includeItemTypes=Movie,Episode&fields=Path')
movie() { printf '%s' "$items" | jq -c --arg n "$1" '.Items[] | select(.Type == "Movie" and .Name == $n)'; }
# episode SERIES SEASON EPISODE: the JSON of one episode.
episode() {
  printf '%s' "$items" | jq -c --arg s "$1" --argjson n "$2" --argjson e "$3" \
    '.Items[] | select(.Type == "Episode" and .SeriesName == $s and .ParentIndexNumber == $n and .IndexNumber == $e)'
}

# played USER ITEM DATE: marks an item as played on DATE.
played() {
  [ -n "$2" ] || fail "item not found for $1"
  id=$(printf '%s' "$2" | jq -r .Id)
  api POST "/UserPlayedItems/$id?userId=$(user_id "$1")&datePlayed=$3" > /dev/null
}

# resumed USER ITEM DATE PERCENT: an unfinished item, PERCENT of its runtime
# (the videos last 6 seconds, so the position comes from the lab runtime).
resumed() {
  [ -n "$2" ] || fail "item not found for $1"
  id=$(printf '%s' "$2" | jq -r .Id)
  ticks=$(printf '%s' "$2" | jq -r .RunTimeTicks)
  body="{\"PlaybackPositionTicks\":$((ticks * $4 / 100)),\"LastPlayedDate\":\"$3\",\"Played\":false}"
  api POST "/UserItems/$id/UserData?userId=$(user_id "$1")" "$body" > /dev/null
}

# Playback state of testdata/jellyfin/played-* and resumable-*, written once: a
# second run would count the plays again.
if [ "$(api GET "/Items?userId=$(user_id alice)&recursive=true&includeItemTypes=Movie,Episode&isPlayed=true&limit=1" | jq -r .TotalRecordCount)" = 0 ]; then
  played alice "$(episode 'Breaking Bad' 2 3)" 2026-07-03T20:47:00Z
  played alice "$(episode 'Breaking Bad' 2 2)" 2026-07-02T20:47:00Z
  played alice "$(episode 'Breaking Bad' 2 1)" 2026-07-01T20:47:00Z
  played alice "$(episode 'Breaking Bad' 1 3)" 2026-06-28T20:47:00Z
  played alice "$(episode 'Breaking Bad' 1 2)" 2026-06-27T21:47:00Z
  played alice "$(episode 'Breaking Bad' 1 1)" 2026-06-23T20:47:00Z
  played alice "$(episode 'The Expanse' 1 3)" 2026-05-04T20:50:00Z
  played alice "$(episode 'The Expanse' 1 2)" 2026-05-03T21:50:00Z
  played alice "$(episode 'The Expanse' 1 1)" 2026-05-03T20:50:00Z
  played alice "$(movie Inception)" 2025-08-27T22:26:40Z
  played bob "$(episode 'Shogun' 1 1)" 2026-09-26T21:00:00Z
  played bob "$(episode 'Breaking Bad' 1 1)" 2026-09-21T20:46:40Z
  played bob "$(episode 'Home Videos' 1 1)" 2026-09-11T20:10:00Z
  played bob "$(movie Inception)" 2026-09-01T22:28:00Z
  resumed alice "$(movie Dune)" 2026-09-29T20:45:00Z 45
  resumed alice "$(episode 'The Expanse' 2 1)" 2026-05-14T20:20:00Z 40
  resumed alice "$(movie Interstellar)" 2026-03-15T20:50:00Z 30
  resumed bob "$(movie Tenet)" 2026-09-30T01:00:00Z 7

  # bob's "Cat's in the Bag..." is played without a date.
  id=$(episode 'Breaking Bad' 1 2 | jq -r .Id)
  api POST "/UserItems/$id/UserData?userId=$(user_id bob)" '{"Played":true}' > /dev/null
fi

# API key for hangarr: created once, always written to the out volume.
keys=$(api GET /Auth/Keys)
hangarr_key() { printf '%s' "$1" | jq -r 'first(.Items[]? | select(.AppName == "hangarr") | .AccessToken) // empty'; }
if [ -z "$(hangarr_key "$keys")" ]; then
  api POST '/Auth/Keys?app=hangarr' > /dev/null
  keys=$(api GET /Auth/Keys)
fi
key=$(hangarr_key "$keys")
[ -n "$key" ] || fail "API key not created"
(umask 077 && printf '%s\n' "$key" > /out/api-key)

echo "Jellyfin lab ready: http://localhost:${LAB_PORT:-8096} (admin / $admin_password)"
echo "API key written to the out volume"
