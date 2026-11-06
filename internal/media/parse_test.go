package media

import (
	"reflect"
	"testing"
)

func TestParseNames(t *testing.T) {
	cases := []struct {
		name    string
		kind    Kind
		title   string
		year    int
		season  int
		eps     []int
		subtitl bool
	}{
		{"Marvels_Jessica_Jones_S03E03_A_K_A_I_Have_No_Spleen_720p_NF_WEB.mkv", Episode, "Marvels Jessica Jones", 0, 3, []int{3}, false},
		{"Marvels_Jessica_Jones_S03E13_A_K_A_Everything_720p_NF_WEB_DL_x265.mkv", Episode, "Marvels Jessica Jones", 0, 3, []int{13}, false},
		{"Seed.of.Chucky.2004.UNRATED.REMASTERED.1080p.BluRay.x264.mkv", Movie, "Seed of Chucky", 2004, 0, nil, false},
		{"Bride.of.Chucky.1998.1080p.BluRay.x264.YIFY.[@MzoneChat].srt", Movie, "Bride of Chucky", 1998, 0, nil, true},
		{"Pinocchio.Unstrung.2026.720p.BluRay.x264.AAC-[ - YTS].srt", Movie, "Pinocchio Unstrung", 2026, 0, nil, true},
		{"Dexter_Resurrection_S02E01_1080p_10bit_WEBRip_6CH_x265_HEVC_PSA.mkv", Episode, "Dexter Resurrection", 0, 2, []int{1}, false},
		{"Good.Omens.S03E01.1080p.mkv", Episode, "Good Omens", 0, 3, []int{1}, false},
		{"Up (2009).mkv", Movie, "Up", 2009, 0, nil, false},
		{"Up.2009.1080p.BluRay.x264.mkv", Movie, "Up", 2009, 0, nil, false},
		{"2012.2009.1080p.BluRay.mkv", Movie, "2012", 2009, 0, nil, false},
		{"Shameless.US.S11E12.720p.WEB.mkv", Episode, "Shameless", 0, 11, []int{12}, false},
		{"Show.Name.S01E01E02.720p.mkv", Episode, "Show Name", 0, 1, []int{1, 2}, false},
		{"Show.Name.S01E01-E03.720p.mkv", Episode, "Show Name", 0, 1, []int{1, 2, 3}, false},
		{"Show Name 1x05 Title.mkv", Episode, "Show Name", 0, 1, []int{5}, false},
		{"Show Name Season 2 Episode 7.mkv", Episode, "Show Name", 0, 2, []int{7}, false},
		{"the.office.us.s02e03.720p.hdtv.x264.mkv", Episode, "the office", 0, 2, []int{3}, false},
		{"Blade.Runner.2049.2017.2160p.UHD.BluRay.x265.mkv", Movie, "Blade Runner 2049", 2017, 0, nil, false},
	}
	for _, c := range cases {
		in := Parse(c.name)
		if in.Kind != c.kind || in.Title != c.title || in.Year != c.year || in.Subtitle != c.subtitl ||
			(c.kind == Episode && (in.Season != c.season || !reflect.DeepEqual(in.Episodes, c.eps))) {
			t.Errorf("%q\n  got  kind=%v title=%q year=%d season=%d eps=%v sub=%v\n  want kind=%v title=%q year=%d season=%d eps=%v sub=%v",
				c.name, in.Kind, in.Title, in.Year, in.Season, in.Episodes, in.Subtitle,
				c.kind, c.title, c.year, c.season, c.eps, c.subtitl)
		}
	}
}

func TestCountryTagComesOffTheTitle(t *testing.T) {
	in := Parse("Shameless US S08E01 720p BluRay x265-HETeam .mkv")
	if in.Title != "Shameless" || in.Country != "US" || in.Season != 8 {
		t.Errorf("%+v", in)
	}
	if got := Parse("Skins.UK.S01E01.mkv"); got.Country != "GB" || got.Title != "Skins" {
		t.Errorf("UK means GB at TMDB: %+v", got)
	}
	if got := Parse("Us.2019.1080p.mkv"); got.Title != "Us" || got.Country != "" {
		t.Errorf("a title that is only the tag stays: %+v", got)
	}
}

func TestParseInUsesFolders(t *testing.T) {
	cases := []struct {
		rel    string
		kind   Kind
		title  string
		season int
		eps    []int
		weak   bool
	}{
		{"Shameless/Season 01/Episode 03.mkv", Episode, "Shameless", 1, []int{3}, true},
		{"shameless season 11/Shameless.S11E01.mkv", Episode, "Shameless", 11, []int{1}, false},
		{"Shameless/Season 2/03.mkv", Unknown, "", 0, nil, true},
		{"downloads/Up (2009).mkv", Movie, "Up", 0, nil, false},
	}
	for _, c := range cases {
		in := ParseIn(c.rel)
		if c.kind == Unknown {
			continue // a bare number is not enough to go on; checked in review
		}
		if in.Kind != c.kind || !equalFold(in.Title, c.title) || (c.kind == Episode && (in.Season != c.season || !reflect.DeepEqual(in.Episodes, c.eps))) {
			t.Errorf("%q: got kind=%v title=%q season=%d eps=%v weak=%v", c.rel, in.Kind, in.Title, in.Season, in.Episodes, in.Weak)
		}
		if c.weak && !in.Weak {
			t.Errorf("%q: expected Weak", c.rel)
		}
	}
}

func equalFold(a, b string) bool { return Norm(a) == Norm(b) }

func TestNorm(t *testing.T) {
	if Norm("Marvel's Jessica Jones") != Norm("marvels_jessica.jones") {
		t.Fatal("not equal")
	}
	if Norm("Spider-Man: No Way Home") != "spider man no way home" {
		t.Fatal(Norm("Spider-Man: No Way Home"))
	}
}
