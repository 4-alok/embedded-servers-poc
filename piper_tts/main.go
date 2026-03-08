package main

import (
	"archive/tar"
	"compress/bzip2"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// ── Config ────────────────────────────────────────────────────────────────────

const (
	piperModelsURL = "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models"
)

var dataDir string

// ── Persistent TTS engine with voice hot-swap ─────────────────────────────────

type ttsState struct {
	mu        sync.RWMutex
	engine    *sherpa.OfflineTts
	voiceName string // currently loaded voice
}

var tts = &ttsState{}

// getOrSwap returns the current TTS engine if it matches the requested voice,
// otherwise tears down the old model and loads the new one.
func (t *ttsState) getOrSwap(v *voiceEntry, numThreads int) (*sherpa.OfflineTts, error) {
	// Fast path: read lock to check if the voice is already loaded.
	t.mu.RLock()
	if t.engine != nil && t.voiceName == v.Name {
		engine := t.engine
		t.mu.RUnlock()
		return engine, nil
	}
	t.mu.RUnlock()

	// Slow path: write lock to swap the model.
	t.mu.Lock()
	defer t.mu.Unlock()

	// Double-check after acquiring write lock.
	if t.engine != nil && t.voiceName == v.Name {
		return t.engine, nil
	}

	// Tear down old engine.
	if t.engine != nil {
		log.Printf("Unloading voice %q", t.voiceName)
		sherpa.DeleteOfflineTts(t.engine)
		t.engine = nil
		t.voiceName = ""
	}

	// Create new engine for the requested voice.
	dir := modelDir(v)
	config := sherpa.OfflineTtsConfig{}
	config.Model.Vits.Model = filepath.Join(dir, v.modelFile)
	config.Model.Vits.DataDir = filepath.Join(dir, "espeak-ng-data")
	config.Model.Vits.Tokens = filepath.Join(dir, "tokens.txt")
	config.Model.Vits.LengthScale = 1.0
	config.Model.NumThreads = numThreads
	config.Model.Debug = 0
	config.Model.Provider = "cpu"
	config.MaxNumSentences = 1

	log.Printf("Loading voice %q ...", v.Name)
	engine := sherpa.NewOfflineTts(&config)
	if engine == nil {
		return nil, fmt.Errorf("failed to load voice %q — check model files in %s", v.Name, dir)
	}

	t.engine = engine
	t.voiceName = v.Name
	log.Printf("Voice %q loaded and ready!", v.Name)
	return engine, nil
}

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
	return filepath.Join(dataDir, strings.TrimSuffix(v.archive, ".tar.bz2"))
}

// ── Entry point ───────────────────────────────────────────────────────────────

var numThreadsGlobal int

func main() {
	port := flag.Int("port", 8081, "Port to listen on")
	numThreads := flag.Int("threads", runtime.NumCPU(), "Number of threads for ONNX inference")
	flag.Parse()

	numThreadsGlobal = *numThreads

	var err error
	dataDir, err = resolveDataDir()
	if err != nil {
		log.Fatalf("data dir: %v", err)
	}
	log.Printf("Data directory: %s", dataDir)

	// Download the default voice on first run so the server is immediately usable.
	if err := ensureVoice(&voices[0]); err != nil {
		log.Fatalf("default voice setup failed: %v", err)
	}

	// Pre-load the default voice into memory.
	if _, err := tts.getOrSwap(&voices[0], numThreadsGlobal); err != nil {
		log.Fatalf("failed to load default voice: %v", err)
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

// ── Synthesis (in-process with voice hot-swap) ───────────────────────────────

func synthesize(text string, v *voiceEntry, speed float64) ([]byte, error) {
	engine, err := tts.getOrSwap(v, numThreadsGlobal)
	if err != nil {
		return nil, err
	}

	speedF32 := float32(math.Max(speed, 1e-6))

	// Acquire read lock during synthesis to prevent model swap mid-inference.
	tts.mu.RLock()
	audio := engine.Generate(text, 0, speedF32)
	tts.mu.RUnlock()

	if audio == nil || len(audio.Samples) == 0 {
		return nil, fmt.Errorf("synthesis returned empty audio")
	}

	return samplesToWAV(audio.Samples, audio.SampleRate), nil
}

// samplesToWAV encodes float32 PCM samples into a WAV file (16-bit PCM) in memory.
func samplesToWAV(samples []float32, sampleRate int) []byte {
	numSamples := len(samples)
	dataSize := numSamples * 2
	fileSize := 44 + dataSize

	buf := make([]byte, fileSize)

	// RIFF header
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(fileSize-8))
	copy(buf[8:12], "WAVE")

	// fmt sub-chunk
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:24], 1) // mono
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(buf[32:34], 2)
	binary.LittleEndian.PutUint16(buf[34:36], 16)

	// data sub-chunk
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataSize))

	for i, s := range samples {
		if s > 1.0 {
			s = 1.0
		} else if s < -1.0 {
			s = -1.0
		}
		val := int16(s * 32767)
		binary.LittleEndian.PutUint16(buf[44+i*2:44+i*2+2], uint16(val))
	}

	return buf
}

// ── Download helpers ──────────────────────────────────────────────────────────

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

// ── Platform helpers ──────────────────────────────────────────────────────────

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
