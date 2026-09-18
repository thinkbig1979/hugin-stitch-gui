package main

import "testing"

func TestCleanDownloadName(t *testing.T) {
	for _, tc := range []struct{ in, ext, want string }{
		{"my panorama", ".tif", "my_panorama.tif"},
		{"beach.jpg", ".tif", "beach.tif"},                   // extension forced to the format
		{"  spaced  ", ".png", "spaced.png"},                 // trimmed before sanitising
		{"", ".tif", "stitch.tif"},                           // empty falls back
		{"../../etc/passwd", ".tif", ".._.._etc_passwd.tif"}, // separators neutralised
		{"a/b\\c", ".jpg", "a_b_c.jpg"},
		{"2024.05.31-dunes", ".tif", "2024.05.31-dunes.tif"}, // dots that are not an extension survive
	} {
		if got := cleanDownloadName(tc.in, tc.ext); got != tc.want {
			t.Errorf("cleanDownloadName(%q, %q) = %q, want %q", tc.in, tc.ext, got, tc.want)
		}
	}
}

func TestCleanDownloadNameTruncates(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	got := cleanDownloadName(long, ".tif")
	if len(got) != 80+len(".tif") {
		t.Errorf("name length = %d, want %d", len(got), 80+len(".tif"))
	}
}

func TestMakeDownloadNameFromCaptureTime(t *testing.T) {
	got := makeDownloadName("/tmp/job/DSC001.jpg", "2024:05:31 14:03:09", ".tif")
	if want := "20240531_140309_pano.tif"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMakeDownloadNameFallsBackToFirstFrame(t *testing.T) {
	for _, tc := range []struct{ path, dt, want string }{
		{"/tmp/job/DSC001.jpg", "", "DSC001_pano.tif"},
		{"/tmp/job/DSC001.jpg", "not a date", "DSC001_pano.tif"},
		{"", "", "stitch_pano.tif"},
		{"/tmp/job/my photo.jpg", "", "my_photo_pano.tif"},
	} {
		if got := makeDownloadName(tc.path, tc.dt, ".tif"); got != tc.want {
			t.Errorf("makeDownloadName(%q, %q) = %q, want %q", tc.path, tc.dt, got, tc.want)
		}
	}
}

func TestUploadName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		allowed bool
	}{
		{"plain jpeg", "DSC001.JPG", "DSC001.jpg", true}, // extension normalised to lower case
		{"strips unix path", "/etc/passwd.jpg", "passwd.jpg", true},
		{"strips windows path", `C:\Users\me\shot.jpg`, "shot.jpg", true},
		{"strips traversal", "../../evil.png", "evil.png", true},
		{"raw accepted", "IMG_0001.CR2", "IMG_0001.cr2", true},
		{"unknown extension rejected", "payload.exe", "", false},
		{"no extension defaults to png", "noext", "noext.png", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := uploadName(tc.in, 0, map[string]bool{})
			if ok != tc.allowed {
				t.Fatalf("accepted = %v, want %v", ok, tc.allowed)
			}
			if ok && got != tc.want {
				t.Errorf("uploadName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Two files with the same name must not overwrite each other in the job dir.
func TestUploadNameDeduplicates(t *testing.T) {
	taken := map[string]bool{}
	first, _ := uploadName("shot.jpg", 0, taken)
	second, _ := uploadName("shot.jpg", 1, taken)
	third, _ := uploadName("shot.jpg", 2, taken)
	if first == second || second == third || first == third {
		t.Errorf("names collided: %q, %q, %q", first, second, third)
	}
	if first != "shot.jpg" {
		t.Errorf("first name = %q, want the original", first)
	}
}

func TestParseQuality(t *testing.T) {
	for in, want := range map[string]int{
		"90": 90, "1": 1, "100": 100,
		"0": 1, "-5": 1, "500": 100, // clamped
		"": 90, "abc": 90, // default
		"87.6": 87, // a fractional quality truncates rather than rounding
	} {
		if got := parseQuality(in); got != want {
			t.Errorf("parseQuality(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestLookupFormat(t *testing.T) {
	key, format := lookupFormat("exr")
	if key != "exr" || format.Ext != ".exr" || !format.HDR {
		t.Errorf("lookupFormat(exr) = %q, %+v", key, format)
	}
	if key, format := lookupFormat("bogus"); key != DefaultFormat || format.Ext != ".tif" {
		t.Errorf("unknown format should fall back to TIFF, got %q", key)
	}
	if _, format := lookupFormat("jpg"); !format.Quality {
		t.Error("JPEG should honour the quality slider")
	}
	if _, format := lookupFormat("tif_hdr"); format.EXIF {
		t.Error("HDR TIFF should not attempt EXIF transfer")
	}
}
