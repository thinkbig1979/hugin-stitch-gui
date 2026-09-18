package main

import (
	"context"
	"fmt"
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

// copyMetadata transfers a finished panorama's own metadata from the master
// into a re-encoded copy of it.
//
// The stitch tags the master and only the master, but every LDR download
// except TIFF is re-encoded from it by Go's image packages, which write no
// metadata whatsoever. Without this step a JPEG or PNG download arrives bare,
// having lost the capture time the stitch went to the trouble of copying.
//
// Two things make this safe to do bluntly:
//
//   - ExifTool identifies both files from their content, so dst may still be
//     the part-written temporary that has not got its real extension yet.
//   - Tags describing the master's own storage rather than the picture, the
//     TIFF strip offsets among them, are "unsafe" in ExifTool's sense and are
//     left behind by the "-all:all" wildcard instead of corrupting the copy.
//
// Orientation and the source pixel size need no second thought here: copyEXIF
// already corrected them on the master, so what arrives is already right.
func copyMetadata(tools Toolchain, master, dst string) error {
	if !tools.Has("exiftool") {
		return nil
	}
	cmd := exec.Command(tools.Path("exiftool"), "-q", "-overwrite_original",
		"-TagsFromFile", master, "-all:all", dst)
	cmd.Env = tools.Env()
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("exiftool: %s", firstLine(string(output), err))
	}
	return nil
}
