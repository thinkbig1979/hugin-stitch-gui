package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// toolchain lists the Hugin binaries the stitch pipeline shells out to. None
// of them are bundled; they are found on the system at startup.
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
	".dng": true, ".heic": true, ".heif": true,
}

// needsDecode maps an upload extension to the converter family that can read
// it. Hugin reads anything absent from this map directly; anything present
// has to be decoded first, and cannot be stitched at all when the machine has
// no decoder for that family installed.
var needsDecode = map[string]decodeFamily{
	".cr2": familyRaw, ".cr3": familyRaw, ".nef": familyRaw,
	".arw": familyRaw, ".dng": familyRaw,
	".heic": familyHEIF, ".heif": familyHEIF,
}

// InstallHelp tells the user how to get the missing tools on their own system.
type InstallHelp struct {
	Platform string   `json:"platform"`
	Steps    []string `json:"steps"`
	URL      string   `json:"url"`
}

// Toolchain is the availability report shown by /health, by the UI banner and
// at startup.
type Toolchain struct {
	Ready    bool            `json:"engine_ready"`
	Tools    map[string]bool `json:"tools"`
	Missing  []string        `json:"missing"`
	Optional map[string]bool `json:"optional_tools"`
	// RawConverter names the RAW decoder in use, or "" when none was found.
	// It has its own field because /health reported it before there were
	// other families; Decoders is the general form.
	RawConverter string `json:"raw_converter"`
	// Decoders names the converter chosen for each format family that needs
	// one. A family missing from the map has no decoder on this machine.
	Decoders map[string]string `json:"decoders"`
	// Install describes how to install what is missing, for this platform.
	Install InstallHelp `json:"install"`
	// Searched lists the directories probed beyond PATH, so a user whose
	// install lives somewhere unusual can see what was looked at.
	Searched []string `json:"searched"`

	// paths maps a tool name to where it was actually found. Tools are run by
	// absolute path, because on macOS and Windows a normal Hugin install does
	// not put them on PATH.
	paths map[string]string
}

// Path returns where a tool was found, falling back to the bare name so exec
// can still search PATH itself.
func (tc Toolchain) Path(name string) string {
	if p := tc.paths[name]; p != "" {
		return p
	}
	return name
}

// Has reports whether an optional tool is available.
func (tc Toolchain) Has(name string) bool { return tc.Optional[name] }

// DecoderFor names the converter chosen for a family, or "" when the machine
// has none.
func (tc Toolchain) DecoderFor(family decodeFamily) string {
	return tc.Decoders[string(family)]
}

// CanDecode reports whether an upload extension can be read on this machine,
// either by Hugin directly or through a decoder that was found.
func (tc Toolchain) CanDecode(ext string) bool {
	family, needed := needsDecode[strings.ToLower(ext)]
	return !needed || tc.DecoderFor(family) != ""
}

// ToolDirs lists the directories the tools were found in, in a stable order
// and without duplicates.
func (tc Toolchain) ToolDirs() []string {
	var dirs []string
	seen := make(map[string]bool)
	// Iterate the tool lists rather than the map so the order is stable.
	for _, name := range append(append([]string{}, toolchain...), optionalTools...) {
		path := tc.paths[name]
		if path == "" {
			continue
		}
		dir := filepath.Dir(path)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// Env builds the environment for running one of the tools.
//
// The tool directories are put on PATH because finding the tools ourselves is
// not enough: hugin_executor is a launcher that starts nona and enblend by
// bare name, so it searches PATH on its own account. Without this, a Hugin
// that is installed but not on PATH remaps successfully and then fails at the
// blending step with "execvp(enblend) failed".
func (tc Toolchain) Env() []string {
	env := os.Environ()
	dirs := tc.ToolDirs()
	if len(dirs) > 0 {
		separator := string(filepath.ListSeparator)
		path := strings.Join(dirs, separator)
		if existing := os.Getenv("PATH"); existing != "" {
			path += separator + existing
		}
		env = append(env, "PATH="+path)
	}
	// LC_ALL keeps the tools' output parseable regardless of locale.
	return append(env, "LC_ALL=C")
}

// DetectToolchain finds every tool the pipeline can use.
//
// Tools are looked for on PATH and in the places each platform's installer
// actually puts them. That matters because installing Hugin normally does not
// make its command-line tools reachable: on macOS they live inside the
// application bundle, and on Windows the installer does not amend PATH. Any
// directories in extraDirs are searched first, so an unusual install can be
// pointed at explicitly.
func DetectToolchain(extraDirs ...string) Toolchain {
	searched := append(append([]string{}, extraDirs...), platformDirs()...)

	tc := Toolchain{
		Ready:    true,
		Tools:    make(map[string]bool, len(toolchain)),
		Optional: make(map[string]bool, len(optionalTools)),
		Missing:  []string{},
		Decoders: make(map[string]string, len(decodeFamilies)),
		Searched: searched,
		paths:    make(map[string]string),
	}

	for _, name := range toolchain {
		path, ok := findTool(name, searched)
		tc.Tools[name] = ok
		if ok {
			tc.paths[name] = path
			continue
		}
		tc.Ready = false
		tc.Missing = append(tc.Missing, name)
	}
	for _, name := range optionalTools {
		if path, ok := findTool(name, searched); ok {
			tc.Optional[name] = true
			tc.paths[name] = path
		} else {
			tc.Optional[name] = false
		}
	}
	// Decoders are probed after the main tools so that the environment passed
	// to a verify hook already carries the Hugin directories.
	for _, entry := range decodeFamilies {
		for _, conv := range entry.converters {
			path, ok := findTool(conv.name, searched)
			if !ok {
				continue
			}
			// Finding the binary is not always proof it can read the format.
			if conv.verify != nil && !conv.verify(path, tc.Env()) {
				continue
			}
			tc.Decoders[string(entry.family)] = conv.name
			tc.Optional[conv.name] = true
			tc.paths[conv.name] = path
			break
		}
	}
	tc.RawConverter = tc.Decoders[string(familyRaw)]

	sort.Strings(tc.Missing)
	tc.Install = installHelp()
	return tc
}

// findTool looks on PATH first, then in each of dirs.
func findTool(name string, dirs []string) (string, bool) {
	if path, err := exec.LookPath(name); err == nil {
		return path, true
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		for _, candidate := range executableNames(name) {
			full := filepath.Join(dir, candidate)
			if isExecutableFile(full) {
				return full, true
			}
		}
	}
	return "", false
}

// executableNames returns the filenames a tool may have on this platform.
func executableNames(name string) []string {
	if runtime.GOOS == "windows" {
		return []string{name + ".exe", name}
	}
	return []string{name}
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true // the extension already decided it
	}
	return info.Mode()&0o111 != 0
}

// platformDirs lists where each platform's Hugin installer puts the tools,
// most likely first.
func platformDirs() []string {
	switch runtime.GOOS {
	case "darwin":
		var dirs []string
		for _, root := range []string{"/Applications", filepath.Join(os.Getenv("HOME"), "Applications")} {
			dirs = append(dirs,
				// Layout of the official disk image.
				filepath.Join(root, "Hugin", "tools_mac"),
				filepath.Join(root, "Hugin", "Hugin.app", "Contents", "MacOS"),
				filepath.Join(root, "Hugin", "HuginStitchProject.app", "Contents", "MacOS"),
				// Layout when the bundle is dragged out on its own.
				filepath.Join(root, "Hugin.app", "Contents", "MacOS"),
			)
		}
		// Homebrew, for the optional tools and for enblend.
		return append(dirs, "/opt/homebrew/bin", "/usr/local/bin")
	case "windows":
		var dirs []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
			if root := os.Getenv(env); root != "" {
				dirs = append(dirs, filepath.Join(root, "Hugin", "bin"))
			}
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			dirs = append(dirs, filepath.Join(local, "Programs", "Hugin", "bin"))
		}
		return dirs
	default:
		return []string{"/usr/bin", "/usr/local/bin", "/opt/hugin/bin"}
	}
}

// installHelp returns instructions for the platform actually being used,
// rather than assuming everyone is on Debian.
func installHelp() InstallHelp {
	const downloads = "https://hugin.sourceforge.io/download/"
	switch runtime.GOOS {
	case "darwin":
		return InstallHelp{
			Platform: "macOS",
			URL:      downloads,
			Steps: []string{
				"brew install --cask hugin",
				"or download the disk image and drag Hugin to /Applications",
				"No PATH changes needed: the tools live inside the app bundle and are found there automatically.",
				"For RAW files and metadata: brew install libraw exiftool",
				"HEIC needs nothing extra: macOS ships sips, which decodes it.",
			},
		}
	case "windows":
		return InstallHelp{
			Platform: "Windows",
			URL:      downloads,
			Steps: []string{
				"Download and run the Hugin .msi installer",
				"No PATH changes needed: an install under Program Files is found automatically.",
				"For RAW files and metadata, install LibRaw and ExifTool and put them on PATH.",
				"For HEIC, install ImageMagick with HEIC support and put it on PATH.",
			},
		}
	default:
		return InstallHelp{
			Platform: "Linux",
			URL:      downloads,
			Steps: []string{
				"Debian/Ubuntu:  sudo apt install hugin-tools enblend libimage-exiftool-perl libraw-bin libheif-examples",
				"Fedora:         sudo dnf install hugin enblend perl-Image-ExifTool LibRaw-tools libheif-tools",
				"Arch:           sudo pacman -S hugin enblend-enfuse perl-image-exiftool libraw libheif",
			},
		}
	}
}

// MissingMessage describes what is missing and how to fix it, for the UI and
// for the terminal.
func (tc Toolchain) MissingMessage() string {
	if tc.Ready {
		return ""
	}
	var b strings.Builder
	b.WriteString("Hugin is not installed, or its command-line tools could not be found (missing: ")
	b.WriteString(strings.Join(tc.Missing, ", "))
	b.WriteString(").\n")
	b.WriteString("On " + tc.Install.Platform + ":\n")
	for _, step := range tc.Install.Steps {
		b.WriteString("  " + step + "\n")
	}
	b.WriteString("Downloads: " + tc.Install.URL)
	return b.String()
}
