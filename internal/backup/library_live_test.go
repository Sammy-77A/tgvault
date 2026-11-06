package backup

import (
	"context"
	"os"
	"testing"

	"github.com/Sammy-77A/tgvault/internal/media"
	"github.com/Sammy-77A/tgvault/internal/tmdb"
)

// Runs the organizer's matching against the real TMDB. Needs TMDB_KEY, and is
// skipped without it: go test -run Live ./internal/backup
func TestLiveTMDBMatches(t *testing.T) {
	key := os.Getenv("TMDB_KEY")
	if key == "" {
		t.Skip("TMDB_KEY not set")
	}
	e, _, _, _, _ := moveRig(t)
	e.SetMovieDB(&tmdb.Client{Key: key})
	lk := newLookup(e.movieDB)
	root := e.MediaRoot()
	cases := []struct{ file, want string }{
		{"Marvels_Jessica_Jones_S03E03_A_K_A_I_Have_No_Spleen_720p_NF_WEB.mkv", root + "/Shows/Marvel's Jessica Jones/Season 03/Marvel's Jessica Jones - S03E03.mkv"},
		{"Seed.of.Chucky.2004.UNRATED.REMASTERED.1080p.BluRay.x264.mkv", root + "/Movies/Seed of Chucky (2004)/Seed of Chucky (2004).mkv"},
		{"Bride.of.Chucky.1998.1080p.BluRay.x264.YIFY.[@MzoneChat].srt", root + "/Movies/Bride of Chucky (1998)/Bride of Chucky (1998).srt"},
		{"Dexter_Resurrection_S02E01_1080p_10bit_WEBRip_6CH_x265_HEVC_PSA.mkv", ""},
		{"Good.Omens.S03E01.1080p.mkv", ""},
		{"Up (2009).mkv", root + "/Movies/Up (2009)/Up (2009).mkv"},
	}
	for _, c := range cases {
		d := e.decide(context.Background(), lk, root+"/"+c.file)
		t.Logf("%-70s -> to=%q reason=%q read=%s", c.file, d.to, d.reason, media.ParseIn(c.file).Title)
		if d.retry != nil {
			t.Errorf("%s: %v", c.file, d.retry)
		}
		if c.want != "" && d.to != c.want {
			t.Errorf("%s:\n got  %q (%s)\n want %q", c.file, d.to, d.reason, c.want)
		}
	}
}
