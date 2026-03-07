package main

import (
	"archive/tar"
	"compress/bzip2"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ── Config ────────────────────────────────────────────────────────────────────
// Uses sherpa-onnx binary (bundles all dylibs — no system dependency) to run
// VITS/piper voice models downloaded from the sherpa-onnx tts-models release.

const (
	sherpaVersion  = "1.12.28"
	sherpaBaseURL  = "https://github.com/k2-fsa/sherpa-onnx/releases/download/v" + sherpaVersion
	piperModelsURL = "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models"
)

var dataDir string

// ── Voice catalogue ───────────────────────────────────────────────────────────

type voiceEntry struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	ID          string `json:"id"`
	archive     string // tar.bz2 name in piperModelsURL
	modelFile   string // .onnx filename inside the archive dir
}

// Voices are loaded from voices.go

func findVoice(name string) *voiceEntry {
	for i, v := range voices {
		if v.Name == name {
			return &voices[i]
		}
	}
	return &voices[0] // default: first voice
}

func modelDir(v *voiceEntry) string {
	// archive "vits-piper-en_US-amy-low.tar.bz2" → dir "vits-piper-en_US-amy-low"
	return filepath.Join(dataDir, strings.TrimSuffix(v.archive, ".tar.bz2"))
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	port := flag.Int("port", 8081, "Port to listen on")
	flag.Parse()

	var err error
	dataDir, err = resolveDataDir()
	if err != nil {
		log.Fatalf("data dir: %v", err)
	}
	log.Printf("Data directory: %s", dataDir)

	if err := ensureSherpaOnnx(); err != nil {
		log.Fatalf("sherpa-onnx setup failed: %v", err)
	}
	// Download the default voice on first run so the server is immediately usable.
	if err := ensureVoice(&voices[0]); err != nil {
		log.Fatalf("default voice setup failed: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /v1/audio/voices", handleVoices)
	mux.HandleFunc("GET /v1/audio/voices/catalog", handleVoicesCatalog)
	mux.HandleFunc("POST /v1/audio/voices/download", handleVoiceDownload)
	mux.HandleFunc("POST /v1/audio/speech", handleSpeech)

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("Piper TTS server listening on http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok", "engine": "piper-vits"})
}

// GET /v1/audio/voices — returns only downloaded voices.
func handleVoices(w http.ResponseWriter, r *http.Request) {
	var available []voiceEntry
	for i, v := range voices {
		if fileExists(filepath.Join(modelDir(&voices[i]), v.modelFile)) {
			available = append(available, v)
		}
	}
	if available == nil {
		available = []voiceEntry{}
	}
	writeJSON(w, available)
}

type catalogEntry struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	ID          string `json:"id"`
	Downloaded  bool   `json:"downloaded"`
}

// GET /v1/audio/voices/catalog — returns all known voices with download status.
func handleVoicesCatalog(w http.ResponseWriter, r *http.Request) {
	catalog := make([]catalogEntry, len(voices))
	for i, v := range voices {
		catalog[i] = catalogEntry{
			Name:        v.Name,
			DisplayName: v.DisplayName,
			ID:          v.ID,
			Downloaded:  fileExists(filepath.Join(modelDir(&voices[i]), v.modelFile)),
		}
	}
	writeJSON(w, catalog)
}

// POST /v1/audio/voices/download — downloads a voice model synchronously.
// Request:  {"voice": "en_US-libritts-high"}
// Response: {"status": "ok", "voice": "en_US-libritts-high"}
func handleVoiceDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Voice string `json:"voice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	var target *voiceEntry
	for i := range voices {
		if voices[i].Name == req.Voice {
			target = &voices[i]
			break
		}
	}
	if target == nil {
		http.Error(w, "unknown voice: "+req.Voice, http.StatusBadRequest)
		return
	}
	if err := ensureVoice(target); err != nil {
		http.Error(w, "download failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "voice": target.Name})
}

// POST /v1/audio/speech — OpenAI-compatible synthesis.
// Request:  {"input": "...", "voice": "en_US-amy-low", "speed": 1.0}
// Response: audio/wav bytes
func handleSpeech(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input string  `json:"input"`
		Voice string  `json:"voice"`
		Speed float64 `json:"speed"`
	}
	req.Speed = 1.0
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		http.Error(w, "input is required", http.StatusBadRequest)
		return
	}

	v := findVoice(req.Voice)

	// Lazy-download requested voice if not present
	if !fileExists(filepath.Join(modelDir(v), v.modelFile)) {
		log.Printf("Downloading voice %s on demand…", v.Name)
		if err := ensureVoice(v); err != nil {
			http.Error(w, "voice download failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	wavBytes, err := synthesize(req.Input, v, req.Speed)
	if err != nil {
		log.Printf("synthesis error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "audio/wav")
	w.Write(wavBytes)
}

// ── Synthesis ─────────────────────────────────────────────────────────────────

func synthesize(text string, v *voiceEntry, speed float64) ([]byte, error) {
	dir := modelDir(v)

	tmpFile, err := os.CreateTemp("", "piper-*.wav")
	if err != nil {
		return nil, err
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	bin := filepath.Join(dataDir, "sherpa-onnx", sherpaBinaryName())
	sherpaDir := filepath.Join(dataDir, "sherpa-onnx")

	args := []string{
		"--vits-model=" + filepath.Join(dir, v.modelFile),
		"--vits-data-dir=" + filepath.Join(dir, "espeak-ng-data"),
		"--vits-tokens=" + filepath.Join(dir, "tokens.txt"),
		fmt.Sprintf("--vits-length-scale=%.2f", 1.0/speed), // VITS uses --vits-length-scale, not --length-scale
		"--output-filename=" + tmpFile.Name(),
		text,
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = sherpaDir
	cmd.Env = append(os.Environ(),
		"DYLD_LIBRARY_PATH="+sherpaDir,
		"LD_LIBRARY_PATH="+sherpaDir,
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sherpa-onnx error: %v\n%s", err, out)
	}

	return os.ReadFile(tmpFile.Name())
}

// ── Download helpers ──────────────────────────────────────────────────────────

func ensureSherpaOnnx() error {
	bin := filepath.Join(dataDir, "sherpa-onnx", sherpaBinaryName())
	if fileExists(bin) {
		log.Printf("sherpa-onnx binary already present.")
		return nil
	}

	archiveName := sherpaArchiveName()
	url := sherpaBaseURL + "/" + archiveName
	archivePath := filepath.Join(dataDir, archiveName)

	log.Printf("Downloading sherpa-onnx from %s ...", url)
	if err := downloadFile(url, archivePath); err != nil {
		return fmt.Errorf("download sherpa-onnx: %w", err)
	}
	defer os.Remove(archivePath)

	log.Printf("Extracting sherpa-onnx...")
	destDir := filepath.Join(dataDir, "sherpa-onnx-extract")
	if err := extractTarBz2(archivePath, destDir); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	entries, _ := os.ReadDir(destDir)
	if len(entries) == 0 {
		return fmt.Errorf("extraction produced no files")
	}
	topDir := filepath.Join(destDir, entries[0].Name())
	target := filepath.Join(dataDir, "sherpa-onnx")
	os.MkdirAll(target, 0755)

	srcBin := filepath.Join(topDir, "bin", sherpaBinaryName())
	if err := copyFile(srcBin, bin); err != nil {
		srcBin = filepath.Join(topDir, sherpaBinaryName())
		if err2 := copyFile(srcBin, bin); err2 != nil {
			return fmt.Errorf("copy binary: %v / %v", err, err2)
		}
	}
	os.Chmod(bin, 0755)

	for _, ext := range []string{".dylib", ".so"} {
		copyGlob(filepath.Join(topDir, "lib"), target, "*"+ext)
		copyGlob(topDir, target, "*"+ext)
	}
	copyGlob(filepath.Join(topDir, "bin"), target, "*.dll")

	os.RemoveAll(destDir)
	log.Printf("sherpa-onnx ready.")
	return nil
}

func ensureVoice(v *voiceEntry) error {
	dir := modelDir(v)
	if fileExists(filepath.Join(dir, v.modelFile)) {
		log.Printf("Voice %q already present.", v.Name)
		return nil
	}

	url := piperModelsURL + "/" + v.archive
	archivePath := filepath.Join(dataDir, v.archive)
	log.Printf("Downloading voice %q from %s ...", v.Name, url)
	if err := downloadFile(url, archivePath); err != nil {
		return fmt.Errorf("download voice: %w", err)
	}
	defer os.Remove(archivePath)

	log.Printf("Extracting voice %q ...", v.Name)
	return extractTarBz2(archivePath, dataDir)
}

// ── Archive / file helpers ────────────────────────────────────────────────────

func extractTarBz2(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	tr := tar.NewReader(bzip2.NewReader(f))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, filepath.Clean(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, hdr.FileInfo().Mode())
		case tar.TypeReg, tar.TypeRegA:
			os.MkdirAll(filepath.Dir(target), 0755)
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode())
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			out.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func downloadFile(url, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func copyGlob(srcDir, dstDir, pattern string) {
	matches, _ := filepath.Glob(filepath.Join(srcDir, pattern))
	for _, m := range matches {
		copyFile(m, filepath.Join(dstDir, filepath.Base(m)))
	}
}

// ── Platform helpers ──────────────────────────────────────────────────────────

func sherpaArchiveName() string {
	switch runtime.GOOS {
	case "darwin":
		return fmt.Sprintf("sherpa-onnx-v%s-osx-universal2-shared.tar.bz2", sherpaVersion)
	case "windows":
		return fmt.Sprintf("sherpa-onnx-v%s-win-x64-shared-MD-Release.tar.bz2", sherpaVersion)
	default:
		return fmt.Sprintf("sherpa-onnx-v%s-linux-x86_64-shared.tar.bz2", sherpaVersion)
	}
}

func sherpaBinaryName() string {
	if runtime.GOOS == "windows" {
		return "sherpa-onnx-offline-tts.exe"
	}
	return "sherpa-onnx-offline-tts"
}

func resolveDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	var base string
	switch runtime.GOOS {
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support", "piper_tts")
	case "windows":
		base = filepath.Join(os.Getenv("APPDATA"), "piper_tts")
	default:
		base = filepath.Join(home, ".local", "share", "piper_tts")
	}
	return base, os.MkdirAll(base, 0755)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
