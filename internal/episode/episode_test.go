package episode

import "testing"

func TestIsVideo(t *testing.T) {
	cases := map[string]bool{
		"foo.mkv":     true,
		"foo.MP4":     true,
		"foo.ts":      true,
		"foo.flv":     true,
		"foo.webm":    true,
		"index.m3u8":  true,
		"cover.jpg":   false,
		"OST 01.flac": false,
		"sub.ass":     false,
		"movie.nfo":   false,
		"poster.png":  false,
		"sub.srt":     false,
		"noextension": false,
		"archive.zip": false,
		"sample.mp4":  true,
		"特典映像.MKV":    true,
	}
	for name, want := range cases {
		if got := IsVideo(name); got != want {
			t.Errorf("IsVideo(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		file string
		num  float64
		ok   bool
	}{
		{"[Lilith-Raws] Bocchi the Rock! - 05 [Baha][WEB-DL][1080p][AVC AAC][CHT][MP4].mp4", 5, true},
		{"[SubsPlease] Sousou no Frieren - 28 (1080p) [ABCDEF12].mkv", 28, true},
		{"[ANi] 葬送的芙莉蓮 - 01 [1080P][Baha][WEB-DL][AAC AVC][CHT].mp4", 1, true},
		{"Frieren.S01E12.1080p.WEB-DL.x264.mkv", 12, true},
		{"番剧 第03话 1080p.mp4", 3, true},
		{"第 7 集.mkv", 7, true},
		{"[VCB-Studio] K-ON! [01][Ma10p_1080p][x265_flac].mkv", 1, true},
		{"EP05.mkv", 5, true},
		{"ep.12 v2.mp4", 12, true},
		{"#08 タイトル.mkv", 8, true},
		{"[Moozzi2] Steins;Gate - 11.5 (BD 1920x1080 x264 FLAC).mkv", 11.5, true},
		{"Mushoku Tensei (2021) - 09 [1080p].mkv", 9, true},
		{"[Sub] Show 2nd Season - 13v2 [720p].mkv", 13, true},
		// 解析不到集数的
		{"NCOP.mkv", 0, false},
		{"Menu.mkv", 0, false},
		{"映像特典 PV.mkv", 0, false},
	}
	for _, c := range cases {
		num, ok := Parse(c.file)
		if ok != c.ok || (ok && num != c.num) {
			t.Errorf("Parse(%q) = (%v, %v), want (%v, %v)", c.file, num, ok, c.num, c.ok)
		}
	}
}

func TestFormatEpisodeNumber(t *testing.T) {
	if got := FormatEpisodeNumber(5); got != "第 5 话" {
		t.Errorf("got %q", got)
	}
	if got := FormatEpisodeNumber(11.5); got != "第 11.5 话" {
		t.Errorf("got %q", got)
	}
}

func TestCleanDirName(t *testing.T) {
	cases := []struct {
		dir  string
		want string
	}{
		{"[Lilith-Raws] Bocchi the Rock! [Baha][1080p]", "Bocchi the Rock!"},
		{"[VCB-Studio] Sousou no Frieren [Ma10p_1080p]", "Sousou no Frieren"},
		{"孤独摇滚", "孤独摇滚"},
		{"[ANi] 葬送的芙莉蓮 [1080P][Baha][WEB-DL][AAC AVC][CHT]", "葬送的芙莉蓮"},
	}
	for _, c := range cases {
		if got := CleanDirName(c.dir); got != c.want {
			t.Errorf("CleanDirName(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}
