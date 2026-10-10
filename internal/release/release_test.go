package release

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		name   string
		source Source
		light  bool
	}{
		{"/media/Harry Potter and the Prisoner of Azkaban (2004) Remux-2160p.mkv", Remux, false},
		{"Rocky (1976) Bluray-2160p.mkv", BluRay, false},
		{"Rocky (1976) MULTi VFI 2160p 10bit 4KLight DV HDR BluRay DTS-HD MA 5.1 x265-QTZ.mkv", BluRay, true},
		{"Dune 2021 VFF VO DOLBY TRUEHD ATMOS 2160P UHD BLURAY REMUX HEVC HDR10 DOLBY VISION-Obi.mkv", Remux, false},
		{"UFC.284.Makhachev.vs.Volkanovski.11.02.2023.FRENCH.1080p.HDTV.H264-R3WiND.mkv", HDTV, false},
		{"Movie.2020.1080p.WEB-DL.DDP5.1.H.264-GRP.mkv", WEBDL, false},
		{"Movie (2020) WEBDL-1080p.mkv", WEBDL, false},
		{"Movie.2020.1080p.WEBRip.x264-GRP.mkv", WEBRip, false},
		{"Movie.2020.1080p.WEB.Rip.x264.mkv", WEBRip, false},
		{"Movie.2020.2160p.WEB.H265-GRP.mkv", WEB, false},
		{"Movie.2020.MULTi.1080p.HDLight.x264.mkv", Unknown, true},
		{"Movie.2001.DVDRip.XviD.avi", DVD, false},
		{"Movie.2010.1080p.BDRip.x264.mkv", BluRay, false},
		{`C:\films\Movie.2010.Blu-ray.1080p.mkv`, BluRay, false},
		{"FiNAL.PSG vs Inter Milan.mkv", Unknown, false},
		{"Blurayzzz.Remuxed.mkv", Unknown, false},
		{"", Unknown, false},
		// Accepted false positive: a title word "Web" alone reads as WEB.
		{"The Web (2019).mkv", WEB, false},

		// Radarr naming and priority between sources.
		{"Movie (2020) Remux-2160p.mkv", Remux, false},
		{"Movie (2020) WEBRip-720p.mkv", WEBRip, false},
		{"Movie (2020) HDTV-720p.mkv", HDTV, false},
		{"Movie.2020.BLURAY.REMUX.mkv", Remux, false},
		{"Movie.2020.DVD.HDTV.mkv", HDTV, false},
		{"Movie.2020.WEB-DL.WEBRip.mkv", WEBDL, false},

		// Other spellings.
		{"Movie.2020.BDRemux.mkv", Remux, false},
		{"Movie.2020.WEBDL.mkv", WEBDL, false},
		{"Movie.2020.PDTV.mkv", HDTV, false},
		{"Movie.2020.DVD.Rip.mkv", DVD, false},
		{"Movie.2020.1080p.BluRay", BluRay, false},

		// Light releases.
		{"Movie.2020.1080pLight.x264.mkv", Unknown, true},
		{"Movie.2020.mHD.mkv", Unknown, true},
	}
	for _, c := range cases {
		got := Parse(c.name)
		if got.Source != c.source || got.Light != c.light {
			t.Errorf("Parse(%q) = %+v, want Source %q, Light %v", c.name, got, c.source, c.light)
		}
	}
}
