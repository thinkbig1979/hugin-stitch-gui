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
const resultWrap = document.getElementById("result-wrap");
const resultImg = document.getElementById("result-img");
const downloadLink = document.getElementById("download-link");

let files = [];
let stitching = false;
let currentJob = null;
let cancelling = false;

const PROJ_HINTS = {
  auto: "Auto measures the final field of view and picks the projection: rectilinear for moderate sweeps, cylindrical for wide-but-far-from-360\u00b0, equirectangular near 360\u00b0.",
  rectilinear: "Straight lines stay straight. Use for narrow sweeps (\u2264100\u00b0); beyond that the edges stretch badly.",
  cylindrical: "Good for wide panoramas (100\u2013240\u00b0): verticals stay straight near the center, no edge stretch.",
  equirectangular: "Spherical mapping; only recommended near 360\u00b0.",
};

const FORMAT_LABELS = { tif: "TIFF", png: "PNG", jpg: "JPEG", tif_hdr: "HDR TIFF", exr: "EXR" };

const FORMAT_HINTS = {
  tif: "8-bit RGBA, LZW (lossless). Best all-round choice for display and printing: pixel-exact, compact, and readable everywhere.",
  png: "8-bit RGBA, Deflate (lossless). Universally supported and pixel-exact, but the files are larger than TIFF at equal quality.",
  jpg: "8-bit RGB, lossy DCT compression. Smallest files, fine for sharing \u2014 but colours, gradients and fine detail degrade (quality slider above).",
  tif_hdr: "Linear, scene-referred 32-bit float. Keeps the full precision and range of the stitch with no tone mapping \u2014 the archive choice for further editing. Large files, and normal image viewers may not open it.",
  exr: "OpenEXR, 32-bit float, PIZ (lossless). The standard interchange format for HDR, VFX and 3D pipelines. Same precision as HDR TIFF; browsers can\u2019t show it, so the preview below is a separate JPEG.",
};

const formatHint = document.getElementById("format-hint");

function updateFormatControls() {
  qualityWrap.classList.toggle("hidden", formatSelect.value !== "jpg");
  formatHint.textContent = FORMAT_HINTS[formatSelect.value] || "";
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
  const name = outputNameInput.value.trim();
  downloadLink.href = name ? `${base}?name=${encodeURIComponent(name)}` : base;
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

function addFiles(list) {
  const valid = Array.from(list).filter((f) => f.type.startsWith("image/") ||
    /\.(jpe?g|png|bmp|tiff?|webp|cr2|cr3|nef|arw|dng|heic)$/i.test(f.name));
  for (const f of valid) {
    files.push({ file: f, url: URL.createObjectURL(f) });
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

function updateProjHint() {
  projHint.textContent = PROJ_HINTS[projectionSelect.value] || "";
}
projectionSelect.addEventListener("change", updateProjHint);
updateProjHint();

formatSelect.addEventListener("change", updateFormatControls);
qualityInput.addEventListener("input", () => {
  qualityValue.textContent = qualityInput.value;
});
updateFormatControls();