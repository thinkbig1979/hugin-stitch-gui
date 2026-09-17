#!/usr/bin/env python3
"""Lightweight local web UI for panorama stitching backed by the Hugin CLI.

Run:
    python app.py [port]
Then open http://127.0.0.1:8765, drop images in, and click "Stitch".

Pipeline (all external tools, none bundled):
    pto_gen -> cpfind -> autooptimiser -> pano_modify -> hugin_executor
hugin_executor remaps with `nona` and blends with `enblend` (GPU-accelerated
via OpenCL when enblend was built with it).
"""

import glob
import json
import math
import mimetypes
import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

BASE_DIR = os.path.dirname(os.path.abspath(__file__))
STATIC_DIR = os.path.join(BASE_DIR, "static")

MAX_BODY = 4 * 1024 * 1024 * 1024  # 4 GB safety cap
ALLOWED_EXTS = {
    ".jpg", ".jpeg", ".png", ".bmp", ".tif", ".tiff", ".webp",
    ".cr2", ".cr3", ".nef", ".arw", ".dng", ".heic",
}
RAW_EXTS = {".cr2", ".cr3", ".nef", ".arw", ".dng"}

TOOLCHAIN = ["pto_gen", "cpfind", "autooptimiser", "pano_modify",
             "hugin_executor", "nona", "enblend"]
OPTIONAL_TOOLS = ["exiftool"]

JOBS = {}
JOBS_LOCK = threading.Lock()

_PCT_RE = re.compile(r"(\d{1,3}(?:\.\d+)?)\s*%")

# (phase name, progress when the phase STARTS, progress when phase DONE)
# cpfind is the long sequential-feature-matching step; blending is long too.
PHASES = [
    ("Preparing images...", 0.00, 0.04),
    ("Building control-point project (pto_gen)...", 0.04, 0.06),
    ("Finding control points (cpfind)...", 0.06, 0.60),
    ("Optimizing alignment (autooptimiser)...", 0.60, 0.70),
    ("Preparing output (pano_modify)...", 0.70, 0.76),
    ("Stitching & blending (nona + enblend)...", 0.76, 0.99),
]


def _tools_status():
    found = {}
    for tool in TOOLCHAIN:
        found[tool] = shutil.which(tool) is not None
    ready = all(found.values())
    missing = [t for t, ok in found.items() if not ok]
    optional = {t: shutil.which(t) is not None for t in OPTIONAL_TOOLS}
    return ready, found, missing, optional


ENGINE_READY, TOOLS, TOOLS_MISSING, OPT_TOOLS = _tools_status()


def _set_job(job_id, **fields):
    with JOBS_LOCK:
        job = JOBS.get(job_id)
        if job is not None:
            job.update(fields)


def _push_line(job_id, line):
    """Keep a bounded recent-log tail for error reporting."""
    with JOBS_LOCK:
        job = JOBS.get(job_id)
        if job is None:
            return
        lines = job.setdefault("log", [])
        lines.append(line)
        del lines[:-400]


def _run_tool(job_id, phase_index, cmd):
    """Run one Hugin tool, stream its output into the job log, return (ok, msg)."""
    _set_job(job_id, progress=PHASES[phase_index][1],
             message=PHASES[phase_index][0])
    env = dict(os.environ, LC_ALL="C")
    try:
        proc = subprocess.Popen(cmd, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, env=env)
    except FileNotFoundError:
        return False, "tool not found: " + cmd[0]
    tail = []
    while True:
        chunk = proc.stdout.read(4096)
        if not chunk:
            break
        try:
            text = chunk.decode("utf-8", "replace")
        except Exception:  # noqa: BLE001
            text = chunk.decode("latin-1", "replace")
        for line in text.splitlines():
            tail.append(line)
            del tail[:-200]
            _push_line(job_id, line)
            _maybe_progress(job_id, phase_index, line)
    proc.wait()
    if proc.returncode == 0:
        _set_job(job_id, progress=PHASES[phase_index][2])
        return True, ""
    msg = "\n".join(tail[-15:])
    return False, f"'{cmd[0]}' failed (exit {proc.returncode})\n{msg}"


def _maybe_progress(job_id, phase_index, line):
    """Scoop up e.g. enblend 'NN%' lines and map them into the current phase."""
    if phase_index != 5:  # only during stitching/blending
        return
    m = _PCT_RE.search(line)
    if m:
        try:
            pct = float(m.group(1))
        except ValueError:
            return
        lo, hi = PHASES[5][1], PHASES[5][2]
        frac = max(0.0, pct)
        _set_job(job_id,
                 progress=round(lo + (hi - lo) * min(frac / 100.0, 1.0), 4),
                 message=f"Stitching & blending... {pct:.1f}%")


def _prepare_source(path, job_dir):
    """Pass through normal images; demosaic RAW files to PNG for Hugin."""
    ext = os.path.splitext(path)[1].lower()
    if ext not in RAW_EXTS:
        return path, None
    try:
        import rawpy
        from PIL import Image
        raw = rawpy.imread(path)
        try:
            rgb = raw.postprocess()
        finally:
            raw.close()
        out = os.path.join(job_dir, os.path.splitext(os.path.basename(path))[0] + ".png")
        Image.fromarray(rgb).save(out)
        return out, None
    except Exception as exc:  # noqa: BLE001
        return None, f"{os.path.basename(path)}: {exc}"


def _locate_output(job_dir, fmt=None):
    """Find the stitched output for a job.

    Hugin names LDR output `result.<ext>` and HDR output `result_hdr.<ext>`,
    so prefer the exact name for the chosen format and fall back to the
    largest stitch file as a hedge against other Hugin versions.
    """
    if fmt in FORMATS:
        for pat in FORMATS[fmt]["basenames"]:
            hits = [p for p in glob.glob(os.path.join(job_dir, pat))
                    if os.path.getsize(p) > 0]
            if hits:
                return hits[0]
    candidates = []
    for pattern in ("result*.tif", "result*.tiff", "result*.jpg",
                    "result*.jpeg", "result*.png", "result*.exr"):
        candidates += glob.glob(os.path.join(job_dir, pattern))
    candidates = sorted((os.path.getsize(p), p) for p in candidates if os.path.getsize(p) > 0)
    if not candidates:
        return None
    return candidates[-1][1]  # largest


def _locate_preview_source(job_dir, fmt):
    """For HDR outputs Hugin also writes an LDR JPEG companion; use that to
    build the browser preview because PIL cannot decode 32-bit float images."""
    if not FORMATS[fmt]["hdr"]:
        return None
    for pat in ("result.jpg", "result.jpeg", "result.png",
                "result.tif", "result.tiff"):
        hits = [p for p in glob.glob(os.path.join(job_dir, pat))
                if os.path.getsize(p) > 0]
        if hits:
            return hits[0]
    return None


# Hugin projection codes (pto "p fX" value): rectilinear, cylindrical,
# equirectangular are the parameter-free workhorses.
PROJECTIONS = {
    "auto": None,
    "rectilinear": "0",
    "cylindrical": "2",
    "equirectangular": "3",
}
PROJECTION_FRIENDLY = {"0": "rectilinear", "2": "cylindrical", "3": "equirectangular"}


def _compute_hfov(pto_path):
    """Total horizontal field of view covered by the aligned images (degrees)."""
    spans = []
    for line in open(pto_path, encoding="utf-8", errors="replace"):
        if not line.startswith("i "):
            continue
        t = line.split()
        if len(t) < 16:
            continue
        try:
            w = int(re.sub(r"[^-\d]", "", t[1]))
            h = int(re.sub(r"[^-\d]", "", t[2]))
            fov_type = re.sub(r"[^-\d]", "", t[3])
            v = float(re.sub(r"[^-\d.]", "", t[4]))
            yaw = float(re.sub(r"[^-\d.]", "", t[15]))
        except (ValueError, IndexError):
            continue
        if fov_type == "1":  # fov given as vertical -> derive horizontal
            hfov = 2 * math.degrees(math.atan(math.tan(math.radians(v) / 2) * w / h))
        else:
            hfov = v
        spans.append((yaw - hfov / 2, yaw + hfov / 2))
    if not spans:
        return None
    return max(s[1] for s in spans) - min(s[0] for s in spans)


def _resolve_projection(opt_pto, projection):
    """Pick the projection for a sweep we didn't measure ahead of time."""
    if projection in PROJECTIONS and PROJECTIONS[projection] is not None:
        return PROJECTIONS[projection]
    hfov = _compute_hfov(opt_pto)
    if hfov is None:          # fall back to Hugin's default (cylindrical)
        return "2"
    if hfov <= 100:           # moderate sweep: everything stays straight
        return "0"
    if hfov <= 240:           # wide panorama: cylindrical kills edge stretch
        return "2"
    return "3"                # near-360: equirectangular


def _make_preview(image_path, job_dir):
    """Stitched image -> 8-bit PNG so the browser can display it."""
    try:
        from PIL import Image
        png_path = os.path.join(job_dir, "preview.png")
        with Image.open(image_path) as im:
            im.convert("RGB").save(png_path, optimize=True)
        return png_path
    except Exception:  # noqa: BLE001
        return None


def _copy_exif(src, dst, job_id):
    """Copy EXIF from one source frame into the stitched output with ExifTool.

    Copies all metadata, then clears the tags that would be wrong on a
    panorama (source pixel size, orientation). Two traps to avoid:

    - "Orientation#=1" (the '#' disables print conversion): a plain
      "Orientation=1" write stores value 3 ("Rotate 180") in ExifTool
      versions seen on Debian, turning the panorama upside down.
    - Never delete "ImageWidth"/"ImageHeight": on a TIFF those are the
      mandatory structural tags and deleting them corrupts the file.

    Returns the DateTimeOriginal value if present, else None.
    """
    exif = shutil.which("exiftool")
    if exif is None:
        _push_line(job_id, "exiftool not installed - skipping EXIF transfer "
                           "(sudo apt install libimage-exiftool-perl)")
        return None
    env = dict(os.environ, LC_ALL="C")
    try:
        subprocess.run([exif, "-q", "-overwrite_original",
                        "-TagsFromFile", src, "-all:all", dst],
                       check=True, capture_output=True, env=env)
        subprocess.run([exif, "-q", "-overwrite_original",
                        "-Orientation#=1", "-ExifImageWidth=",
                        "-ExifImageHeight=", dst],
                       check=True, capture_output=True, env=env)
    except subprocess.CalledProcessError as exc:
        _push_line(job_id, "exiftool transfer failed: "
                           + (exc.stderr or b"").decode("utf-8", "replace")[:300])
        return None
    try:
        out = subprocess.run([exif, "-s", "-s", "-s", "-DateTimeOriginal", dst],
                             capture_output=True, text=True, env=env)
        return out.stdout.strip() or None
    except Exception:  # noqa: BLE001
        return None


# Output formats exposed in the UI, mapped to pano_modify arguments.
# - LDR formats are encoded for display/storage (TIFF, PNG lossless; JPEG lossy).
# - HDR formats keep the full stitch precision as linear floating point in a
#   32-bit float container. Hugin emits a companion LDR JPEG alongside them so
#   the browser still gets a real preview.
FORMATS = {
    "tif": {
        "label": "TIFF", "ext": ".tif", "mime": "image/tiff",
        "basenames": ("result.tif", "result.tiff"),
        "args": ("--ldr-file=TIF",),
        "quality": False, "hdr": False, "exif": True,
    },
    "png": {
        "label": "PNG", "ext": ".png", "mime": "image/png",
        "basenames": ("result.png",),
        "args": ("--ldr-file=PNG",),
        "quality": False, "hdr": False, "exif": True,
    },
    "jpg": {
        "label": "JPEG", "ext": ".jpg", "mime": "image/jpeg",
        "basenames": ("result.jpg", "result.jpeg"),
        "args": ("--ldr-file=JPG",),
        "quality": True, "hdr": False, "exif": True,
    },
    "tif_hdr": {
        "label": "HDR TIFF", "ext": ".tif", "mime": "image/tiff",
        "basenames": ("result_hdr.tif",),
        "args": ("--output-type=HDR,NORMAL", "--ldr-file=JPG",
                 "--hdr-file=TIF"),
        "quality": False, "hdr": True, "exif": False,
    },
    "exr": {
        "label": "OpenEXR", "ext": ".exr", "mime": "image/x-exr",
        "basenames": ("result_hdr.exr",),
        "args": ("--output-type=HDR,NORMAL", "--ldr-file=JPG",
                 "--hdr-file=EXR"),
        "quality": False, "hdr": True, "exif": False,
    },
}


def _clean_download_name(name, ext):
    """Sanitise a user-supplied filename and force the right extension."""
    safe = re.sub(r"[^A-Za-z0-9._-]", "_", (name or "").strip())[:80] or "stitch"
    return os.path.splitext(safe)[0] + ext


def _make_download_name(source_path, datetime_str, ext):
    """A meaningful name for the final file: capture time if we have it,
    otherwise the first frame's name."""
    if datetime_str:
        m = re.match(r"(\d{4}):(\d{2}):(\d{2}) (\d{2}):(\d{2}):(\d{2})", datetime_str)
        if m:
            return (f"{m.group(1)}{m.group(2)}{m.group(3)}_"
                    f"{m.group(4)}{m.group(5)}{m.group(6)}_pano{ext}")
    stem = os.path.splitext(os.path.basename(source_path or "stitch"))[0]
    safe = re.sub(r"[^A-Za-z0-9._-]", "_", stem)[:60] or "stitch"
    return f"{safe}_pano{ext}"


def _run_job(job_id, image_paths):
    with JOBS_LOCK:
        job = JOBS.get(job_id)
        if job is None:
            return
        job_dir = job["dir"]

    if not ENGINE_READY:
        _set_job(job_id, state="error", progress=1,
                 message="Hugin is not installed. On Debian/Ubuntu run: "
                         "sudo apt install hugin enblend")
        return

    sources = []
    unreadable = []
    for idx, path in enumerate(image_paths):
        _set_job(job_id, progress=round(0.02 * (idx + 1) / max(1, len(image_paths)), 4),
                 message=f"Preparing images ({idx + 1}/{len(image_paths)})...")
        src, err = _prepare_source(path, job_dir)
        if src is None:
            unreadable.append(err)
        elif os.path.getsize(src) > 0:
            sources.append(src)

    if len(sources) < 2:
        _set_job(job_id, state="error", progress=1,
                 message="Need at least 2 readable images."
                         + ((" Unreadable: " + "; ".join(unreadable)) if unreadable else ""))
        return

    try:
        ok, msg = _run_tool(job_id, 1, ["pto_gen", "-o", os.path.join(job_dir, "project.pto")] + sources)
        if not ok:
            raise RuntimeError(msg)
        ok, msg = _run_tool(job_id, 2, ["cpfind", "-o", os.path.join(job_dir, "cp.pto"),
                                        os.path.join(job_dir, "project.pto")])
        if not ok:
            raise RuntimeError(msg)
        ok, msg = _run_tool(job_id, 3, ["autooptimiser", "-a", "-o",
                                        os.path.join(job_dir, "opt.pto"),
                                        os.path.join(job_dir, "cp.pto")])
        if not ok:
            raise RuntimeError(msg)
        opt_pto = os.path.join(job_dir, "opt.pto")
        projection = job.get("projection") or "auto"
        proj_code = _resolve_projection(opt_pto, projection)
        proj_name = PROJECTION_FRIENDLY.get(proj_code, proj_code)
        hfov = _compute_hfov(opt_pto)
        _set_job(job_id, last_proj_code=proj_code,
                 message=f"Output projection: {proj_name}"
                         + (f" (coverage ~{hfov:.0f}\u00b0)" if hfov else ""))
        fmt = (job.get("format") or "tif").lower()
        fmt_cfg = FORMATS[fmt]
        ext = fmt_cfg["ext"]
        modify_args = ["pano_modify", f"--projection={proj_code}",
                       "--fov=AUTO", "--crop=AUTO", "--canvas=AUTO",
                       *fmt_cfg["args"]]
        if fmt_cfg["quality"]:
            modify_args.append(f"--ldr-compression={int(job.get('quality') or 90)}")
        modify_args += ["-o", os.path.join(job_dir, "pp.pto"), opt_pto]
        ok, msg = _run_tool(job_id, 4, modify_args)
        if not ok:
            raise RuntimeError(msg)
        ok, msg = _run_tool(job_id, 5, ["hugin_executor",
                                        "--prefix=" + os.path.join(job_dir, "result"),
                                        "--stitching", os.path.join(job_dir, "pp.pto")])
        if not ok:
            raise RuntimeError(msg)
    except Exception as exc:  # noqa: BLE001
        _set_job(job_id, state="error", progress=1,
                 message="Stitching failed: " + str(exc).strip())
        return

    output = _locate_output(job_dir, fmt)
    if output is None:
        _set_job(job_id, state="error", progress=1,
                 message="Hugin did not produce an output file. The images may "
                         "have too little overlap.")
        return

    preview = _make_preview(_locate_preview_source(job_dir, fmt) or output, job_dir)
    dt = None
    if fmt_cfg["exif"]:
        dt = _copy_exif(image_paths[0], output, job_id)
    if job.get("filename"):
        download_name = _clean_download_name(job["filename"], ext)
    else:
        download_name = _make_download_name(image_paths[0], dt, ext)
    with JOBS_LOCK:
        job = JOBS.get(job_id)
        if job is None:
            return
        job["preview"] = preview
        job["download"] = output
        job["download_name"] = download_name
        proj_name = PROJECTION_FRIENDLY.get(job.get("last_proj_code", ""), "")

    extra = ""
    if unreadable:
        extra = " (skipped: " + "; ".join(unreadable) + ")"
    proj_tag = f" [{proj_name}]" if proj_name else ""
    if dt:
        extra += f" (captured {dt.replace(' ', ' @ ')})"
    _set_job(job_id, state="done", progress=1.0,
             message="Stitch complete" + proj_tag + extra)


def _parse_boundary(content_type):
    m = re.search(r"boundary=([^;]+)", content_type or "")
    if not m:
        return None
    return m.group(1).strip('"').encode("utf-8")


def parse_multipart(body, boundary):
    """Parse a multipart body into (files, fields).

    files: list of (filename, content-bytes) for file parts.
    fields: dict of name -> utf-8 value for regular form fields.
    """
    delim = b"--" + boundary
    files = []
    fields = {}
    if not body.startswith(delim):
        return files, fields
    cursor = body.index(delim) + len(delim)
    while True:
        if body[cursor:cursor + 2] == b"--":
            break
        if not body[cursor:cursor + 2] == b"\r\n":
            break
        cursor += 2
        end = body.find(b"\r\n" + delim, cursor)
        if end == -1:
            break
        raw = body[cursor:end]
        cursor = end + 2 + len(delim)
        hdr_end = raw.find(b"\r\n\r\n")
        if hdr_end == -1:
            continue
        header_blob = raw[:hdr_end].decode("latin-1")
        content = raw[hdr_end + 4:]
        filename = None
        field_name = None
        for line in header_blob.split("\r\n"):
            if line.lower().startswith("content-disposition:"):
                m = re.search(r'filename="([^"]*)"', line, re.IGNORECASE)
                if m:
                    filename = os.path.basename(m.group(1))
                n = re.search(r'name="([^"]*)"', line, re.IGNORECASE)
                if n:
                    field_name = n.group(1)
        if filename:
            files.append((filename, content))
        elif field_name:
            fields[field_name] = content.decode("utf-8", "replace")
    return files, fields


class Handler(BaseHTTPRequestHandler):
    server_version = "HuginStitchUI/1.0"

    def log_message(self, fmt, *args):
        sys.stdout.write("[%s] %s\n" % (time.strftime("%H:%M:%S"), fmt % args))

    # -- helpers ---------------------------------------------------------
    def _send(self, code, content, content_type="application/json"):
        if isinstance(content, str):
            content = content.encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(content)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(content)

    def _send_json(self, code, payload):
        self._send(code, json.dumps(payload, ensure_ascii=False))

    def _read_body(self):
        length = self.headers.get("Content-Length")
        if length is None:
            return None
        length = int(length)
        if length > MAX_BODY:
            return None
        return self.rfile.read(length)

    def _get_job(self, job_id):
        with JOBS_LOCK:
            return JOBS.get(job_id)

    # -- GET --------------------------------------------------------------
    def do_GET(self):
        path = urlsplit(self.path).path

        if path.startswith("/status/"):
            job_id = path.rsplit("/", 1)[-1]
            job = self._get_job(job_id)
            if job is None:
                self._send_json(404, {"error": "unknown job"})
                return
            self._send_json(200, {
                "state": job.get("state", "running"),
                "progress": round(job.get("progress", 0), 4),
                "message": job.get("message", ""),
            })
            return

        if path.startswith("/result/") or path.startswith("/download/"):
            job_id = path.rsplit("/", 1)[-1]
            job = self._get_job(job_id)
            if job is None:
                self._send_json(404, {"error": "unknown job"})
                return
            if path.startswith("/download/"):
                target = job.get("download")
                fmt = (job.get("format") or "tif").lower()
                cfg = FORMATS.get(fmt)
                if cfg:
                    content_type = cfg["mime"]
                else:
                    ext = os.path.splitext(target or "")[1].lower()
                    content_type = {"": "image/tiff", ".png": "image/png"}.get(
                        ext, "image/jpeg" if ext in (".jpg", ".jpeg") else "image/tiff")
                name = job.get("download_name", "stitched" + (cfg["ext"] if cfg else ".tif"))
            else:
                target = job.get("preview")
                content_type = "image/png"
                name = "preview.png"
            if not target or not os.path.exists(target):
                self._send_json(404, {"error": "result not ready"})
                return
            data = open(target, "rb").read()
            self.send_response(200)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Content-Disposition", f'attachment; filename="{name}"')
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(data)
            return

        if path in ("/", "/index.html"):
            path = "/index.html"
        rel = path.lstrip("/")
        file_path = os.path.normpath(os.path.join(STATIC_DIR, rel))
        if not file_path.startswith(STATIC_DIR) or not os.path.isfile(file_path):
            self._send_json(404, {"error": "not found"})
            return
        content_type = mimetypes.guess_type(file_path)[0] or "application/octet-stream"
        data = open(file_path, "rb").read()
        self._send(200, data, content_type)

    # -- POST -------------------------------------------------------------
    def do_POST(self):
        path = urlsplit(self.path).path

        if path == "/health":
            self._send_json(200, {
                "engine_ready": ENGINE_READY,
                "tools": TOOLS,
                "missing": TOOLS_MISSING,
                "optional_tools": OPT_TOOLS,
            })
            return

        if path == "/stitch":
            if not ENGINE_READY:
                self._send_json(500, {
                    "error": "Hugin is not installed. Install it with: "
                             "sudo apt install hugin enblend  (missing: "
                             + ", ".join(TOOLS_MISSING) + ")"
                })
                return
            body = self._read_body()
            if body is None:
                self._send_json(400, {"error": "invalid request body"})
                return
            boundary = _parse_boundary(self.headers.get("Content-Type", ""))
            if boundary is None:
                self._send_json(400, {"error": "expected multipart/form-data"})
                return

            files, fields = parse_multipart(body, boundary)
            images = [(fn, data) for fn, data in files if data]
            if len(images) < 2:
                self._send_json(400, {"error": "You must drop at least 2 image files."})
                return

            projection = (fields.get("projection") or "auto").strip().lower()
            if projection not in PROJECTIONS:
                projection = "auto"

            fmt = (fields.get("format") or "tif").strip().lower()
            if fmt not in FORMATS:
                fmt = "tif"
            try:
                quality = min(100, max(1, int(float(fields.get("quality") or 90))))
            except ValueError:
                quality = 90
            filename = (fields.get("filename") or "").strip()[:100]

            job_id = uuid.uuid4().hex
            job_dir = tempfile.mkdtemp(prefix="hugin_")
            image_paths = []
            seen = set()
            for idx, (fname, data) in enumerate(images):
                fname = fname or "image.png"
                ext = os.path.splitext(fname)[1].lower()
                if ext in ALLOWED_EXTS or not ext:
                    stem = os.path.splitext(os.path.basename(fname))[0]
                    safe = re.sub(r"[^A-Za-z0-9._-]", "_", stem)[:80] or "image"
                    if safe in seen:
                        safe = f"{safe}_{idx}"
                    seen.add(safe)
                    out_path = os.path.join(job_dir, safe + (ext if ext in ALLOWED_EXTS else ".png"))
                    try:
                        with open(out_path, "wb") as fh:
                            fh.write(data)
                        image_paths.append(out_path)
                    except OSError:
                        pass

            if len(image_paths) < 2:
                self._send_json(400, {"error": "Could not save the uploaded files."})
                return

            with JOBS_LOCK:
                JOBS[job_id] = {
                    "state": "running",
                    "progress": 0.0,
                    "message": "Starting...",
                    "dir": job_dir,
                    "projection": projection,
                    "format": fmt,
                    "quality": quality,
                    "filename": filename,
                }

            t = threading.Thread(target=_run_job, args=(job_id, image_paths), daemon=True)
            t.start()
            self._send_json(200, {"job_id": job_id})
            return

        self._send_json(404, {"error": "unknown endpoint"})
        return


def main():
    port = 8765
    if len(sys.argv) > 1:
        port = int(sys.argv[1])

    if not ENGINE_READY:
        print("WARNING: Hugin CLI tools missing:", ", ".join(TOOLS_MISSING), file=sys.stderr)
        print("Install with: sudo apt install hugin enblend", file=sys.stderr)

    host = os.environ.get("HOST", "127.0.0.1")
    httpd = ThreadingHTTPServer((host, port), Handler)
    httpd.daemon_threads = True
    print(f"Serving Hugin Stitch UI at http://{host}:{port}  (Ctrl+C to stop)")
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        print("\nStopped.")


if __name__ == "__main__":
    main()