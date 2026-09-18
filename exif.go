package main

import (
	"context"
	"os/exec"
	"strings"
)

// copyEXIF copies EXIF from one source frame into the stitched output with
// ExifTool, then clears the tags that would be wrong on a panorama (source
// pixel size, orientation).
//
// Two traps to avoid:
//
//   - "Orientation#=1" (the '#' disables print conversion): a plain
//     "Orientation=1" write stores value 3 ("Rotate 180") in ExifTool versions
//     seen on Debian, turning the panorama upside down.
//   - Never delete "ImageWidth"/"ImageHeight": on a TIFF those are the
//     mandatory structural tags and deleting them corrupts the file.
//
// Returns the DateTimeOriginal value if present, else "".
func copyEXIF(ctx context.Context, tools Toolchain, src, dst string, job *Job) string {
	if !tools.Has("exiftool") {
		job.pushLine("exiftool not installed - skipping EXIF transfer. " +
			"Install it to keep the capture time and camera details.")
		return ""
	}
	exifTool := tools.Path("exiftool")

	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, exifTool, args...)
		cmd.Env = tools.Env()
		output, err := cmd.CombinedOutput()
		if err != nil {
			job.pushLine("exiftool transfer failed: " + firstLine(string(output), err))
		}
		return err
	}

	if err := run("-q", "-overwrite_original", "-TagsFromFile", src, "-all:all", dst); err != nil {
		return ""
	}
	if err := run("-q", "-overwrite_original",
		"-Orientation#=1", "-ExifImageWidth=", "-ExifImageHeight=", dst); err != nil {
		return ""
	}

	cmd := exec.CommandContext(ctx, exifTool, "-s", "-s", "-s", "-DateTimeOriginal", dst)
	cmd.Env = tools.Env()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
