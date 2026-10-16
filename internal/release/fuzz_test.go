package release

import "testing"

func FuzzParse(f *testing.F) {
	for _, s := range []string{"Dune.2021.MULTi.2160p.UHD.BluRay.REMUX.HDR.HEVC.TrueHD.7.1.Atmos-GRP.mkv", "", "...", "x264"} {
		f.Add(s)
	}
	f.Fuzz(func(_ *testing.T, s string) { _ = Parse(s) })
}
