# Hugin Stitch GUI

A local web UI for panorama stitching that drives the [Hugin](https://hugin.sourceforge.io/)
command-line tools. Drop photos into the browser, reorder them, pick a projection, and get a
stitched panorama back.

The server is a single Go binary with the UI embedded in it. Nothing is bundled,
nothing is uploaded anywhere: images stay on the machine running it, and all the heavy lifting
is done by the Hugin binaries already installed on the system.

![Hugin Stitch GUI screenshot](static/screenshot.png)

## What it does

- Drag-and-drop queue with thumbnails, drag-to-reorder, and per-image removal
- Accepts JPEG, PNG, BMP, TIFF, WebP, HEIC, and RAW (CR2, CR3, NEF, ARW, DNG)
- Projection choice: auto, rectilinear, cylindrical, or equirectangular
- Auto mode measures the horizontal field of view from the optimised project and picks
  rectilinear up to 100°, cylindrical up to 240°, equirectangular beyond that
- Horizon levelling (on by default): rotates the panorama so the horizon sits
  flat, which also stops a level horizon rendering as a curve in cylindrical and
  equirectangular output
- Exposure and vignetting matching (on by default): solves the true brightness
  difference between frames from their overlaps rather than trusting EXIF, and
  corrects lens vignetting
- Control-point cleanup: discards matches found in drifting cloud and moving
  water before solving, which is where a tilted or bowed horizon usually starts
- Manual yaw/pitch/roll nudge for when the automatic orientation lands close but
  not quite
- Lens distortion is solved from the control points as part of alignment
- Live progress with per-phase weighting (cpfind and blending dominate the wall clock)
- PNG preview in the browser; output format selectable from all the formats
  Hugin supports, each explained in the UI before you commit: TIFF (8-bit
  lossless), PNG (8-bit lossless), JPEG (lossy, with a quality slider), HDR
  TIFF (32-bit float linear), or OpenEXR (32-bit float linear)
- Optional custom output filename, read when you click Download rather than
  when the stitch started, so a late rename still takes effect (the app falls
  back to a timestamp- or first-frame-derived name otherwise)
- Stop button to abandon a running stitch, which kills the whole tool tree and
  discards the partial output
- Copies the original capture time and EXIF data from the first frame into the
  stitched panorama (via `exiftool` when installed), and uses it to name the
  download file
- Uploads stream straight to disk, so a multi-gigabyte drop never has to fit in memory
- Finds the Hugin tools wherever the platform's installer put them, without
  needing them on `PATH`, and says what to install when they are missing

## Requirements

Hugin does the stitching, so its command-line tools have to be on the machine:

```
pto_gen  cpfind  autooptimiser  pano_modify  hugin_executor  nona  enblend
```

You do not have to put them on `PATH`. On startup the app looks on `PATH` and
then in the places each platform's installer actually uses, because installing
Hugin normally does not make its tools reachable from a shell: on macOS they
live inside the application bundle, and the Windows installer does not amend
`PATH`. The directories it found the tools in are handed down to the tools
themselves, since `hugin_executor` launches `nona` and `enblend` by name and
searches `PATH` on its own account.

If the tools are missing, the app says so when the page opens, naming what is
missing and how to install it on the system it is running on, rather than
waiting for a stitch to fail.

**macOS**

```bash
brew install --cask hugin          # or the disk image from the link below
brew install libraw exiftool       # optional: RAW decoding and metadata
```

**Windows**

Run the Hugin `.msi` installer. An install under `Program Files` is found
automatically. For RAW files and metadata, install LibRaw and ExifTool.

**Linux**

```bash
sudo apt install hugin-tools enblend libimage-exiftool-perl libraw-bin   # Debian/Ubuntu
sudo dnf install hugin enblend perl-Image-ExifTool LibRaw-tools          # Fedora
sudo pacman -S hugin enblend-enfuse perl-image-exiftool libraw           # Arch
```

Downloads for every platform: <https://hugin.sourceforge.io/download/>

If Hugin lives somewhere unusual, point at it directly:

```bash
./hugin-stitch-gui -hugin-dir /opt/hugin/bin
HUGIN_DIR=/opt/hugin/bin ./hugin-stitch-gui
```

Several directories can be listed, separated the way your platform separates
`PATH` entries. `/health` reports what was found and which directories were
searched.

### Optional tools

None of these are required; each one is skipped cleanly when absent.

- `exiftool` copies the capture time and camera details into the finished
  panorama, and names the download after the original shot.
- `cpclean` and `celeste_standalone` (both ship with `hugin-tools`) discard
  unreliable control points before alignment. `celeste` removes matches found
  in cloud, `cpclean` those whose alignment error is a statistical outlier.
- A RAW decoder is needed for CR2/CR3/NEF/ARW/DNG uploads. The app uses the
  first of `dcraw_emu` (from LibRaw), `dcraw` or `darktable-cli` it finds.
  Without one, RAW files are skipped and every other format is unaffected.

The only build-time dependency is the Go toolchain.

Or skip all of it and use the container, which has everything already: see
[Running with Docker](#running-with-docker).

## Building

```bash
go build -o hugin-stitch-gui .
```

The UI is embedded with `go:embed`, so the resulting binary is self-contained
and can be copied anywhere on its own. The build uses no cgo, so it
cross-compiles to any supported platform without a C toolchain:

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o hugin-stitch-gui.exe .
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o hugin-stitch-gui-macos .
```

The Hugin tools are still required at run time on whichever machine the binary
lands on.

## Running

```bash
./hugin-stitch-gui          # http://127.0.0.1:8765
./hugin-stitch-gui 9000     # or pick a port
./hugin-stitch-gui -host 0.0.0.0 -port 9000
```

Open the URL, drop images in, click **Stitch panorama**.

`HOST` and `PORT` environment variables are honoured when the matching flag is
not given. `-hugin-dir <dir>` (or `HUGIN_DIR`) points at a Hugin install the
app did not find on its own. `-static <dir>` serves the UI from a directory
instead of the embedded copy, which is convenient while editing the frontend.

## Testing

```bash
go test ./...
go test -race ./...
```

## Running with Docker

Build an image that bundles the app plus the Hugin toolchain, ExifTool and
LibRaw, then run it on any machine with Docker:

```bash
docker build -t hugin-stitch-gui .
docker run --rm -p 8765:8765 hugin-stitch-gui
```

Open http://localhost:8765. Inside the container the server binds to
`0.0.0.0` (set `HOST`/`PORT` env vars to override). Stitch output is served
over HTTP and scratch files live in the container's temp dir, so no volume is
required; mount one under it if you want outputs on the host.

Note that containers generally don't expose GPUs, so `enblend` falls back to
CPU blending unless you pass `--device /dev/dri` (or `--gpus all` with the
NVIDIA Container Toolkit) and your build of enblend was compiled with OpenCL.

## How stitching works

```
pto_gen        create the project from the input images
cpfind         find control points between overlapping frames
celeste        drop control points that landed in cloud
cpclean        drop control points whose alignment error is an outlier
autooptimiser  solve for camera positions and lens distortion (-a), level the
               horizon (-l), and match exposure and vignetting (-m)
pano_modify    apply any manual rotation, then set projection, field of view,
               crop, canvas, output type/quality
hugin_executor remap with nona, blend with enblend into the chosen format
```

`enblend` uses OpenCL when it was built with GPU support, which is where most of
the blending speed comes from on large panoramas.

### Why levelling matters

`autooptimiser -a` solves where each camera was pointing, but nothing in that
step says which way is up. A tripod that was a few degrees off leaves the whole
panorama rolled, and the horizon comes out tilted.

The same error also bends things. In cylindrical and equirectangular projection
a straight horizon renders as a straight line *only* when it lies on the
projection's equator. If the panorama's axis is pitched even slightly, a
dead-level horizon bows into an arc — which reads as lens distortion but is not.
Rectilinear output does not show this, which is why it only appears on wider
sweeps.

`-l` rotates the solved panorama so the horizon becomes the equator, fixing both
at once. It assumes the scene has a real horizon, so it is a per-stitch toggle:
turn it off for panoramas shot deliberately up or down.

When levelling lands close but not exact, or when the scene gives it no horizon
to work with, the fine-tune rotation fields nudge the result by hand. They are
applied before the field of view, crop and canvas are measured, so the output is
sized around the rotated panorama rather than cropped from it.

### Why exposure matching matters

Each frame carries its own metered exposure, and across a sweep that can differ
by a couple of stops. Blending hides a seam, but it cannot invent the brightness
that should have been there, so a large even area such as sky or water steps in
brightness from frame to frame.

`-m` solves the real relative exposure, white balance and lens vignetting from
the overlaps instead of trusting each frame's EXIF. On a five-frame test set it
narrowed a 2.08 EV spread to under 1 EV and recovered a vignetting coefficient
that was otherwise left at zero. It costs about a second.

## HTTP interface

| Method | Path             | Purpose                                      |
|--------|------------------|----------------------------------------------|
| POST   | `/stitch`        | Multipart upload of images + projection/format/quality/filename/level/photometric/yaw/pitch/roll fields, returns a job id |
| GET    | `/status/<id>`   | Job state, progress fraction, current phase message |
| POST   | `/cancel/<id>`   | Stop a running stitch; 409 if it already finished |
| GET    | `/result/<id>`   | PNG preview of the finished panorama         |
| GET    | `/download/<id>` | Stitched panorama in the chosen format (TIFF, PNG, JPEG, HDR TIFF, or EXR); `?name=` overrides the filename |
| GET    | `/health`        | Toolchain report: what was found, what is missing, where it looked, and how to install the rest on this platform (POST also accepted) |

`level` and `photometric` accept `0`/`false`/`off`/`no` to disable that
correction; any other value, including omitting the field, leaves it on.
`yaw`, `pitch` and `roll` are degrees, default `0`, clamped to ±180.

`/download` takes an optional `name` query parameter, which overrides the name
chosen when the job finished. The extension always comes from the format that
was actually stitched, never from the supplied name.

`/status` reports `running`, `done`, `error` or `cancelled`. Cancelling kills
the tool and everything it started, then deletes the job's working directory,
so a stopped stitch leaves nothing behind. On Windows only the tool itself is
killed, so a `nona` or `enblend` helper may run on briefly.

Uploads are capped at 4 GB per request. Jobs run in a background goroutine and write to a
temporary directory per job.

## Layout

```
main.go             flags, startup, graceful shutdown
server.go           HTTP handlers, streaming uploads, embedded UI
pipeline.go         the Hugin pipeline, phases, progress parsing
jobs.go             job store and its concurrency guards
pto.go              Hugin project parsing and auto projection choice
rotation.go         manual yaw/pitch/roll nudge
formats.go          output formats and projection codes
images.go           preview rendering and RAW decoding
exif.go             metadata transfer via ExifTool
naming.go           filename sanitising for uploads and downloads
proc_unix.go        process-group teardown so cancelling kills the tool tree
proc_windows.go     the Windows no-op equivalent
toolchain.go        finding the Hugin tools, and per-platform install advice
*_test.go           unit tests for the pipeline, parsing and HTTP surface
Dockerfile          container image with the full toolchain
static/index.html   UI markup
static/app.js       queue, drag-and-drop, progress polling
static/style.css    styling
static/screenshot.png  screenshot used in this README
```

## License

MIT — see [LICENSE](LICENSE). The application is your own code and only invokes
the external tools as separate command-line processes, so their copyleft
licenses (GPL for Hugin/Enblend, GPL/Artistic for ExifTool) don't apply to this
project; the binaries themselves keep their own licenses.

## Notes

This is a local tool. It binds to `127.0.0.1` and has no authentication, so don't expose it to a
network you don't control.
