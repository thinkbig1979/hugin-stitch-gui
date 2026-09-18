package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// unsafeName matches everything we refuse to keep in a filename. Uploads name
// files on disk and downloads name them in a Content-Disposition header, so
// both go through here.
var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// exifDateTime matches ExifTool's DateTimeOriginal format, "2024:05:31 14:03:09".
var exifDateTime = regexp.MustCompile(`^(\d{4}):(\d{2}):(\d{2}) (\d{2}):(\d{2}):(\d{2})`)

// sanitise strips path separators and anything non-portable from a name, then
// truncates it. An empty result falls back to fallback.
func sanitise(name string, limit int, fallback string) string {
	safe := unsafeName.ReplaceAllString(strings.TrimSpace(name), "_")
	if len(safe) > limit {
		safe = safe[:limit]
	}
	if safe == "" {
		return fallback
	}
	return safe
}

// cleanDownloadName sanitises a user-supplied filename and forces the correct
// extension for the chosen output format.
func cleanDownloadName(name, ext string) string {
	safe := sanitise(name, 80, "stitch")
	return strings.TrimSuffix(safe, plausibleExt(safe)) + ext
}

// plausibleExt returns the trailing extension only when it looks like one a
// person would type, so "beach.jpg" loses its extension but a name that merely
// contains dots, such as "2024.05.31-dunes", keeps every character.
func plausibleExt(name string) string {
	ext := filepath.Ext(name)
	if len(ext) < 2 || len(ext) > 5 {
		return ""
	}
	for _, r := range ext[1:] {
		if !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9') {
			return ""
		}
	}
	return ext
}

// makeDownloadName builds a meaningful name for the final file: the capture
// time when we could read one, otherwise the first frame's name.
func makeDownloadName(sourcePath, dateTime, ext string) string {
	if m := exifDateTime.FindStringSubmatch(dateTime); m != nil {
		return fmt.Sprintf("%s%s%s_%s%s%s_pano%s", m[1], m[2], m[3], m[4], m[5], m[6], ext)
	}
	base := filepath.Base(sourcePath)
	if sourcePath == "" {
		base = "stitch"
	}
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	return sanitise(stem, 60, "stitch") + "_pano" + ext
}

// uploadName turns an uploaded file's name into a safe on-disk name, keeping
// the extension when it is one the toolchain can read. taken records the stems
// already used by this job so two files never collide.
func uploadName(original string, index int, taken map[string]bool) (string, bool) {
	base := filepath.Base(strings.ReplaceAll(original, `\`, "/"))
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "image.png"
	}
	ext := strings.ToLower(filepath.Ext(base))
	known := allowedExts[ext]
	if !known && ext != "" {
		return "", false
	}
	stem := sanitise(strings.TrimSuffix(base, filepath.Ext(base)), 80, "image")
	if taken[stem] {
		stem = fmt.Sprintf("%s_%d", stem, index)
	}
	taken[stem] = true
	if !known {
		ext = ".png"
	}
	return stem + ext, true
}
