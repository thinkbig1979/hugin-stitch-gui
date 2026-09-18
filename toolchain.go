package main

import (
	"os/exec"
	"sort"
	"strings"
)

// toolchain lists the Hugin binaries the stitch pipeline shells out to. None
// of them are bundled; they must be on PATH.
var toolchain = []string{
	"pto_gen", "cpfind", "autooptimiser", "pano_modify",
	"hugin_executor", "nona", "enblend",
}

// optionalTools improve the result when present but never block a stitch.
var optionalTools = []string{"exiftool", "cpclean", "celeste_standalone"}

// allowedExts are the upload extensions the pipeline will accept.
var allowedExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".bmp": true,
	".tif": true, ".tiff": true, ".webp": true,
	".cr2": true, ".cr3": true, ".nef": true, ".arw": true,
	".dng": true, ".heic": true,
}

// rawExts need demosaicing before Hugin can read them.
var rawExts = map[string]bool{
	".cr2": true, ".cr3": true, ".nef": true, ".arw": true, ".dng": true,
}

// Toolchain is the availability report shown by /health and printed at startup.
type Toolchain struct {
	Ready    bool            `json:"engine_ready"`
	Tools    map[string]bool `json:"tools"`
	Missing  []string        `json:"missing"`
	Optional map[string]bool `json:"optional_tools"`
	// RawConverter names the RAW decoder in use, or "" when none was found.
	RawConverter string `json:"raw_converter"`
}

// InstallHint is the message shown when the Hugin tools are missing.
const InstallHint = "Hugin is not installed. On Debian/Ubuntu run: sudo apt install hugin-tools enblend"

// DetectToolchain probes PATH for every tool the pipeline can use.
func DetectToolchain() Toolchain {
	tc := Toolchain{
		Ready:    true,
		Tools:    make(map[string]bool, len(toolchain)),
		Optional: make(map[string]bool, len(optionalTools)),
		Missing:  []string{},
	}
	for _, name := range toolchain {
		ok := have(name)
		tc.Tools[name] = ok
		if !ok {
			tc.Ready = false
			tc.Missing = append(tc.Missing, name)
		}
	}
	for _, name := range optionalTools {
		tc.Optional[name] = have(name)
	}
	if conv, ok := detectRawConverter(); ok {
		tc.RawConverter = conv.name
		tc.Optional[conv.name] = true
	}
	sort.Strings(tc.Missing)
	return tc
}

// MissingMessage describes the missing tools for the UI.
func (tc Toolchain) MissingMessage() string {
	if tc.Ready {
		return ""
	}
	return InstallHint + "  (missing: " + strings.Join(tc.Missing, ", ") + ")"
}

func have(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
