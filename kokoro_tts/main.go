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
	kokoroModelURL = "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/kokoro-multi-lang-v1_1.tar.bz2"
)

var dataDir string

// ── Persistent TTS engine ─────────────────────────────────────────────────────

var (
	ttsEngine *sherpa.OfflineTts
	ttsMu     sync.Mutex // guards ttsEngine during synthesis
)

// ── Kokoro voice catalogue ────────────────────────────────────────────────────

type voiceEntry struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	ID          string `json:"id"`
	SID         int    `json:"-"` // passed as --sid to sherpa-onnx
}

// kokoroVoices list is auto-generated in voices.go

func voiceSID(name string) int {
	for _, v := range kokoroVoices {
		if v.Name == name {
			return v.SID
		}
	}
	return 0 // default to af
}

func voiceLang(name string) string {
	if len(name) < 1 {
		return "en"
	}
	switch name[0] {
	case 'a', 'b':
		return "en"
	case 'j':
		return "ja"
	case 'z':
		return "zh"
	case 'e':
		return "es"
	case 'f':
		return "fr"
	case 'h':
		return "hi"
	case 'i':
		return "it"
	case 'p':
		return "pt-br"
	}
	return "en"
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	port := flag.Int("port", 8082, "Port to listen on")
	numThreads := flag.Int("threads", runtime.NumCPU(), "Number of threads for ONNX inference")
	flag.Parse()

	var err error
	dataDir, err = resolveDataDir()
	if err != nil {
		log.Fatalf("data dir: %v", err)
	}
	log.Printf("Data directory: %s", dataDir)

	if err := ensureKokoroModel(); err != nil {
		log.Fatalf("Kokoro model setup failed: %v", err)
	}

	// ── Load TTS model once into memory ──────────────────────────────────
	modelDir := filepath.Join(dataDir, "kokoro-multi-lang-v1_1")

	lexicons := filepath.Join(modelDir, "lexicon-us-en.txt") + "," +
		filepath.Join(modelDir, "lexicon-gb-en.txt") + "," +
		filepath.Join(modelDir, "lexicon-zh.txt")

	config := sherpa.OfflineTtsConfig{}
	config.Model.Kokoro.Model = filepath.Join(modelDir, "model.onnx")
	config.Model.Kokoro.Voices = filepath.Join(modelDir, "voices.bin")
	config.Model.Kokoro.Tokens = filepath.Join(modelDir, "tokens.txt")
	config.Model.Kokoro.DataDir = filepath.Join(modelDir, "espeak-ng-data")
	config.Model.Kokoro.Lexicon = lexicons
	config.Model.Kokoro.LengthScale = 1.0
	config.Model.NumThreads = *numThreads
	config.Model.Debug = 0
	config.Model.Provider = "cpu"
	config.MaxNumSentences = 1

	log.Printf("Loading Kokoro model (threads=%d)...", *numThreads)
	ttsEngine = sherpa.NewOfflineTts(&config)
	if ttsEngine == nil {
		log.Fatalf("Failed to create TTS engine — check model paths")
	}
	defer sherpa.DeleteOfflineTts(ttsEngine)
	log.Println("Kokoro model loaded and ready!")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /v1/audio/voices", handleVoices)
	mux.HandleFunc("GET /v1/audio/voices/catalog", handleVoicesCatalog)
	mux.HandleFunc("POST /v1/audio/speech", handleSpeech)

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("Kokoro TTS server listening on http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok", "engine": "kokoro"})
}

// GET /v1/audio/voices — returns the catalogue of Kokoro style voices.
func handleVoices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, kokoroVoices)
}

type catalogEntry struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	ID          string `json:"id"`
	Downloaded  bool   `json:"downloaded"`
}

// GET /v1/audio/voices/catalog — all Kokoro voices ship with the single model,
// so every voice is always downloaded once the server starts.
func handleVoicesCatalog(w http.ResponseWriter, r *http.Request) {
	catalog := make([]catalogEntry, len(kokoroVoices))
	for i, v := range kokoroVoices {
		catalog[i] = catalogEntry{
			Name:        v.Name,
			DisplayName: v.DisplayName,
			ID:          v.ID,
			Downloaded:  true,
		}
	}
	writeJSON(w, catalog)
}

// POST /v1/audio/speech — OpenAI-compatible synthesis endpoint.
// Request:  {"input": "...", "voice": "af_sky", "speed": 1.0}
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

	wavBytes, err := synthesize(req.Input, req.Voice, req.Speed)
	if err != nil {
		log.Printf("synthesis error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "audio/wav")
	w.Write(wavBytes)
}

// ── Synthesis (in-process) ───────────────────────────────────────────────────

func synthesize(text, voiceName string, speed float64) ([]byte, error) {
	sid := voiceSID(voiceName)
	speedF32 := float32(math.Max(speed, 1e-6))

	ttsMu.Lock()
	audio := ttsEngine.Generate(text, sid, speedF32)
	ttsMu.Unlock()

	if audio == nil || len(audio.Samples) == 0 {
		return nil, fmt.Errorf("synthesis returned empty audio")
	}

	return samplesToWAV(audio.Samples, audio.SampleRate), nil
}

// samplesToWAV encodes float32 PCM samples into a WAV file (16-bit PCM) in memory.
func samplesToWAV(samples []float32, sampleRate int) []byte {
	numSamples := len(samples)
	dataSize := numSamples * 2 // 16-bit = 2 bytes per sample
	fileSize := 44 + dataSize  // 44-byte WAV header

	buf := make([]byte, fileSize)

	// RIFF header
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(fileSize-8))
	copy(buf[8:12], "WAVE")

	// fmt sub-chunk
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16) // PCM chunk size
	binary.LittleEndian.PutUint16(buf[20:22], 1)  // PCM format
	binary.LittleEndian.PutUint16(buf[22:24], 1)  // mono
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(sampleRate*2)) // byte rate
	binary.LittleEndian.PutUint16(buf[32:34], 2)                   // block align
	binary.LittleEndian.PutUint16(buf[34:36], 16)                  // bits per sample

	// data sub-chunk
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataSize))

	// Convert float32 [-1,1] → int16
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

func ensureKokoroModel() error {
	modelDir := filepath.Join(dataDir, "kokoro-multi-lang-v1_1")
	if fileExists(filepath.Join(modelDir, "model.onnx")) {
		log.Printf("Kokoro model already present.")
		return nil
	}

	archivePath := filepath.Join(dataDir, "kokoro-multi-lang-v1_1.tar.bz2")
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

// ── Platform helpers ──────────────────────────────────────────────────────────

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
