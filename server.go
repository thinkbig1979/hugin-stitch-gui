package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// MaxUploadBytes caps a single /stitch request. Uploads stream to disk, so
// this bounds disk use rather than memory.
const MaxUploadBytes = 4 << 30 // 4 GB

// staticAssets holds the UI. Embedding it means the binary runs anywhere
// without a directory of files beside it. The README screenshot is left out on
// purpose: it is documentation, not part of the app.
//
//go:embed static/index.html static/app.js static/style.css
var staticAssets embed.FS

// Server wires the HTTP surface to the job store and the stitch pipeline.
type Server struct {
	tools    Toolchain
	jobs     *JobStore
	pipeline *Pipeline
	static   http.FileSystem
	// ctx is cancelled at shutdown so running tools are torn down with it.
	ctx context.Context
}

// NewServer builds the HTTP surface. When staticDir is non-empty the UI is
// served from that directory instead of the embedded copy, which is convenient
// while editing the frontend.
func NewServer(ctx context.Context, tools Toolchain, staticDir string) (*Server, error) {
	var assets http.FileSystem
	if staticDir != "" {
		if _, err := os.Stat(filepath.Join(staticDir, "index.html")); err != nil {
			return nil, err
		}
		assets = http.Dir(staticDir)
	} else {
		sub, err := fs.Sub(staticAssets, "static")
		if err != nil {
			return nil, err
		}
		assets = http.FS(sub)
	}
	return &Server{
		tools:    tools,
		jobs:     NewJobStore(),
		pipeline: NewPipeline(tools),
		static:   assets,
		ctx:      ctx,
	}, nil
}

// Handler returns the router for the whole app.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/stitch", s.handleStitch)
	mux.HandleFunc("/status/", s.handleStatus)
	mux.HandleFunc("/cancel/", s.handleCancel)
	mux.HandleFunc("/result/", s.handleResult)
	mux.HandleFunc("/download/", s.handleDownload)
	mux.HandleFunc("/health", s.handleHealth)
	mux.Handle("/", s.staticHandler())
	return logRequests(mux)
}

// -- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}

// jobFromPath pulls the trailing job id out of a request path and looks it up.
func (s *Server) jobFromPath(w http.ResponseWriter, r *http.Request) (*Job, bool) {
	id := path.Base(r.URL.Path)
	job, ok := s.jobs.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown job")
		return nil, false
	}
	return job, true
}

// -- handlers --------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.tools)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, job.Status())
}

// handleCancel stops a running stitch at the user's request.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	job, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	if !job.Cancel() {
		// The job finished on its own between the click and this request.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "job is no longer running",
			"state": job.Status().State,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	preview, _, _ := job.results()
	s.serveFile(w, r, preview, "image/png", "preview.png")
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	_, download, name := job.results()
	_, format := lookupFormat(job.Format)
	// A name in the query wins, so editing the filename after the stitch has
	// finished still renames the download. The extension always comes from the
	// format that was actually stitched, never from what the user typed.
	if requested := strings.TrimSpace(r.URL.Query().Get("name")); requested != "" {
		name = cleanDownloadName(requested, format.Ext)
	}
	if name == "" {
		name = "stitched" + format.Ext
	}
	s.serveFile(w, r, download, format.MIME, name)
}

// serveFile sends a finished job artefact as an attachment.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, target, contentType, name string) {
	if target == "" {
		writeError(w, http.StatusNotFound, "result not ready")
		return
	}
	f, err := os.Open(target)
	if err != nil {
		writeError(w, http.StatusNotFound, "result not ready")
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusNotFound, "result not ready")
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// staticHandler serves the UI.
//
// The assets are opened directly rather than through http.FileServer, which
// redirects "/index.html" to "/" and would bounce endlessly against the "/" to
// index.html mapping this app needs.
func (s *Server) staticHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := path.Clean(r.URL.Path)
		if name == "/" || name == "." {
			name = "/index.html"
		}
		if strings.Contains(name, "..") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}

		file, err := s.static.Open(name)
		if err != nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		defer file.Close()

		info, err := file.Stat()
		if err != nil || info.IsDir() {
			writeError(w, http.StatusNotFound, "not found")
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	})
}

// handleStitch accepts a multipart upload, stores the frames, and starts a job.
func (s *Server) handleStitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if !s.tools.Ready {
		writeError(w, http.StatusInternalServerError, s.tools.MissingMessage())
		return
	}

	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected multipart/form-data")
		return
	}

	dir, err := os.MkdirTemp("", "hugin_")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create a work directory")
		return
	}

	images, fields, err := receiveUpload(reader, dir)
	if err != nil {
		os.RemoveAll(dir)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(images) < 2 {
		os.RemoveAll(dir)
		writeError(w, http.StatusBadRequest, "You must drop at least 2 image files.")
		return
	}

	formatKey, _ := lookupFormat(strings.ToLower(strings.TrimSpace(fields["format"])))
	job := &Job{
		ID:          newJobID(),
		Dir:         dir,
		Projection:  normaliseProjection(strings.ToLower(strings.TrimSpace(fields["projection"]))),
		Format:      formatKey,
		Quality:     parseQuality(fields["quality"]),
		Level:       parseLevel(fields["level"]),
		Photometric: parseLevel(fields["photometric"]),
		Rotation:    parseRotation(fields),
		Filename:    truncate(strings.TrimSpace(fields["filename"]), 100),
		state:       StateRunning,
		message:     "Starting...",
	}
	// Each job gets its own context so one can be cancelled without
	// disturbing the others, while server shutdown still stops them all.
	jobCtx, cancel := context.WithCancel(s.ctx)
	job.cancel = cancel
	s.jobs.Add(job)

	go func() {
		defer cancel()
		s.pipeline.Run(jobCtx, job, images)
	}()

	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})
}

// receiveUpload streams each multipart part to disk and collects the form
// fields. Frames are written as they arrive, so a multi-gigabyte upload never
// has to fit in memory.
func receiveUpload(reader *multipart.Reader, dir string) (images []string, fields map[string]string, err error) {
	fields = make(map[string]string)
	taken := make(map[string]bool)
	var total int64

	for index := 0; ; index++ {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, errors.New("invalid request body")
		}

		if part.FileName() == "" {
			value, err := io.ReadAll(io.LimitReader(part, 4096))
			part.Close()
			if err != nil {
				return nil, nil, errors.New("invalid request body")
			}
			fields[part.FormName()] = string(value)
			continue
		}

		name, ok := uploadName(part.FileName(), index, taken)
		if !ok {
			part.Close()
			continue // an extension the toolchain cannot read
		}

		written, err := writePart(filepath.Join(dir, name), part, MaxUploadBytes-total)
		part.Close()
		if err != nil {
			return nil, nil, err
		}
		total += written
		if written > 0 {
			images = append(images, filepath.Join(dir, name))
		}
	}
	return images, fields, nil
}

// writePart copies one uploaded file to disk, refusing to exceed the request
// budget left in limit.
func writePart(dst string, src io.Reader, limit int64) (int64, error) {
	if limit <= 0 {
		return 0, errors.New("upload is larger than the 4 GB limit")
	}
	f, err := os.Create(dst)
	if err != nil {
		return 0, errors.New("could not save the uploaded files")
	}
	defer f.Close()

	written, err := io.Copy(f, io.LimitReader(src, limit))
	if err != nil {
		return written, errors.New("could not save the uploaded files")
	}
	if written == limit {
		return written, errors.New("upload is larger than the 4 GB limit")
	}
	return written, nil
}

// parseLevel reads an on/off form field. These corrections are on unless the
// client explicitly disables one, so a request that omits the field entirely
// still gets the corrected behaviour.
func parseLevel(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

func parseQuality(raw string) int {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 90
	}
	quality := int(value)
	if quality < 1 {
		return 1
	}
	if quality > 100 {
		return 100
	}
	return quality
}

func truncate(s string, limit int) string {
	if len(s) > limit {
		return s[:limit]
	}
	return s
}

// logRequests writes one line per request, so a local run shows what the UI is doing.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		log.Printf("%s %s", r.Method, r.URL.Path)
	})
}
