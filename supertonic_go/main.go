package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// modelReadyCh is closed once the model assets are downloaded, extracted, and loaded.
var modelReadyCh = make(chan struct{})

// ── Config ────────────────────────────────────────────────────────────────────

const (
	supertonicModelURL = "https://storage.googleapis.com/adsorp-public-assets/models/supertonic-3-onnx.tar.gz"
)

var (
	dataDir      string
	textToSpeech *TextToSpeech
	ttsMu        sync.Mutex // guards synthesis execution to prevent concurrent ONNX calls
)

// ── Voice Catalog ─────────────────────────────────────────────────────────────

type voiceEntry struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	ID          string `json:"id"`
}

type catalogEntry struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	ID          string `json:"id"`
	Downloaded  bool   `json:"downloaded"`
}

var languages = []struct {
	Code string
	Name string
}{
	{"en", "English"},
	{"ko", "Korean"},
	{"ja", "Japanese"},
	{"ar", "Arabic"},
	{"bg", "Bulgarian"},
	{"cs", "Czech"},
	{"da", "Danish"},
	{"de", "German"},
	{"el", "Greek"},
	{"es", "Spanish"},
	{"et", "Estonian"},
	{"fi", "Finnish"},
	{"fr", "French"},
	{"hi", "Hindi"},
	{"hr", "Croatian"},
	{"hu", "Hungarian"},
	{"id", "Indonesian"},
	{"it", "Italian"},
	{"lt", "Lithuanian"},
	{"lv", "Latvian"},
	{"nl", "Dutch"},
	{"pl", "Polish"},
	{"pt", "Portuguese"},
	{"ro", "Romanian"},
	{"ru", "Russian"},
	{"sk", "Slovak"},
	{"sl", "Slovenian"},
	{"sv", "Swedish"},
	{"tr", "Turkish"},
	{"uk", "Ukrainian"},
	{"vi", "Vietnamese"},
	{"na", "Language-Agnostic"},
}

var styles = []struct {
	Code string
	Name string
}{
	{"F1", "Female 1"},
	{"F2", "Female 2"},
	{"F3", "Female 3"},
	{"F4", "Female 4"},
	{"F5", "Female 5"},
	{"M1", "Male 1"},
	{"M2", "Male 2"},
	{"M3", "Male 3"},
	{"M4", "Male 4"},
	{"M5", "Male 5"},
}

var supertonicVoices []voiceEntry
var cachedStyles = make(map[string]*Style)

func initVoices() {
	supertonicVoices = make([]voiceEntry, 0, len(languages)*len(styles))
	for _, lang := range languages {
		for _, style := range styles {
			id := lang.Code + "_" + style.Code
			displayName := fmt.Sprintf("%s (%s) — Supertonic", lang.Name, style.Name)
			supertonicVoices = append(supertonicVoices, voiceEntry{
				Name:        id,
				DisplayName: displayName,
				ID:          id,
			})
		}
	}
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	port := flag.Int("port", 8083, "Port to listen on")
	numThreads := flag.Int("threads", runtime.NumCPU(), "Number of threads for ONNX inference")
	ipc := flag.Bool("ipc", false, "Use stdin/stdout for IPC instead of HTTP")
	flag.Parse()

	if *ipc {
		// Redirect all logs to Stderr to keep Stdout clean for the IPC protocol.
		log.SetOutput(os.Stderr)
	}

	initVoices()

	var err error
	dataDir, err = resolveDataDir()
	if err != nil {
		log.Fatalf("data dir: %v", err)
	}
	log.Printf("Data directory: %s", dataDir)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /v1/audio/voices", handleVoices)
	mux.HandleFunc("GET /v1/audio/voices/catalog", handleVoicesCatalog)
	mux.HandleFunc("POST /v1/audio/speech", handleSpeech)

	// Start HTTP server immediately so Flutter's health-check succeeds
	// right away (green status) rather than waiting for model download.
	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	go func() {
		log.Printf("Supertonic TTS server listening on http://%s", addr)
		log.Fatal(http.ListenAndServe(addr, mux))
	}()

	// Download + load model (may take several minutes on first run).
	if err := ensureSupertonicModel(); err != nil {
		log.Fatalf("Supertonic model setup failed: %v", err)
	}

	// ── Initialize ONNX Runtime & Load Models ────────────────────────────
	log.Printf("STAGE: LOAD")
	log.Printf("Initializing ONNX Runtime...")
	if err := InitializeONNXRuntime(); err != nil {
		log.Fatalf("ONNX Runtime initialization failed: %v", err)
	}

	onnxDir := filepath.Join(dataDir, "onnx")
	log.Printf("Loading configuration from %s...", onnxDir)
	cfg, err := LoadCfgs(onnxDir)
	if err != nil {
		log.Fatalf("Failed to load configs: %v", err)
	}

	log.Printf("Loading model sessions (threads=%d)...", *numThreads)
	// We pass CPU provider for our local inference
	textToSpeech, err = LoadTextToSpeech(onnxDir, false, cfg)
	if err != nil {
		log.Fatalf("Failed to load TextToSpeech engine: %v", err)
	}

	log.Printf("Pre-loading all 10 voice styles...")
	for _, style := range styles {
		stylePath := filepath.Join(dataDir, "voice_styles", style.Code+".json")
		log.Printf("  Loading %s.json...", style.Code)
		st, err := LoadVoiceStyle([]string{stylePath}, false)
		if err != nil {
			log.Fatalf("Failed to load style %s: %v", style.Code, err)
		}
		cachedStyles[style.Code] = st
	}

	log.Println("Supertonic model loaded and ready!")
	close(modelReadyCh)

	if *ipc {
		handleIPC()
		return
	}

	// Keep main goroutine alive (HTTP server runs in goroutine above).
	select {}
}

// ── IPC Handler ──────────────────────────────────────────────────────────────

type ipcRequest struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type ipcResponse struct {
	ID    int    `json:"id"`
	Error string `json:"error,omitempty"`
}

func handleIPC() {
	log.Println("IPC mode enabled. Listening on Stdin...")

	reader := bufio.NewReader(os.Stdin)
	header := make([]byte, 8)

	for {
		// Read 8-byte header: [4 bytes Total Payload Length][4 bytes JSON Length]
		_, err := io.ReadFull(reader, header)
		if err != nil {
			if err != io.EOF {
				log.Printf("IPC: header read error: %v", err)
			}
			return
		}

		totalLen := binary.BigEndian.Uint32(header[0:4])
		jsonLen := binary.BigEndian.Uint32(header[4:8])

		if totalLen < jsonLen {
			log.Printf("IPC: invalid lengths (total=%d, json=%d)", totalLen, jsonLen)
			return
		}

		// Read the full payload
		payload := make([]byte, totalLen)
		_, err = io.ReadFull(reader, payload)
		if err != nil {
			log.Printf("IPC: payload read error: %v", err)
			return
		}

		// Process request
		var req ipcRequest
		if err := json.Unmarshal(payload[:jsonLen], &req); err != nil {
			log.Printf("IPC: json unmarshal error: %v", err)
			continue
		}

		go handleIPCRequest(req)
	}
}

func handleIPCRequest(req ipcRequest) {
	log.Printf("IPC: Handling request method: '%s' (ID: %d)", req.Method, req.ID)
	switch req.Method {
	case "health":
		sendIPCResponse(req.ID, map[string]string{"status": "ok", "engine": "supertonic"}, nil)

	case "voices":
		select {
		case <-modelReadyCh:
			sendIPCResponse(req.ID, supertonicVoices, nil)
		default:
			sendIPCResponse(req.ID, []voiceEntry{}, nil)
		}

	case "catalog":
		log.Println("IPC: Handling catalog request")
		catalog := make([]catalogEntry, len(supertonicVoices))
		for i, v := range supertonicVoices {
			catalog[i] = catalogEntry{
				Name:        v.Name,
				DisplayName: v.DisplayName,
				ID:          v.ID,
				Downloaded:  true, // Models are unified in a single tar.gz download
			}
		}
		sendIPCResponse(req.ID, catalog, nil)

	case "speech":
		select {
		case <-modelReadyCh:
			var params struct {
				Input string  `json:"input"`
				Voice string  `json:"voice"`
				Speed float64 `json:"speed"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				sendIPCError(req.ID, "invalid params: "+err.Error())
				return
			}
			if params.Speed == 0 {
				params.Speed = 1.05
			}

			wavBytes, err := synthesize(params.Input, params.Voice, params.Speed)
			if err != nil {
				sendIPCError(req.ID, "synthesis error: "+err.Error())
				return
			}
			// Send response with binary WAV data
			sendIPCResponse(req.ID, map[string]bool{"success": true}, wavBytes)

		default:
			sendIPCError(req.ID, "model not ready")
		}

	default:
		sendIPCError(req.ID, "unknown method: "+req.Method)
	}
}

func sendIPCError(id int, message string) {
	sendIPCResponse(id, map[string]string{"error": message}, nil)
}

// stdoutMu guards os.Stdout to ensure frames don't overlap.
var stdoutMu sync.Mutex

func sendIPCResponse(id int, result any, binaryPart []byte) {
	resp := struct {
		ID     int `json:"id"`
		Result any `json:"result"`
	}{
		ID:     id,
		Result: result,
	}

	jsonBytes, err := json.Marshal(resp)
	if err != nil {
		log.Printf("IPC: marshal response error: %v", err)
		return
	}

	totalLen := uint32(len(jsonBytes) + len(binaryPart))
	jsonLen := uint32(len(jsonBytes))

	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header[0:4], totalLen)
	binary.BigEndian.PutUint32(header[4:8], jsonLen)

	stdoutMu.Lock()
	defer stdoutMu.Unlock()

	os.Stdout.Write(header)
	os.Stdout.Write(jsonBytes)
	if len(binaryPart) > 0 {
		os.Stdout.Write(binaryPart)
	}
}

// ── HTTP Handlers ─────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok", "engine": "supertonic"})
}

// GET /v1/audio/voices — returns the catalogue of Supertonic voices.
func handleVoices(w http.ResponseWriter, r *http.Request) {
	select {
	case <-modelReadyCh:
	default:
		writeJSON(w, []voiceEntry{})
		return
	}
	writeJSON(w, supertonicVoices)
}

// GET /v1/audio/voices/catalog — returns all known voices with download status.
func handleVoicesCatalog(w http.ResponseWriter, r *http.Request) {
	catalog := make([]catalogEntry, len(supertonicVoices))
	for i, v := range supertonicVoices {
		catalog[i] = catalogEntry{
			Name:        v.Name,
			DisplayName: v.DisplayName,
			ID:          v.ID,
			Downloaded:  true,
		}
	}
	writeJSON(w, catalog)
}

// POST /v1/audio/speech — OpenAI-compatible synthesis.
func handleSpeech(w http.ResponseWriter, r *http.Request) {
	select {
	case <-modelReadyCh:
	default:
		http.Error(w, "model not ready yet, please wait", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Input string  `json:"input"`
		Voice string  `json:"voice"`
		Speed float64 `json:"speed"`
	}
	req.Speed = 1.05
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

// ── Synthesis Pipeline ────────────────────────────────────────────────────────

func synthesize(text string, voiceName string, speed float64) ([]byte, error) {
	// Parse composite voice ID (e.g. "ko_F1" -> lang="ko", styleCode="F1")
	lang := "en"
	styleCode := "M1"
	parts := strings.Split(voiceName, "_")
	if len(parts) == 2 {
		lang = parts[0]
		styleCode = parts[1]
	} else if len(parts) == 1 && parts[0] != "" {
		// Fallback for simple voice name or legacy setting
		styleCode = parts[0]
	}

	// Validate styleCode matches cached style
	style, found := cachedStyles[styleCode]
	if !found {
		log.Printf("Warning: requested voice style %q not found, defaulting to M1", styleCode)
		style = cachedStyles["M1"]
	}

	speedF32 := float32(speed)
	if speedF32 <= 0 {
		speedF32 = 1.05
	}

	// Guard synthesis execution to avoid concurrent ONNX runtime crashes
	ttsMu.Lock()
	samples, _, err := textToSpeech.Call(text, lang, style, 8, speedF32, 0.3)
	ttsMu.Unlock()

	if err != nil {
		return nil, fmt.Errorf("synthesis failed: %w", err)
	}

	if len(samples) == 0 {
		return nil, fmt.Errorf("synthesis returned empty samples")
	}

	return samplesToWAV(samples, textToSpeech.SampleRate), nil
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

func ensureSupertonicModel() error {
	onnxDir := filepath.Join(dataDir, "onnx")
	// Check for a file that is extracted late to ensure extraction completed.
	if fileExists(filepath.Join(onnxDir, "vocoder.onnx")) && fileExists(filepath.Join(dataDir, "voice_styles", "M1.json")) {
		log.Printf("Supertonic model already present.")
		return nil
	}

	archivePath := filepath.Join(dataDir, "supertonic-3-onnx.tar.gz")
	log.Printf("Downloading Supertonic model from %s ...", supertonicModelURL)
	if err := downloadFile(supertonicModelURL, archivePath); err != nil {
		return fmt.Errorf("download supertonic model: %w", err)
	}
	defer os.Remove(archivePath)

	log.Printf("STAGE: EXTRACT")
	log.Printf("Extracting Supertonic model...")
	return extractTarGz(archivePath, dataDir)
}

// ── Archive helpers ───────────────────────────────────────────────────────────

type progressWriter struct {
	io.Writer
	totalWritten int64
	lastReport   int64
	fileName     string
	totalSize    int64
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.Writer.Write(p)
	pw.totalWritten += int64(n)
	// Report every 20MB
	if pw.totalWritten-pw.lastReport >= 20*1024*1024 {
		pw.lastReport = pw.totalWritten
		if pw.totalSize > 0 {
			percent := int((float64(pw.totalWritten) / float64(pw.totalSize)) * 100)
			log.Printf("Unpacking %s: %d%% (%d MB / %d MB)", pw.fileName, percent, pw.totalWritten/(1024*1024), pw.totalSize/(1024*1024))
		} else {
			log.Printf("Unpacking %s: %d MB", pw.fileName, pw.totalWritten/(1024*1024))
		}
	}
	return n, err
}

func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
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
			log.Printf("Unpacking: %s (size: %d MB)", hdr.Name, hdr.Size/(1024*1024))
			pw := &progressWriter{
				Writer:    out,
				fileName:  hdr.Name,
				totalSize: hdr.Size,
			}
			_, err = io.Copy(pw, tr)
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
	log.Printf("STAGE: DOWNLOAD")
	log.Printf("Downloading Supertonic model from %s ...", url)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}

	totalSize := resp.ContentLength
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	var downloaded int64
	buffer := make([]byte, 32*1024)
	lastReport := 0

	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			f.Write(buffer[:n])
			downloaded += int64(n)
			if totalSize > 0 {
				percent := int(float64(downloaded) / float64(totalSize) * 100)
				if percent > lastReport {
					log.Printf("PROGRESS: %d%%", percent)
					lastReport = percent
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
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
		base = filepath.Join(home, "Library", "Application Support", "supertonic_tts")
	case "windows":
		base = filepath.Join(os.Getenv("APPDATA"), "supertonic_tts")
	default:
		base = filepath.Join(home, ".local", "share", "supertonic_tts")
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
