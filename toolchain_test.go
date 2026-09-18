package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeTool writes an executable stub into dir.
func fakeTool(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The whole point of the search: a tool that is not on PATH is still found in
// a directory the user named, which is how a normal macOS or Windows install
// has to work.
func TestFindToolLooksBeyondPATH(t *testing.T) {
	dir := t.TempDir()
	want := fakeTool(t, dir, "pto_gen_testonly")

	if _, ok := findTool("pto_gen_testonly", nil); ok {
		t.Fatal("the stub must not already be on PATH, or this proves nothing")
	}
	got, ok := findTool("pto_gen_testonly", []string{dir})
	if !ok {
		t.Fatal("findTool did not look in the supplied directory")
	}
	if got != want {
		t.Errorf("findTool = %q, want %q", got, want)
	}
}

func TestFindToolIgnoresDirectoriesAndMissingEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "cpfind_testonly"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := findTool("cpfind_testonly", []string{dir, "", "/nonexistent"}); ok {
		t.Error("a directory named like the tool must not count as the tool")
	}
}

// Tools found off PATH have to be run by their full path, or exec would fail
// to find them at stitch time.
func TestToolchainPathPrefersTheResolvedLocation(t *testing.T) {
	dir := t.TempDir()
	want := fakeTool(t, dir, "nona_testonly")

	tc := Toolchain{paths: map[string]string{"nona_testonly": want}}
	if got := tc.Path("nona_testonly"); got != want {
		t.Errorf("Path = %q, want the resolved location %q", got, want)
	}
	// An unknown tool falls back to the bare name so exec can search PATH.
	if got := tc.Path("unknown"); got != "unknown" {
		t.Errorf("Path = %q, want the bare name", got)
	}
}

func TestDetectToolchainSearchesExtraDirsFirst(t *testing.T) {
	dir := t.TempDir()
	tc := DetectToolchain(dir)

	found := false
	for _, d := range tc.Searched {
		if d == dir {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Searched = %v, want it to include the supplied directory", tc.Searched)
	}
	if len(tc.Searched) < 2 {
		t.Error("the platform's own install locations should be searched too")
	}
}

// The advice has to match the machine the user is on; telling a Mac user to
// run apt is worse than saying nothing.
func TestInstallHelpMatchesThePlatform(t *testing.T) {
	help := installHelp()
	if len(help.Steps) == 0 || help.URL == "" {
		t.Fatalf("install help is incomplete: %+v", help)
	}

	wantPlatform := map[string]string{
		"darwin": "macOS", "windows": "Windows", "linux": "Linux",
	}[runtime.GOOS]
	if wantPlatform != "" && help.Platform != wantPlatform {
		t.Errorf("platform = %q, want %q", help.Platform, wantPlatform)
	}

	joined := strings.Join(help.Steps, "\n")
	switch runtime.GOOS {
	case "darwin":
		if !strings.Contains(joined, "brew") {
			t.Errorf("macOS help should mention Homebrew, got %q", joined)
		}
		if strings.Contains(joined, "apt install") {
			t.Errorf("macOS help must not tell the user to run apt: %q", joined)
		}
	case "windows":
		if !strings.Contains(strings.ToLower(joined), "msi") {
			t.Errorf("Windows help should mention the installer, got %q", joined)
		}
		if strings.Contains(joined, "apt install") {
			t.Errorf("Windows help must not tell the user to run apt: %q", joined)
		}
	case "linux":
		if !strings.Contains(joined, "apt install") {
			t.Errorf("Linux help should cover Debian/Ubuntu, got %q", joined)
		}
	}
}

// The failure message a user actually sees must name what is missing and how
// to fix it on their own system.
func TestMissingMessageIsActionable(t *testing.T) {
	tc := Toolchain{
		Missing: []string{"cpfind", "enblend"},
		Install: installHelp(),
	}
	msg := tc.MissingMessage()
	for _, want := range []string{"cpfind", "enblend", tc.Install.Platform, tc.Install.URL} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q:\n%s", want, msg)
		}
	}

	if ready := (Toolchain{Ready: true}).MissingMessage(); ready != "" {
		t.Errorf("a ready toolchain should have no message, got %q", ready)
	}
}

// platformDirs must name real candidate locations, not empty strings.
func TestPlatformDirsAreConcrete(t *testing.T) {
	dirs := platformDirs()
	if len(dirs) == 0 {
		t.Fatal("no candidate directories for this platform")
	}
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			t.Error("platformDirs produced an empty entry")
		}
		if !filepath.IsAbs(dir) {
			t.Errorf("candidate %q is not absolute", dir)
		}
	}
}

func TestHuginDirsParsing(t *testing.T) {
	t.Setenv("HUGIN_DIR", "")
	if got := huginDirs(""); got != nil {
		t.Errorf("no flag and no env should give nothing, got %v", got)
	}

	joined := strings.Join([]string{"/one", "/two"}, string(filepath.ListSeparator))
	got := huginDirs(joined)
	if len(got) != 2 || got[0] != "/one" || got[1] != "/two" {
		t.Errorf("huginDirs(%q) = %v, want both entries", joined, got)
	}

	t.Setenv("HUGIN_DIR", "/from/env")
	if got := huginDirs(""); len(got) != 1 || got[0] != "/from/env" {
		t.Errorf("huginDirs should fall back to HUGIN_DIR, got %v", got)
	}
	// The flag wins over the environment.
	if got := huginDirs("/from/flag"); len(got) != 1 || got[0] != "/from/flag" {
		t.Errorf("the flag should take precedence, got %v", got)
	}
}

// /health has to carry everything the banner needs.
func TestHealthCarriesInstallHelp(t *testing.T) {
	tc := DetectToolchain()
	if tc.Install.Platform == "" || len(tc.Install.Steps) == 0 {
		t.Errorf("install help missing from the report: %+v", tc.Install)
	}
}

// Tools that launch other tools search PATH themselves, so the directories we
// found the tools in have to be handed down. Without this, a Hugin that is not
// on PATH remaps fine and then dies with "execvp(enblend) failed".
func TestEnvPutsToolDirsOnPath(t *testing.T) {
	dir := t.TempDir()
	tc := Toolchain{paths: map[string]string{
		"nona":    filepath.Join(dir, "nona"),
		"enblend": filepath.Join(dir, "enblend"),
	}}

	var gotPath string
	var gotLocale bool
	for _, entry := range tc.Env() {
		if strings.HasPrefix(entry, "PATH=") {
			gotPath = strings.TrimPrefix(entry, "PATH=")
		}
		if entry == "LC_ALL=C" {
			gotLocale = true
		}
	}

	if !gotLocale {
		t.Error("LC_ALL=C is missing, so tool output may not parse")
	}
	if gotPath == "" {
		t.Fatal("Env did not set PATH")
	}
	first := strings.Split(gotPath, string(filepath.ListSeparator))[0]
	if first != dir {
		t.Errorf("PATH starts with %q, want the tool directory %q first", first, dir)
	}
	if existing := os.Getenv("PATH"); existing != "" && !strings.Contains(gotPath, existing) {
		t.Error("Env dropped the inherited PATH instead of prepending to it")
	}
}

// One directory holding every tool must not be repeated on PATH.
func TestToolDirsAreDeduplicated(t *testing.T) {
	dir := t.TempDir()
	tc := Toolchain{paths: map[string]string{
		"nona":     filepath.Join(dir, "nona"),
		"enblend":  filepath.Join(dir, "enblend"),
		"cpfind":   filepath.Join(dir, "cpfind"),
		"exiftool": filepath.Join(dir, "exiftool"),
	}}
	if got := tc.ToolDirs(); len(got) != 1 || got[0] != dir {
		t.Errorf("ToolDirs = %v, want exactly [%s]", got, dir)
	}
}

// With nothing resolved there is no PATH to prepend, and the inherited
// environment must be left alone.
func TestEnvWithoutResolvedToolsKeepsInheritedPath(t *testing.T) {
	tc := Toolchain{}
	if got := tc.ToolDirs(); len(got) != 0 {
		t.Errorf("ToolDirs = %v, want none", got)
	}
	for _, entry := range tc.Env() {
		if strings.HasPrefix(entry, "PATH=") && entry != "PATH="+os.Getenv("PATH") {
			t.Errorf("Env rewrote PATH to %q with nothing to add", entry)
		}
	}
}
