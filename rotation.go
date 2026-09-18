package main

import (
	"math"
	"strconv"
	"strings"
)

// maxAngle bounds a fine-tune rotation. Anything beyond a half turn is the
// same rotation expressed the long way round.
const maxAngle = 180

// Rotation is a manual nudge applied to the solved panorama, in degrees.
//
// Automatic levelling gets the horizon close on most scenes, but it needs a
// real horizon to find. When it lands slightly off, or when the scene gives it
// nothing to work with, these offsets adjust the result without re-shooting.
type Rotation struct {
	Yaw   float64 // turn left/right, which recentres the panorama
	Pitch float64 // tilt up/down
	Roll  float64 // rotate in-plane, which is what levels a tilted horizon
}

// IsZero reports whether the panorama should be left where the optimiser put it.
func (r Rotation) IsZero() bool {
	return r.Yaw == 0 && r.Pitch == 0 && r.Roll == 0
}

// Arg renders the pano_modify flag. The tool takes the three angles in
// yaw,pitch,roll order.
func (r Rotation) Arg() string {
	return "--rotate=" + formatAngle(r.Yaw) + "," +
		formatAngle(r.Pitch) + "," + formatAngle(r.Roll)
}

func formatAngle(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// parseAngle reads one rotation field, clamping it and treating anything
// unparseable as no rotation at all.
func parseAngle(raw string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(v) {
		return 0
	}
	return math.Max(-maxAngle, math.Min(maxAngle, v))
}

// parseRotation reads the three rotation fields out of the upload form.
func parseRotation(fields map[string]string) Rotation {
	return Rotation{
		Yaw:   parseAngle(fields["yaw"]),
		Pitch: parseAngle(fields["pitch"]),
		Roll:  parseAngle(fields["roll"]),
	}
}
