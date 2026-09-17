# Hugin Stitch GUI

A local web UI for panorama stitching that drives the [Hugin](https://hugin.sourceforge.io/)
command-line tools. Drop photos into the browser, reorder them, pick a projection, and get a
stitched panorama back.

The server is a single Python file on the standard library `http.server`. Nothing is bundled,
nothing is uploaded anywhere: images stay on the machine running it, and all the heavy lifting
is done by the Hugin binaries already installed on the system.

## What it does

- Drag-and-drop queue with thumbnails, drag-to-reorder, and per-image removal
- Accepts JPEG, PNG, BMP, TIFF, WebP, HEIC, and RAW (CR2, CR3, NEF, ARW, DNG)
- Projection choice: auto, rectilinear, cylindrical, or equirectangular
- Auto mode measures the horizontal field of view from the optimised project and picks
  rectilinear up to 100°, cylindrical up to 240°, equirectangular beyond that
- Live progress with per-phase weighting (cpfind and blending dominate the wall clock)
- PNG preview in the browser, full-resolution TIFF download

## Requirements

Hugin's CLI tools must be on `PATH`:

```
pto_gen  cpfind  autooptimiser  pano_modify  hugin_executor  nona  enblend
```

On Debian/Ubuntu:

```bash
sudo apt install hugin-tools enblend
```

`exiftool` is optional and used for metadata when present.

Python dependencies (`Pillow` for previews, `rawpy` for RAW decoding):

```bash
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
```

## Running

```bash
python app.py          # http://127.0.0.1:8765
python app.py 9000     # or pick a port
```

Open the URL, drop images in, click **Stitch panorama**.

## How stitching works

```
pto_gen        create the project from the input images
cpfind         find control points between overlapping frames
autooptimiser  solve for camera positions and lens parameters
pano_modify    set projection, field of view, crop, and canvas
hugin_executor remap with nona, blend with enblend
```

`enblend` uses OpenCL when it was built with GPU support, which is where most of the blending
speed comes from on large panoramas.

## HTTP interface

| Method | Path             | Purpose                                      |
|--------|------------------|----------------------------------------------|
| POST   | `/stitch`        | Multipart upload of images + projection choice, returns a job id |
| GET    | `/status/<id>`   | Job state, progress fraction, current phase message |
| GET    | `/result/<id>`   | PNG preview of the finished panorama         |
| GET    | `/download/<id>` | Full-resolution TIFF                         |
| POST   | `/health`        | Toolchain availability report                |

Uploads are capped at 4 GB per request. Jobs run in a background thread and write to a
temporary directory per job.

## Layout

```
app.py              server, job runner, and Hugin pipeline
requirements.txt    Pillow, rawpy
static/index.html   UI markup
static/app.js       queue, drag-and-drop, progress polling
static/style.css    styling
```

## Notes

This is a local tool. It binds to `127.0.0.1` and has no authentication, so don't expose it to a
network you don't control.
