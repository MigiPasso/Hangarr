#!/bin/sh
# Writes the scenario of testdata/jellyfin/ into /media: 6-second videos and
# NFO files (local metadata, so the lab needs no internet access). Videos that
# exist already are kept, so the script can run again.
set -eu

ffmpeg=/usr/lib/jellyfin-ffmpeg/ffmpeg
media=/media

# video PATH: a 6-second test pattern, unless the file exists.
video() {
  if [ -e "$media/$1" ]; then
    return 0
  fi
  mkdir -p "$(dirname "$media/$1")"
  "$ffmpeg" -v error -nostdin -f lavfi -i testsrc2=size=320x180:rate=5 -t 6 \
    -c:v libx264 -preset ultrafast -pix_fmt yuv420p "$media/$1"
}

# video_av PATH: like video, with a French audio track and an English subtitle
# track, unless the file exists.
video_av() {
  if [ -e "$media/$1" ]; then
    return 0
  fi
  mkdir -p "$(dirname "$media/$1")"
  srt=/tmp/hangarr-lab-$$.srt
  printf '1\n00:00:00,000 --> 00:00:06,000\nLab subtitle\n\n' > "$srt"
  "$ffmpeg" -v error -nostdin -f lavfi -i testsrc2=size=640x360:rate=5 \
    -f lavfi -i "sine=frequency=440:duration=6" -i "$srt" -t 6 \
    -map 0:v -map 1:a -map 2:s \
    -c:v libx264 -preset ultrafast -pix_fmt yuv420p \
    -c:a aac -metadata:s:a:0 language=fre \
    -c:s srt -metadata:s:s:0 language=eng "$media/$1"
  rm -f "$srt"
}

# nfo PATH XML: the metadata file of a video.
nfo() {
  mkdir -p "$(dirname "$media/$1")"
  printf '%s\n' "$2" > "$media/$1"
}

# episode SHOW SEASON EPISODE [TITLE]: the video and NFO of one episode.
episode() {
  s=$(printf '%02d' "$2")
  e=$(printf '%02d' "$3")
  base="tv/$1/Season $s/$1 S${s}E${e}"
  video "$base.mkv"
  nfo "$base.nfo" "<episodedetails><title>${4:-Episode $3}</title><season>$2</season><episode>$3</episode></episodedetails>"
}

video 'movies/Inception (2010)/Inception (2010).mkv'
nfo 'movies/Inception (2010)/movie.nfo' '<movie><title>Inception</title><year>2010</year><uniqueid type="imdb" default="true">tt1375666</uniqueid><uniqueid type="tmdb">27205</uniqueid></movie>'
video 'movies/Dune (2021)/Dune (2021).mkv'
nfo 'movies/Dune (2021)/movie.nfo' '<movie><title>Dune</title><year>2021</year><uniqueid type="imdb" default="true">tt1160419</uniqueid><uniqueid type="tmdb">438631</uniqueid></movie>'
video_av 'movies/Dune (2021)/Dune (2021) - 2160p.mkv'
video 'movies/Tenet (2020) [copie]/Tenet (2020).mkv'
nfo 'movies/Tenet (2020) [copie]/movie.nfo' '<movie><title>Tenet</title><year>2020</year><uniqueid type="imdb" default="true">tt6723592</uniqueid><uniqueid type="tmdb">577922</uniqueid></movie>'
video 'movies/Tenet (2020)/Tenet (2020).mkv'
nfo 'movies/Tenet (2020)/movie.nfo' '<movie><title>Tenet</title><year>2020</year><uniqueid type="imdb" default="true">tt6723592</uniqueid><uniqueid type="tmdb">577922</uniqueid></movie>'
video 'movies/Interstellar (2014)/Interstellar (2014).mkv'
nfo 'movies/Interstellar (2014)/movie.nfo' '<movie><title>Interstellar</title><year>2014</year><uniqueid type="imdb" default="true">tt0816692</uniqueid></movie>'
video 'movies/Vacances 2019/Vacances 2019.mkv'

nfo 'tv/Breaking Bad/tvshow.nfo' '<tvshow><title>Breaking Bad</title><year>2008</year><uniqueid type="tvdb" default="true">81189</uniqueid><uniqueid type="tmdb">1396</uniqueid><uniqueid type="imdb">tt0903747</uniqueid></tvshow>'
episode 'Breaking Bad' 1 1 'Pilot'
episode 'Breaking Bad' 1 2 "Cat's in the Bag..."
episode 'Breaking Bad' 1 3 "...And the Bag's in the River"
episode 'Breaking Bad' 2 1 'Seven Thirty-Seven'
episode 'Breaking Bad' 2 2 'Grilled'
episode 'Breaking Bad' 2 3 'Bit by a Dead Bee'

nfo 'tv/The Expanse/tvshow.nfo' '<tvshow><title>The Expanse</title><uniqueid type="tvdb" default="true">280619</uniqueid><uniqueid type="tmdb">63639</uniqueid><uniqueid type="imdb">tt3230854</uniqueid></tvshow>'
episode 'The Expanse' 1 1 'Dulcinea'
episode 'The Expanse' 1 2 'The Big Empty'
episode 'The Expanse' 1 3 'Remember the Cant'
episode 'The Expanse' 2 1 'Safe'
episode 'The Expanse' 2 2
episode 'The Expanse' 2 3

nfo 'tv/Severance/tvshow.nfo' '<tvshow><title>Severance</title><uniqueid type="tvdb" default="true">371980</uniqueid></tvshow>'
episode 'Severance' 1 1
episode 'Severance' 1 2
episode 'Severance' 1 3

episode 'Shogun (2024)' 1 1 'Anjin'
episode 'Shogun (2024)' 1 2

episode 'Home Videos' 1 1 'Anniversaire'
episode 'Home Videos' 1 2
episode 'Home Videos' 1 3
episode 'Home Videos' 1 4
