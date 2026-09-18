package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// phase is one step of the pipeline and the slice of the progress bar it owns.
// cpfind is the long sequential feature-matching step; blending is long too.
type phase struct {
	message    string
	start, end float64
}

var phases = []phase{
	{"Preparing images...", 0.00, 0.04},
	{"Building control-point project (pto_gen)...", 0.04, 0.06},
	{"Finding control points (cpfind)...", 0.06, 0.56},
	{"Cleaning control points...", 0.56, 0.64},
	{"Optimizing alignment (autooptimiser)...", 0.64, 0.72},
	{"Preparing output (pano_modify)...", 0.72, 0.78},
	{"Stitching & blending (nona + enblend)...", 0.78, 0.99},
}

// Phase indices. Several steps need to name their own phase, and the blending
// phase is the only one that reports a percentage we can read back.
const (
	phaseClean    = 3
	phaseOptimise = 4
	phaseModify   = 5
	phaseStitch   = 6
)

// percentRe scoops up e.g. enblend's "83% " progress output.
var percentRe = regexp.MustCompile(`(\d{1,3}(?:\.\d+)?)\s*%`)

// Pipeline runs stitch jobs against the detected toolchain.
type Pipeline struct {
	tools Toolchain
}

func NewPipeline(tools Toolchain) *Pipeline { return &Pipeline{tools: tools} }

// Run executes the full Hugin pipeline for one job and records the result on
// the job itself. It is meant to be called on its own goroutine.
//
//	pto_gen -> cpfind -> autooptimiser -> pano_modify -> hugin_executor
//
// hugin_executor remaps with nona and blends with enblend (GPU-accelerated via
// OpenCL when enblend was built with it).
func (p *Pipeline) Run(ctx context.Context, job *Job, uploaded []string) {
	if !p.tools.Ready {
		job.fail(p.tools.MissingMessage())
		return
	}

	sources, unreadable := p.prepareSources(ctx, job, uploaded)
	if p.aborted(ctx, job) {
		return
	}
	if len(sources) < 2 {
		job.fail(tooFewMessage(unreadable))
		return
	}

	dir := job.Dir
	path := func(name string) string { return filepath.Join(dir, name) }

	// step runs one tool and reports whether the pipeline should carry on. A
	// job the user cancelled is not a failure, so it is reported separately.
	step := func(phase int, tool string, args []string) bool {
		err := p.runTool(ctx, job, phase, tool, args)
		if err == nil {
			return true
		}
		if !p.aborted(ctx, job) {
			job.fail("Stitching failed: " + err.Error())
		}
		return false
	}

	if !step(1, "pto_gen", append([]string{"-o", path("project.pto")}, sources...)) {
		return
	}

	// pto_gen exits 0 even when it could not read one of its inputs, so check
	// that every source reached the project instead of trusting the status.
	// A file dropped here would otherwise be stitched around silently, leaving
	// a panorama quietly missing a frame.
	if dropped, err := missingFromPTO(path("project.pto"), sources); err == nil && len(dropped) > 0 {
		gone := make(map[string]bool, len(dropped))
		for _, src := range dropped {
			gone[src] = true
			unreadable = append(unreadable, filepath.Base(src)+": not readable by Hugin")
		}
		kept := make([]string, 0, len(sources))
		for _, src := range sources {
			if !gone[src] {
				kept = append(kept, src)
			}
		}
		sources = kept
		if len(sources) < 2 {
			job.fail(tooFewMessage(unreadable))
			return
		}
	}
	if !step(2, "cpfind", []string{"-o", path("cp.pto"), path("project.pto")}) {
		return
	}

	cleaned := p.cleanControlPoints(ctx, job, path("cp.pto"))

	if !step(phaseOptimise, "autooptimiser",
		optimiserArgs(job.Level, job.Photometric, path("opt.pto"), cleaned)) {
		return
	}

	optPTO := path("opt.pto")
	projCode := resolveProjection(optPTO, job.Projection)
	job.setProjCode(projCode)
	caption := "Output projection: " + projectionFriendly[projCode]
	if hfov, ok := computeHFOV(optPTO); ok {
		caption += fmt.Sprintf(" (coverage ~%.0f°)", hfov)
	}
	job.setProgress(phases[phaseOptimise].end, caption)

	_, requested := lookupFormat(job.Format)
	// LDR jobs stitch to a lossless master and are encoded on download.
	format := masterFor(requested)
	modifyArgs := []string{"--projection=" + projCode}
	// The nudge has to be applied before the AUTO sizing options, so the
	// field of view, crop and canvas are all measured around the rotated
	// panorama rather than the one the optimiser produced.
	if !job.Rotation.IsZero() {
		modifyArgs = append(modifyArgs, job.Rotation.Arg())
	}
	modifyArgs = append(modifyArgs, "--fov=AUTO", "--crop=AUTO", "--canvas=AUTO")
	modifyArgs = append(modifyArgs, format.Args...)
	modifyArgs = append(modifyArgs, "-o", path("pp.pto"), optPTO)
	if !step(phaseModify, "pano_modify", modifyArgs) {
		return
	}

	if !step(phaseStitch, "hugin_executor", []string{
		"--prefix=" + path("result"), "--stitching", path("pp.pto"),
	}) {
		return
	}

	output := locateOutput(dir, format)
	if output == "" {
		job.fail("Hugin did not produce an output file. The images may have too little overlap.")
		return
	}

	previewSource := output
	if src := locatePreviewSource(dir, format); src != "" {
		previewSource = src
	}
	preview := makePreview(previewSource, dir)

	var dateTime string
	if requested.EXIF {
		dateTime = copyEXIF(ctx, p.tools, uploaded[0], output, job)
	}

	downloadName := makeDownloadName(uploaded[0], dateTime, requested.Ext)
	if job.Filename != "" {
		downloadName = cleanDownloadName(job.Filename, requested.Ext)
	}
	job.setResult(preview, output, downloadName)

	// Encode the format that was asked for now, while the user is still
	// looking at the preview, so the download itself is immediate. Other
	// formats are encoded on demand when the selector is changed.
	if requested.Convertible() {
		job.setProgress(phases[phaseStitch].end, "Preparing "+requested.Label+" download...")
		if _, err := encodeTo(output, dir, job.Format, job.Quality); err != nil {
			// Not fatal: the download path will try again and report properly.
			job.pushLine("could not pre-encode the " + requested.Label + " download: " + err.Error())
		}
	}

	// The frames, the copies made of them and the Hugin projects have all
	// done their work by now, and on a RAW shoot they are the bulk of the
	// directory. The panorama, its preview and any encoded download stay.
	pruneIntermediates(dir, append(append([]string{}, uploaded...), sources...),
		output, preview, previewSource)

	message := "Stitch complete"
	if name := projectionFriendly[projCode]; name != "" {
		message += " [" + name + "]"
	}
	if len(unreadable) > 0 {
		message += " (skipped: " + strings.Join(unreadable, "; ") + ")"
	}
	if dateTime != "" {
		message += " (captured " + strings.Replace(dateTime, " ", " @ ", 1) + ")"
	}
	job.finish(message)
}

// optimiserArgs builds the autooptimiser command line.
//
// -a solves camera positions and lens distortion from the control points.
//
// -l then rotates the whole panorama so the horizon becomes the projection's
// equator. That does two visible things: it removes the roll that leaves a
// horizon tilted, and it stops a level horizon from rendering as a curve,
// which is what an off-equator horizon looks like in cylindrical and
// equirectangular output.
//
// -m solves exposure, vignetting and white balance from the overlaps rather
// than trusting each frame's EXIF, which is what stops brightness stepping
// between frames across a large even area such as sky or water.
//
// All three together are what Hugin's own "Align" button runs.
func optimiserArgs(level, photometric bool, output, input string) []string {
	args := []string{"-a"}
	if level {
		args = append(args, "-l")
	}
	if photometric {
		args = append(args, "-m")
	}
	return append(args, "-o", output, input)
}

// cleanControlPoints drops control points that cannot be trusted, returning
// the project to optimise from.
//
// cpfind matches on appearance, so anything that moves between frames breeds
// confident but wrong matches: drifting cloud, wind-ruffled water and its
// reflections. Those points drag the optimiser off true, which shows up as a
// tilted or bowed horizon. celeste classifies and removes cloud points; cpclean
// removes whatever is left whose alignment error is a statistical outlier.
//
// Both tools are optional. When neither is installed, or when one fails, the
// original project is used and the job carries on: worse control points are a
// quality problem, never a reason to abandon a stitch.
func (p *Pipeline) cleanControlPoints(ctx context.Context, job *Job, input string) string {
	ph := phases[phaseClean]
	job.setProgress(ph.start, ph.message)

	dir := filepath.Dir(input)
	current := input

	steps := []struct {
		tool   string
		output string
		args   func(in, out string) []string
	}{
		{"celeste_standalone", "cp_celeste.pto", func(in, out string) []string {
			return []string{"-i", in, "-o", out}
		}},
		{"cpclean", "cp_clean.pto", func(in, out string) []string {
			return []string{"-o", out, in}
		}},
	}

	for _, step := range steps {
		if !p.tools.Optional[step.tool] {
			job.pushLine(step.tool + " not installed - skipping control-point cleanup step")
			continue
		}
		out := filepath.Join(dir, step.output)
		if err := p.runTool(ctx, job, phaseClean, step.tool, step.args(current, out)); err != nil {
			job.pushLine(step.tool + " failed, keeping the previous control points: " + err.Error())
			continue
		}
		if _, err := nonEmptyFile(out, step.tool); err != nil {
			job.pushLine(step.tool + " produced no project, keeping the previous control points")
			continue
		}
		current = out
	}

	job.setProgress(ph.end, ph.message)
	return current
}

// aborted reports whether the job was stopped by the user, recording the fact
// and discarding the half-finished work if so. A cancelled stitch can only
// have produced partial output, which may run to gigabytes.
func (p *Pipeline) aborted(ctx context.Context, job *Job) bool {
	if ctx.Err() == nil {
		return false
	}
	job.markCancelled()
	os.RemoveAll(job.Dir)
	return true
}

// prepareSources passes normal images straight through and demosaics RAW files
// so Hugin can read them. It returns the usable sources and a description of
// each file it had to skip.
func (p *Pipeline) prepareSources(ctx context.Context, job *Job, uploaded []string) (sources, unreadable []string) {
	for i, src := range uploaded {
		if ctx.Err() != nil {
			return nil, nil
		}
		job.setProgress(
			round4(phases[0].end*float64(i+1)/float64(len(uploaded))),
			fmt.Sprintf("Preparing images (%d/%d)...", i+1, len(uploaded)),
		)
		ready, err := prepareSource(src, job.Dir, p.tools)
		if err != nil {
			unreadable = append(unreadable, filepath.Base(src)+": "+err.Error())
			continue
		}
		if info, err := os.Stat(ready); err == nil && info.Size() > 0 {
			sources = append(sources, ready)
		}
	}
	return sources, unreadable
}

// runTool runs one Hugin tool, streaming its output into the job log and
// advancing the progress bar through the phase it owns.
func (p *Pipeline) runTool(ctx context.Context, job *Job, phaseIndex int, name string, args []string) error {
	ph := phases[phaseIndex]
	job.setProgress(ph.start, ph.message)

	cmd := exec.CommandContext(ctx, p.tools.Path(name), args...)
	cmd.Env = p.tools.Env()
	useProcessGroup(cmd)
	// Without a delay, Wait blocks on the output pipe until every process
	// holding it has gone, which a killed tool's children may not do promptly.
	cmd.WaitDelay = 2 * time.Second
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("could not run %q: %w", name, err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("tool not found: %s", name)
		}
		return fmt.Errorf("could not run %q: %w", name, err)
	}

	tail := newRingBuffer(200)
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	scanner.Split(scanProgressLines)
	for scanner.Scan() {
		line := scanner.Text()
		tail.push(line)
		job.pushLine(line)
		p.reportPercent(job, phaseIndex, line)
	}
	// Drain anything the scanner could not tokenise so the child never blocks.
	_, _ = io.Copy(io.Discard, pipe)

	if err := cmd.Wait(); err != nil {
		code := cmd.ProcessState.ExitCode()
		recent := strings.Join(tail.last(15), "\n")
		return fmt.Errorf("'%s' failed (exit %d)\n%s", name, code, recent)
	}
	job.setProgress(ph.end, ph.message)
	return nil
}

// reportPercent maps a tool's own percentage into the slice of the progress
// bar owned by the blending phase, the only phase long enough to report one.
func (p *Pipeline) reportPercent(job *Job, phaseIndex int, line string) {
	if phaseIndex != phaseStitch {
		return
	}
	m := percentRe.FindStringSubmatch(line)
	if m == nil {
		return
	}
	pct, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return
	}
	ph := phases[phaseStitch]
	frac := pct / 100
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	job.setProgress(
		round4(ph.start+(ph.end-ph.start)*frac),
		fmt.Sprintf("Stitching & blending... %.1f%%", pct),
	)
}

// scanProgressLines splits on CR as well as LF. Tools like enblend redraw a
// single progress line with carriage returns, so splitting only on LF would
// hide every percentage until the tool finished.
func scanProgressLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := strings.IndexAny(string(data), "\r\n"); i >= 0 {
		// Treat CRLF as one terminator.
		width := 1
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			width = 2
		}
		return i + width, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// ringBuffer keeps the last n lines of a tool's output for error reporting.
type ringBuffer struct {
	lines []string
	size  int
}

func newRingBuffer(size int) *ringBuffer { return &ringBuffer{size: size} }

func (r *ringBuffer) push(line string) {
	r.lines = append(r.lines, line)
	if n := len(r.lines) - r.size; n > 0 {
		r.lines = append(r.lines[:0], r.lines[n:]...)
	}
}

func (r *ringBuffer) last(n int) []string {
	if n > len(r.lines) {
		n = len(r.lines)
	}
	return r.lines[len(r.lines)-n:]
}

// locateOutput finds the stitched output for a job.
//
// Hugin names LDR output result.<ext> and HDR output result_hdr.<ext>, so
// prefer the exact name for the chosen format and fall back to the largest
// stitch file as a hedge against other Hugin versions.
func locateOutput(dir string, format Format) string {
	for _, name := range format.Basenames {
		if path := firstNonEmpty(dir, name); path != "" {
			return path
		}
	}
	var best string
	var bestSize int64
	for _, pattern := range []string{
		"result*.tif", "result*.tiff", "result*.jpg",
		"result*.jpeg", "result*.png", "result*.exr",
	} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, path := range matches {
			info, err := os.Stat(path)
			if err != nil || info.Size() == 0 {
				continue
			}
			if info.Size() > bestSize {
				best, bestSize = path, info.Size()
			}
		}
	}
	return best
}

// locatePreviewSource finds the LDR companion Hugin writes alongside an HDR
// output. The browser preview is built from that, because a 32-bit float image
// cannot be shown directly.
func locatePreviewSource(dir string, format Format) string {
	if !format.HDR {
		return ""
	}
	for _, name := range []string{
		"result.jpg", "result.jpeg", "result.png", "result.tif", "result.tiff",
	} {
		if path := firstNonEmpty(dir, name); path != "" {
			return path
		}
	}
	return ""
}

// firstNonEmpty returns dir/name when that file exists and has content.
func firstNonEmpty(dir, name string) string {
	path := filepath.Join(dir, name)
	if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Size() > 0 {
		return path
	}
	return ""
}

// tooFewMessage explains a job that ran out of usable inputs, naming whatever
// was rejected so the user knows which files to convert.
func tooFewMessage(unreadable []string) string {
	msg := "Need at least 2 readable images."
	if len(unreadable) > 0 {
		msg += " Unreadable: " + strings.Join(unreadable, "; ")
	}
	return msg
}
