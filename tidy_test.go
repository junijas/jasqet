package main

import (
	"path/filepath"
	"testing"
)

func TestTidyVideoNames(t *testing.T) {
	for _, c := range []struct{ in, want, series string }{
		// films
		{"[YTS.MX] Some.Movie.2021.1080p.WEBRip.x264.AAC5.1.mp4", "Some Movie (2021).mp4", ""},
		{"Avengers.Endgame.2019.1080p.BluRay.x264-[YTS.LT].mp4", "Avengers Endgame (2019).mp4", ""},
		{"Movie_Name_2019_BluRay_1080p.mkv", "Movie Name (2019).mkv", ""},
		{"1917.2019.1080p.WEB-DL.H.264.mkv", "1917 (2019).mkv", ""},
		{"Some Movie (2021) [1080p] [YTS.MX].mp4", "Some Movie (2021).mp4", ""},
		{"the.lost.city.2022.720p.hdrip.x265-GRP.mkv", "The Lost City (2022).mkv", ""},
		{"Movie.Name.2021.mp4", "Movie Name (2021).mp4", ""},
		{"Old.Classic.1080p.BluRay.x264.mkv", "Old Classic.mkv", ""},
		// series
		{"The.Office.US.S02E05.720p.WEB-DL.x264-GROUP.mkv", "The Office US - S02E05.mkv", "The Office US"},
		{"show.name.s01e02.the.pilot.1080p.mkv", "Show Name - S01E02 - The Pilot.mkv", "Show Name"},
		{"Show Name S3E7 HDTV x264.mp4", "Show Name - S03E07.mp4", "Show Name"},
		// already tidy: unchanged, and the same folder again
		{"Some Movie (2021).mp4", "Some Movie (2021).mp4", ""},
		{"The Office US - S02E05.mkv", "The Office US - S02E05.mkv", "The Office US"},
		// not releases: left alone
		{"My trip to the Cam.mp4", "My trip to the Cam.mp4", ""},
		{"Meeting.2024.03.05.mp4", "Meeting.2024.03.05.mp4", ""},
		{"Lecture_03_Physics.mp4", "Lecture_03_Physics.mp4", ""},
		{"Holiday 2019.mp4", "Holiday 2019.mp4", ""},
		{"Some.Video.mp4", "Some.Video.mp4", ""},
		{"report.2021.1080p.pdf", "report.2021.1080p.pdf", ""}, // only videos
		{"1080p.x264.mp4", "1080p.x264.mp4", ""},               // nothing left to call it
	} {
		got, series := tidyVideoName(c.in)
		if got != c.want || series != c.series {
			t.Errorf("%q → %q (series %q), want %q (series %q)", c.in, got, series, c.want, c.series)
		}
	}
}

func TestTidyNamesWhereTheFileGoes(t *testing.T) {
	m := newTestManager(t)
	_, s := m.Snapshot()
	s.Categorize, s.Tidy = true, true
	m.SetSettings(s)
	d := &Download{ID: "a", FileName: "The.Office.US.S02E05.720p.WEB-DL.x264-GROUP.mkv"}
	m.placeFile(d)
	if d.FileName != "The Office US - S02E05.mkv" || d.Dir != filepath.Join(s.DownloadDir, "Video", "The Office US") {
		t.Fatalf("placed as %q in %q", d.FileName, d.Dir)
	}
	// a folder you chose, a video stream, or the setting off: no change of folder or name
	chosen := &Download{ID: "b", FileName: "Show.S01E01.720p.mkv", Dir: s.DownloadDir, DirChosen: true}
	m.placeFile(chosen)
	if chosen.Dir != s.DownloadDir || chosen.FileName != "Show - S01E01.mkv" {
		t.Fatalf("chosen folder: %q in %q", chosen.FileName, chosen.Dir)
	}
	s.Tidy = false
	m.SetSettings(s)
	off := &Download{ID: "c", FileName: "Some.Movie.2021.1080p.WEBRip.x264.mp4"}
	m.placeFile(off)
	if off.FileName != "Some.Movie.2021.1080p.WEBRip.x264.mp4" {
		t.Fatalf("setting off, still renamed: %q", off.FileName)
	}
}
