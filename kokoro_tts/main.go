package main

import (
	"compress/bzip2"
	"archive/tar"
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

const (
	sherpaVersion = "1.12.28"
	sherpaBaseURL = "https://github.com/k2-fsa/sherpa-onnx/releases/download/v" + sherpaVersion
	kokoroModelURL = "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/kokoro-en-v0_19.tar.bz2"
)

var dataDir string

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	port := flag.Int("port", 8082, "Port to listen on")
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
	if err := ensureKokoroModel(); err != nil {
		log.Fatalf("Kokoro model setup failed: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /tts", handleTTS)

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("Kokoro TTS server listening on http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok", "engine": "kokoro"})
}

func handleTTS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text  string  `json:"text"`
		Speed float64 `json:"speed"` // optional, default 1.0
	}
	req.Speed = 1.0
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

	wavBytes, err := synthesize(req.Text, req.Speed)
	if err != nil {
		log.Printf("synthesis error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "audio/wav")
	w.Write(wavBytes)
}

// ── Synthesis ─────────────────────────────────────────────────────────────────

func synthesize(text string, speed float64) ([]byte, error) {
	modelDir := filepath.Join(dataDir, "kokoro-en-v0_19")

	tmpFile, err := os.CreateTemp("", "kokoro-*.wav")
	if err != nil {
		return nil, err
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	bin := filepath.Join(dataDir, "sherpa-onnx", sherpaBinaryName())

	cmd := exec.Command(bin,
		"--kokoro-model="+filepath.Join(modelDir, "model.onnx"),
		"--kokoro-voices="+filepath.Join(modelDir, "voices.bin"),
		"--kokoro-tokens="+filepath.Join(modelDir, "tokens.txt"),
		"--kokoro-data-dir="+filepath.Join(modelDir, "espeak-ng-data"),
		fmt.Sprintf("--length-scale=%.2f", 1.0/speed),
		"--output-filename="+tmpFile.Name(),
		text,
	)
	cmd.Dir = filepath.Join(dataDir, "sherpa-onnx")
	cmd.Env = append(os.Environ(),
		"DYLD_LIBRARY_PATH="+filepath.Join(dataDir, "sherpa-onnx"),
		"LD_LIBRARY_PATH="+filepath.Join(dataDir, "sherpa-onnx"),
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

	// The archive has a versioned subdirectory — find and move bin/ and lib files
	entries, _ := os.ReadDir(destDir)
	if len(entries) == 0 {
		return fmt.Errorf("extraction produced no files")
	}
	topDir := filepath.Join(destDir, entries[0].Name())

	target := filepath.Join(dataDir, "sherpa-onnx")
	os.MkdirAll(target, 0755)

	// Copy the TTS binary
	srcBin := filepath.Join(topDir, "bin", sherpaBinaryName())
	if err := copyFile(srcBin, bin); err != nil {
		// try bin/ without subdir
		srcBin = filepath.Join(topDir, sherpaBinaryName())
		if err2 := copyFile(srcBin, bin); err2 != nil {
			return fmt.Errorf("copy binary: %v / %v", err, err2)
		}
	}
	os.Chmod(bin, 0755)

	// Copy shared libraries
	for _, ext := range []string{".dylib", ".so"} {
		copyGlob(filepath.Join(topDir, "lib"), target, "*"+ext)
		copyGlob(topDir, target, "*"+ext)
	}
	copyGlob(filepath.Join(topDir, "bin"), target, "*.dll")

	os.RemoveAll(destDir)
	log.Printf("sherpa-onnx ready.")
	return nil
}

func ensureKokoroModel() error {
	modelDir := filepath.Join(dataDir, "kokoro-en-v0_19")
	if fileExists(filepath.Join(modelDir, "model.onnx")) {
		log.Printf("Kokoro model already present.")
		return nil
	}

	archivePath := filepath.Join(dataDir, "kokoro-en-v0_19.tar.bz2")
	log.Printf("Downloading Kokoro model from %s ...", kokoroModelURL)
	if err := downloadFile(kokoroModelURL, archivePath); err != nil {
		return fmt.Errorf("download kokoro model: %w", err)
	}
	defer os.Remove(archivePath)

	log.Printf("Extracting Kokoro model...")
	return extractTarBz2(archivePath, dataDir)
}

// ── Archive helpers ───────────────────────────────────────────────────────────

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
		dst := filepath.Join(dstDir, filepath.Base(m))
		copyFile(m, dst)
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
		base = filepath.Join(home, "Library", "Application Support", "kokoro_tts")
	case "windows":
		base = filepath.Join(os.Getenv("APPDATA"), "kokoro_tts")
	default:
		base = filepath.Join(home, ".local", "share", "kokoro_tts")
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
