"use strict";

const dropZone = document.getElementById("drop-zone");
const fileInput = document.getElementById("file-input");
const thumbList = document.getElementById("thumb-list");
const queueSection = document.getElementById("queue");
const queueCount = document.getElementById("queue-count");
const clearBtn = document.getElementById("clear-btn");
const stitchBtn = document.getElementById("stitch-btn");
const projectionSelect = document.getElementById("projection");
const projHint = document.getElementById("proj-hint");
const formatSelect = document.getElementById("format");
const qualityWrap = document.getElementById("quality-wrap");
const qualityInput = document.getElementById("quality");
const qualityValue = document.getElementById("quality-value");
const outputNameInput = document.getElementById("output-name");
const levelInput = document.getElementById("level");
const photometricInput = document.getElementById("photometric");
const rollInput = document.getElementById("roll");
const pitchInput = document.getElementById("pitch");
const yawInput = document.getElementById("yaw");
const resetTuneBtn = document.getElementById("reset-tune");
const progressWrap = document.getElementById("progress-wrap");
const progressFill = document.getElementById("progress-fill");
const progressMsg = document.getElementById("progress-msg");
const cancelBtn = document.getElementById("cancel-btn");
const downloadNote = document.getElementById("download-note");
const setupBanner = document.getElementById("setup");
const setupMissing = document.getElementById("setup-missing");
const setupPlatform = document.getElementById("setup-platform");
const setupSteps = document.getElementById("setup-steps");
const setupLink = document.getElementById("setup-link");
const decoderNote = document.getElementById("decoder-note");
const resultWrap = document.getElementById("result-wrap");
const resultImg = document.getElementById("result-img");
const downloadLink = document.getElementById("download-link");

let files = [];
let stitching = false;
let currentJob = null;
let cancelling = false;
let stitchedFormat = null;

const PROJ_HINTS = {
  auto: "Auto measures the final field of view and picks the projection: rectilinear for moderate sweeps, cylindrical for wide-but-far-from-360\u00b0, equirectangular near 360\u00b0.",
  rectilinear: "Straight lines stay straight. Use for narrow sweeps (\u2264100\u00b0); beyond that the edges stretch badly.",
  cylindrical: "Good for wide panoramas (100\u2013240\u00b0): verticals stay straight near the center, no edge stretch.",
  equirectangular: "Spherical mapping; only recommended near 360\u00b0.",
};

const FORMAT_LABELS = { tif: "TIFF", png: "PNG", jpg: "JPEG", tif_hdr: "HDR TIFF", exr: "EXR" };

const FORMAT_HINTS = {
  tif: "8-bit RGBA, LZW (lossless), exactly as the blender wrote it. Best all-round choice for display and printing: pixel-exact, compact, and readable everywhere.",
  png: "8-bit RGBA, Deflate (lossless), re-encoded from the TIFF. Universally supported and pixel-exact, but the files are larger than TIFF at equal quality.",
  jpg: "8-bit RGB, lossy DCT compression, re-encoded from the lossless TIFF so the quality slider always works from the original pixels. Smallest files, fine for sharing \u2014 but colours, gradients and fine detail degrade.",
  tif_hdr: "Linear, scene-referred 32-bit float. Keeps the full precision and range of the stitch with no tone mapping \u2014 the archive choice for further editing. Large files, and normal image viewers may not open it. Must be chosen before stitching.",
  exr: "OpenEXR, 32-bit float, PIZ (lossless). The standard interchange format for HDR, VFX and 3D pipelines. Same precision as HDR TIFF; browsers can\u2019t show it, so the preview below is a separate JPEG. Must be chosen before stitching.",
};

// HDR output carries floating point data that only the stitch produces, so it
// cannot be re-encoded from a finished panorama the way the others can.
const HDR_FORMATS = new Set(["tif_hdr", "exr"]);

const formatHint = document.getElementById("format-hint");

function updateFormatControls() {
  qualityWrap.classList.toggle("hidden", formatSelect.value !== "jpg");
  formatHint.textContent = FORMAT_HINTS[formatSelect.value] || "";
  updateDownloadLabel();
  refreshDownloadHref();
}

function updateDownloadLabel() {
  downloadLink.textContent = `Download\u00a0${FORMAT_LABELS[formatSelect.value] || "TIFF"}`;
}

// The filename is read when the link is followed, not when the stitch was
// started, so editing it after the panorama is ready still renames the file.
// An empty field leaves the server's own name in place.
function refreshDownloadHref() {
  const base = downloadLink.dataset.base;
  if (!base) return;

  const wanted = formatSelect.value;
  // Switching to an HDR format after the fact needs another stitch, because
  // the floating point data was never produced.
  const needsRestitch =
    HDR_FORMATS.has(wanted) && wanted !== stitchedFormat;

  downloadLink.classList.toggle("disabled", needsRestitch);
  downloadNote.textContent = needsRestitch
    ? `${FORMAT_LABELS[wanted]} has to be produced by the stitch itself. Press Stitch panorama again to get it.`
    : "";
  if (needsRestitch) {
    downloadLink.removeAttribute("href");
    return;
  }

  const params = new URLSearchParams();
  const name = outputNameInput.value.trim();
  if (name) params.set("name", name);
  if (!HDR_FORMATS.has(stitchedFormat)) {
    params.set("format", wanted);
    if (wanted === "jpg") params.set("quality", String(qualityInput.value));
  }
  const query = params.toString();
  downloadLink.href = query ? `${base}?${query}` : base;
}

function show(el) { el.classList.remove("hidden"); }
function hide(el) { el.classList.add("hidden"); }

function refreshQueue() {
  queueCount.textContent = `(${files.length})`;
  if (files.length === 0) {
    hide(queueSection);
  } else {
    show(queueSection);
  }
  stitchBtn.disabled = files.length < 2 || stitching;
  thumbList.replaceChildren();
  files.forEach((entry, i) => thumbList.appendChild(makeThumb(entry, i)));
}

function makeThumb(entry, i) {
  const li = document.createElement("li");
  li.className = "thumb";
  li.draggable = true;
  li.dataset.index = String(i);

  const img = document.createElement("img");
  img.src = entry.url;
  img.alt = entry.file.name;

  const badge = document.createElement("span");
  badge.className = "badge";
  badge.textContent = String(i + 1);

  const name = document.createElement("span");
  name.className = "thumb-name";
  name.textContent = entry.file.name;
  name.title = entry.file.name;

  const rem = document.createElement("button");
  rem.type = "button";
  rem.className = "remove";
  rem.setAttribute("aria-label", "Remove " + entry.file.name);
  rem.textContent = "\u00d7";
  rem.addEventListener("click", () => {
    files.splice(i, 1);
    refreshQueue();
  });

  li.append(img, badge, name, rem);
  return li;
}

// Formats the server has no decoder for. Filled in from /health so the page
// stops offering something this machine cannot read, rather than accepting the
// file and failing once the stitch is already under way.
const undecodable = new Set();

const DECODER_FAMILIES = [
  { family: "heif", label: "HEIC", exts: [".heic", ".heif"] },
  { family: "raw", label: "RAW", exts: [".cr2", ".cr3", ".nef", ".arw", ".dng"] },
];

function applyDecoders(decoders) {
  undecodable.clear();
  const missing = [];
  for (const { family, label, exts } of DECODER_FAMILIES) {
    if (decoders && decoders[family]) continue;
    for (const ext of exts) undecodable.add(ext);
    missing.push(label);
  }

  // Keep the file picker's own list in step with what the server can read.
  fileInput.accept = fileInput.accept
    .split(",")
    .filter((entry) => !undecodable.has(entry.trim().toLowerCase()))
    .join(",");

  if (missing.length) {
    decoderNote.textContent =
      `${missing.join(" and ")} files need a decoder that is not installed here, ` +
      "so they are not accepted.";
    show(decoderNote);
  } else {
    hide(decoderNote);
  }
}

function extensionOf(name) {
  const dot = name.lastIndexOf(".");
  return dot < 0 ? "" : name.slice(dot).toLowerCase();
}

function addFiles(list) {
  const rejected = [];
  const valid = Array.from(list).filter((f) => {
    // Check this before the MIME test: a HEIC reports "image/heic", so a
    // format the server cannot decode would otherwise slip through.
    if (undecodable.has(extensionOf(f.name))) {
      rejected.push(f.name);
      return false;
    }
    return f.type.startsWith("image/") ||
      /\.(jpe?g|png|bmp|tiff?|webp|cr2|cr3|nef|arw|dng|heic|heif)$/i.test(f.name);
  });
  for (const f of valid) {
    files.push({ file: f, url: URL.createObjectURL(f) });
  }
  if (rejected.length) {
    decoderNote.textContent =
      `Skipped ${rejected.join(", ")}: no decoder for that format is installed here.`;
    show(decoderNote);
  }
  refreshQueue();
}

dropZone.addEventListener("click", () => {
  if (!stitching) fileInput.click();
});
fileInput.addEventListener("change", () => {
  addFiles(fileInput.files);
  fileInput.value = "";
});

["dragenter", "dragover"].forEach((ev) =>
  dropZone.addEventListener(ev, (e) => {
    e.preventDefault();
    dropZone.classList.add("drag-over");
  })
);
["dragleave", "drop"].forEach((ev) =>
  dropZone.addEventListener(ev, (e) => {
    e.preventDefault();
    dropZone.classList.remove("drag-over");
  })
);
dropZone.addEventListener("drop", (e) => {
  if (e.dataTransfer && e.dataTransfer.files.length) {
    addFiles(e.dataTransfer.files);
  }
});

// Reordering via drag-and-drop on thumbnails
let dragIndex = null;
thumbList.addEventListener("dragstart", (e) => {
  const li = e.target.closest("li.thumb");
  if (!li) return;
  dragIndex = Number(li.dataset.index);
  li.classList.add("dragging");
  e.dataTransfer.effectAllowed = "move";
});
thumbList.addEventListener("dragover", (e) => {
  e.preventDefault();
  const li = e.target.closest("li.thumb");
  if (!li || dragIndex === null) return;
  const overIndex = Number(li.dataset.index);
  if (overIndex === dragIndex) return;
  const [moved] = files.splice(dragIndex, 1);
  files.splice(overIndex, 0, moved);
  dragIndex = overIndex;
  refreshQueue();
});
thumbList.addEventListener("dragend", () => { dragIndex = null; refreshQueue(); });

clearBtn.addEventListener("click", () => {
  for (const entry of files) URL.revokeObjectURL(entry.url);
  files = [];
  refreshQueue();
});

stitchBtn.addEventListener("click", async () => {
  if (stitching || files.length < 2) return;
  stitching = true;
  hide(resultWrap);
  show(progressWrap);
  progressFill.style.width = "0%";
  progressMsg.textContent = "Uploading images...";
  stitchBtn.disabled = true;

  const form = new FormData();
  for (const entry of files) form.append("files", entry.file);
  form.append("projection", projectionSelect.value);
  form.append("format", formatSelect.value);
  if (formatSelect.value === "jpg") {
    form.append("quality", String(qualityInput.value));
  }
  form.append("filename", outputNameInput.value.trim());
  form.append("level", levelInput.checked ? "1" : "0");
  form.append("photometric", photometricInput.checked ? "1" : "0");
  form.append("roll", rollInput.value || "0");
  form.append("pitch", pitchInput.value || "0");
  form.append("yaw", yawInput.value || "0");

  let jobId;
  cancelling = false;
  try {
    const res = await fetch("/stitch", { method: "POST", body: form });
    const payload = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(payload.error || `upload failed (${res.status})`);
    jobId = payload.job_id;
  } catch (err) {
    finishWithError(err.message);
    return;
  }

  currentJob = jobId;
  stitchedFormat = formatSelect.value;
  show(cancelBtn);
  poll(jobId);
});

async function poll(jobId) {
  try {
    const res = await fetch(`/status/${jobId}`);
    const st = await res.json();
    progressFill.style.width = `${Math.round((st.progress || 0) * 100)}%`;
    progressMsg.textContent = st.message || "Working...";

    if (st.state === "done") {
      stitching = false;
      hide(cancelBtn);
      currentJob = null;
      showResult(`/result/${jobId}`, `/download/${jobId}`);
      return;
    }
    if (st.state === "cancelled") {
      finishStitch(st.message || "Stitch cancelled.");
      return;
    }
    if (st.state === "error") {
      finishWithError(st.message || "Stitching failed.");
      return;
    }
    setTimeout(() => poll(jobId), 800);
  } catch (err) {
    finishWithError(err.message);
  }
}

function showResult(previewUrl, downloadUrl) {
  progressMsg.textContent = "";
  hide(progressWrap);
  resultImg.src = previewUrl;
  downloadLink.dataset.base = downloadUrl;
  refreshDownloadHref();
  updateDownloadLabel();
  show(resultWrap);
  resultWrap.scrollIntoView({ behavior: "smooth", block: "nearest" });
  stitching = false;
  refreshQueue();
}

cancelBtn.addEventListener("click", async () => {
  if (!currentJob || cancelling) return;
  cancelling = true;
  cancelBtn.disabled = true;
  progressMsg.textContent = "Stopping...";
  try {
    await fetch(`/cancel/${currentJob}`, { method: "POST" });
  } catch (err) {
    // The poll below reports whatever actually happened to the job.
  }
  cancelBtn.disabled = false;
});

// Ends a stitch without treating the outcome as a failure.
function finishStitch(msg) {
  stitching = false;
  cancelling = false;
  currentJob = null;
  hide(cancelBtn);
  progressFill.style.width = "0%";
  progressMsg.textContent = msg;
  refreshQueue();
}

function finishWithError(msg) {
  stitching = false;
  cancelling = false;
  currentJob = null;
  hide(cancelBtn);
  progressFill.style.width = "0%";
  progressMsg.textContent = msg;
  progressMsg.classList.add("error");
  show(progressWrap);
  refreshQueue();
  setTimeout(() => progressMsg.classList.remove("error"), 6000);
}

outputNameInput.addEventListener("input", refreshDownloadHref);

resetTuneBtn.addEventListener("click", () => {
  rollInput.value = "0";
  pitchInput.value = "0";
  yawInput.value = "0";
});

// Ask the server what it found on startup, so a machine without the Hugin
// tools says so when the page opens rather than after a failed stitch.
async function checkToolchain() {
  let health;
  try {
    const res = await fetch("/health", { method: "POST" });
    health = await res.json();
  } catch (err) {
    return; // the server is the thing that is unreachable; nothing to advise
  }
  applyDecoders(health && health.decoders);

  if (!health || health.engine_ready) {
    hide(setupBanner);
    return;
  }

  const missing = health.missing || [];
  setupMissing.textContent = missing.length
    ? `Missing: ${missing.join(", ")}`
    : "The Hugin command-line tools could not be found.";

  const install = health.install || {};
  setupPlatform.textContent = install.platform
    ? `To install on ${install.platform}:`
    : "To install:";

  setupSteps.replaceChildren();
  for (const step of install.steps || []) {
    const li = document.createElement("li");
    // A step that is a command reads better as one.
    if (/^(brew|sudo|winget|choco)\s/.test(step) || step.includes(":  ")) {
      const code = document.createElement("code");
      code.textContent = step;
      li.appendChild(code);
    } else {
      li.textContent = step;
    }
    setupSteps.appendChild(li);
  }
  if (install.url) setupLink.href = install.url;

  show(setupBanner);
}

function updateProjHint() {
  projHint.textContent = PROJ_HINTS[projectionSelect.value] || "";
}
projectionSelect.addEventListener("change", updateProjHint);
updateProjHint();

formatSelect.addEventListener("change", updateFormatControls);
qualityInput.addEventListener("input", () => {
  qualityValue.textContent = qualityInput.value;
  refreshDownloadHref();
});
updateFormatControls();
checkToolchain();