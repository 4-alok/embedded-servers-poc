package main

import (
	"archive/tar"
	"compress/gzip"
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
	piperVersion  = "2023.11.14-2"
	piperBaseURL  = "https://github.com/rhasspy/piper/releases/download/" + piperVersion
	hfVoiceBase   = "https://huggingface.co/rhasspy/piper-voices/resolve/main"
	defaultVoice  = "en_US-amy-low"
	defaultVoicePath = "en/en_US/amy/low"
)

var dataDir string

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

	if err := ensurePiper(); err != nil {
		log.Fatalf("Piper setup failed: %v", err)
	}
	if err := ensureVoice(defaultVoice, defaultVoicePath); err != nil {
		log.Fatalf("Voice setup failed: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /tts", handleTTS)

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("Piper TTS server listening on http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok", "engine": "piper"})
}

func handleTTS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text  string `json:"text"`
		Voice string `json:"voice"` // optional, defaults to defaultVoice
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	voice := req.Voice
	if voice == "" {
		voice = defaultVoice
	}

	wavBytes, err := synthesize(req.Text, voice)
	if err != nil {
		log.Printf("synthesis error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "audio/wav")
	w.Write(wavBytes)
}

// ── Synthesis ─────────────────────────────────────────────────────────────────

func synthesize(text, voice string) ([]byte, error) {
	modelPath := filepath.Join(dataDir, "voices", voice+".onnx")
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("voice model not found: %s", modelPath)
	}

	tmpFile, err := os.CreateTemp("", "piper-*.wav")
	if err != nil {
		return nil, err
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	piperBin := filepath.Join(dataDir, "piper", piperBinaryName())
	cmd := exec.Command(piperBin,
		"--model", modelPath,
		"--output_file", tmpFile.Name(),
	)
	cmd.Stdin = strings.NewReader(text)
	cmd.Dir = filepath.Join(dataDir, "piper")
	// Piper needs its dylib/so next to it — set library path
	cmd.Env = append(os.Environ(),
		"DYLD_LIBRARY_PATH="+filepath.Join(dataDir, "piper"),  // macOS
		"LD_LIBRARY_PATH="+filepath.Join(dataDir, "piper"),    // Linux
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("piper error: %v\noutput: %s", err, out)
	}

	return os.ReadFile(tmpFile.Name())
}

// ── Download helpers ──────────────────────────────────────────────────────────

func ensurePiper() error {
	bin := filepath.Join(dataDir, "piper", piperBinaryName())
	if fileExists(bin) {
		log.Printf("Piper binary already present.")
		return nil
	}

	url := piperBaseURL + "/" + piperArchiveName()
	log.Printf("Downloading Piper from %s ...", url)

	archivePath := filepath.Join(dataDir, "piper.tar.gz")
	if err := downloadFile(url, archivePath); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer os.Remove(archivePath)

	log.Printf("Extracting Piper...")
	return extractTarGz(archivePath, dataDir)
}

func ensureVoice(voice, voicePath string) error {
	voiceDir := filepath.Join(dataDir, "voices")
	if err := os.MkdirAll(voiceDir, 0755); err != nil {
		return err
	}

	for _, ext := range []string{".onnx", ".onnx.json"} {
		dst := filepath.Join(voiceDir, voice+ext)
		if fileExists(dst) {
			continue
		}
		url := fmt.Sprintf("%s/%s/%s%s", hfVoiceBase, voicePath, voice, ext)
		log.Printf("Downloading voice file: %s", url)
		if err := downloadFile(url, dst); err != nil {
			return fmt.Errorf("voice download %s: %w", ext, err)
		}
	}
	log.Printf("Voice '%s' ready.", voice)
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

func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
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

// ── Platform helpers ──────────────────────────────────────────────────────────

func piperArchiveName() string {
	switch runtime.GOOS {
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "piper_macos_aarch64.tar.gz"
		}
		return "piper_macos_x64.tar.gz"
	case "windows":
		return "piper_windows_amd64.zip"
	default: // linux
		return "piper_linux_x86_64.tar.gz"
	}
}

func piperBinaryName() string {
	if runtime.GOOS == "windows" {
		return "piper.exe"
	}
	return "piper"
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
