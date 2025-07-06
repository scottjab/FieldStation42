package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Configuration structures
type StationConfig struct {
	NetworkName   string `json:"network_name"`
	ChannelNumber int    `json:"channel_number"`
	NetworkType   string `json:"network_type"`
	ContentDir    string `json:"content_dir,omitempty"`
	StandbyImage  string `json:"standby_image,omitempty"`
}

type ServerConfig struct {
	ChannelSocket  string `json:"channel_socket"`
	DateTimeFormat string `json:"date_time_format"`
}

type StationManager struct {
	Stations   []StationConfig `json:"stations"`
	ServerConf ServerConfig    `json:"server_conf"`
}

// Player structures
type PlayStatus int

const (
	PlayStatusSuccess PlayStatus = iota
	PlayStatusFailed
	PlayStatusChannelChange
)

type PlayerOutcome struct {
	Status  PlayStatus
	Payload string
}

type PlayPoint struct {
	Plan   []PlayEntry
	Index  int
	Offset int
}

type PlayEntry struct {
	Path     string
	Duration int
	Skip     int
	IsStream bool
}

// WebSocket structures
type WebSocketConn struct {
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
}

type WebSocketMessage struct {
	Type   int
	Data   []byte
	Length int64
}

// Web player structures
type WebStationPlayer struct {
	stationConfig          *StationConfig
	currentPlayingFilePath string
	currentStreamURL       string
	receptionQuality       float64
	skipReceptionCheck     bool
	currentProcess         *exec.Cmd
	logger                 *log.Logger
}

type WebFieldPlayer struct {
	host                 string
	port                 int
	logger               *log.Logger
	manager              *StationManager
	receptionQuality     float64
	player               *WebStationPlayer
	currentChannelIndex  int
	connections          map[*WebSocketConn]bool
	connectionsMutex     sync.RWMutex
	running              bool
	currentStreamProcess *exec.Cmd
	hlsServer            *HLSServer
}

// Global variables
var (
	channelSocket = "runtime/channel.sock"
)

// WebSocket implementation using standard library
// generateWebSocketKey generates a random WebSocket key for the upgrade handshake
// This function is currently unused but kept for potential future use
func computeWebSocketAccept(key string) string {
	hash := sha1.New()
	hash.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(hash.Sum(nil))
}

func upgradeToWebSocket(w http.ResponseWriter, r *http.Request) (*WebSocketConn, error) {
	// Check if it's a WebSocket upgrade request
	if r.Header.Get("Upgrade") != "websocket" {
		return nil, fmt.Errorf("not a websocket upgrade request")
	}

	// Get the WebSocket key
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}

	// Compute the accept key
	acceptKey := computeWebSocketAccept(key)

	// Hijack the connection
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("connection doesn't support hijacking")
	}

	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("failed to hijack connection: %v", err)
	}

	// Send the WebSocket upgrade response
	response := fmt.Sprintf("HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\n"+
		"\r\n", acceptKey)

	_, err = bufrw.WriteString(response)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to write upgrade response: %v", err)
	}

	err = bufrw.Flush()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to flush upgrade response: %v", err)
	}

	return &WebSocketConn{
		conn:   conn,
		reader: bufrw.Reader,
		writer: bufrw.Writer,
	}, nil
}

func (ws *WebSocketConn) ReadMessage() (int, []byte, error) {
	// Read the first byte (FIN, RSV, Opcode)
	firstByte, err := ws.reader.ReadByte()
	if err != nil {
		return 0, nil, err
	}

	// Extract opcode
	opcode := firstByte & 0x0F

	// Read the second byte (MASK, Payload length)
	secondByte, err := ws.reader.ReadByte()
	if err != nil {
		return 0, nil, err
	}

	// Check if message is masked (client messages must be masked)
	masked := (secondByte & 0x80) != 0
	if !masked {
		return 0, nil, fmt.Errorf("unmasked message from client")
	}

	// Get payload length
	payloadLen := int64(secondByte & 0x7F)

	// Handle extended payload lengths
	switch payloadLen {
	case 126:
		lenBytes := make([]byte, 2)
		_, err = io.ReadFull(ws.reader, lenBytes)
		if err != nil {
			return 0, nil, err
		}
		payloadLen = int64(lenBytes[0])<<8 | int64(lenBytes[1])
	case 127:
		lenBytes := make([]byte, 8)
		_, err = io.ReadFull(ws.reader, lenBytes)
		if err != nil {
			return 0, nil, err
		}
		payloadLen = 0
		for i := 0; i < 8; i++ {
			payloadLen = payloadLen<<8 | int64(lenBytes[i])
		}
	}

	// Read masking key
	maskKey := make([]byte, 4)
	if masked {
		_, err = io.ReadFull(ws.reader, maskKey)
		if err != nil {
			return 0, nil, err
		}
	}

	// Read payload
	payload := make([]byte, payloadLen)
	_, err = io.ReadFull(ws.reader, payload)
	if err != nil {
		return 0, nil, err
	}

	// Unmask payload if necessary
	if masked {
		for i := 0; i < len(payload); i++ {
			payload[i] ^= maskKey[i%4]
		}
	}

	return int(opcode), payload, nil
}

func (ws *WebSocketConn) WriteMessage(messageType int, data []byte) error {
	// Create WebSocket frame
	frame := make([]byte, 0, len(data)+10)

	// First byte: FIN=1, RSV=0, Opcode
	frame = append(frame, byte(0x80|messageType))

	// Second byte: MASK=0 (server doesn't mask), Payload length
	if len(data) < 126 {
		frame = append(frame, byte(len(data)))
	} else if len(data) < 65536 {
		frame = append(frame, 126)
		frame = append(frame, byte(len(data)>>8))
		frame = append(frame, byte(len(data)))
	} else {
		frame = append(frame, 127)
		for i := 7; i >= 0; i-- {
			frame = append(frame, byte(len(data)>>(i*8)))
		}
	}

	// Payload
	frame = append(frame, data...)

	// Write frame
	_, err := ws.writer.Write(frame)
	if err != nil {
		return err
	}

	return ws.writer.Flush()
}

func (ws *WebSocketConn) Close() error {
	return ws.conn.Close()
}

func main() {
	// Setup logging
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	logger := log.New(os.Stdout, "", log.LstdFlags)

	// Load station configuration
	manager, err := loadStationManager()
	if err != nil {
		logger.Fatalf("Failed to load station configuration: %v", err)
	}

	if len(manager.Stations) == 0 {
		logger.Fatal("No stations configured. Check your configuration files.")
	}

	// Create web player
	webPlayer := NewWebFieldPlayer("0.0.0.0", 9191, manager, logger)

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// HLS server is initialized in NewWebFieldPlayer
	logger.Printf("HLS streaming server ready")

	// Start web server in goroutine
	go func() {
		logger.Printf("Starting web server on %s:%d", webPlayer.host, webPlayer.port)
		if err := webPlayer.startServer(); err != nil {
			logger.Fatalf("Failed to start server: %v", err)
		}
	}()

	// Start main loop
	go func() {
		mainLoop(webPlayer, logger)
	}()

	// Wait for shutdown signal
	<-sigChan
	logger.Println("Shutting down...")
	webPlayer.shutdown()
}

func loadStationManager() (*StationManager, error) {
	// Load server configuration from main_config.json if it exists
	serverConf := ServerConfig{
		ChannelSocket:  "runtime/channel.socket",
		DateTimeFormat: "%Y-%m-%dT%H:%M:%S",
	}

	mainConfigPath := "confs/main_config.json"
	if data, err := os.ReadFile(mainConfigPath); err == nil {
		var mainConfig map[string]interface{}
		if err := json.Unmarshal(data, &mainConfig); err == nil {
			if channelSocket, ok := mainConfig["channel_socket"].(string); ok {
				serverConf.ChannelSocket = channelSocket
			}
			if dateTimeFormat, ok := mainConfig["date_time_format"].(string); ok {
				serverConf.DateTimeFormat = dateTimeFormat
			}
		}
	}

	// Load station configurations from confs directory
	var stations []StationConfig
	confsDir := "confs"

	// Read all JSON files in confs directory
	entries, err := os.ReadDir(confsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read confs directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || entry.Name() == "main_config.json" {
			continue
		}

		filePath := filepath.Join(confsDir, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			log.Printf("Warning: failed to read %s: %v", filePath, err)
			continue
		}

		var config map[string]interface{}
		if err := json.Unmarshal(data, &config); err != nil {
			log.Printf("Warning: failed to parse %s: %v", filePath, err)
			continue
		}

		// Extract station_conf
		stationConfData, ok := config["station_conf"]
		if !ok {
			log.Printf("Warning: no station_conf found in %s", filePath)
			continue
		}

		// Convert to JSON and back to get proper struct
		stationConfJSON, err := json.Marshal(stationConfData)
		if err != nil {
			log.Printf("Warning: failed to marshal station_conf from %s: %v", filePath, err)
			continue
		}

		var station StationConfig
		if err := json.Unmarshal(stationConfJSON, &station); err != nil {
			log.Printf("Warning: failed to parse station_conf from %s: %v", filePath, err)
			continue
		}

		// Set defaults for optional fields
		if station.NetworkType == "" {
			station.NetworkType = "standard"
		}

		// Validate required file paths if they exist in the config
		if station.StandbyImage != "" {
			if _, err := os.Stat(station.StandbyImage); os.IsNotExist(err) {
				log.Printf("Warning: standby_image file does not exist: %s", station.StandbyImage)
			}
		}

		stations = append(stations, station)
	}

	// Sort stations by channel number
	for i := 0; i < len(stations)-1; i++ {
		for j := i + 1; j < len(stations); j++ {
			if stations[i].ChannelNumber > stations[j].ChannelNumber {
				stations[i], stations[j] = stations[j], stations[i]
			}
		}
	}

	return &StationManager{
		Stations:   stations,
		ServerConf: serverConf,
	}, nil
}

func NewWebFieldPlayer(host string, port int, manager *StationManager, logger *log.Logger) *WebFieldPlayer {
	hlsServer := NewHLSServer(logger)

	return &WebFieldPlayer{
		host:                host,
		port:                port,
		logger:              logger,
		manager:             manager,
		receptionQuality:    1.0,
		currentChannelIndex: 0,
		connections:         make(map[*WebSocketConn]bool),
		running:             true,
		hlsServer:           hlsServer,
	}
}

func (w *WebFieldPlayer) startServer() error {
	mux := http.NewServeMux()

	// Setup routes
	mux.HandleFunc("/", w.handleRoot)
	mux.HandleFunc("/api/status", w.handleStatus)
	mux.HandleFunc("/api/channel/up", w.handleChannelUp)
	mux.HandleFunc("/api/channel/down", w.handleChannelDown)
	mux.HandleFunc("/live", w.handleLiveStream)
	mux.HandleFunc("/stream/", w.handleStream)
	mux.HandleFunc("/guide", w.handleGuide)
	mux.HandleFunc("/test", w.handleTestVideo)
	mux.HandleFunc("/ws", w.handleWebSocket)

	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", w.host, w.port),
		Handler: mux,
	}

	return server.ListenAndServe()
}

func (w *WebFieldPlayer) handleRoot(resp http.ResponseWriter, req *http.Request) {
	resp.Header().Set("Content-Type", "text/html")
	if _, err := resp.Write([]byte(w.getHTMLInterface())); err != nil {
		w.logger.Printf("Failed to write HTML response: %v", err)
	}
}

func (w *WebFieldPlayer) handleStatus(resp http.ResponseWriter, req *http.Request) {
	resp.Header().Set("Content-Type", "application/json")

	status := map[string]interface{}{
		"channel":           -1,
		"name":              "",
		"title":             "",
		"stream_url":        "",
		"reception_quality": 0.0,
	}

	if w.player != nil && len(w.manager.Stations) > w.currentChannelIndex {
		station := w.manager.Stations[w.currentChannelIndex]
		status["channel"] = station.ChannelNumber
		status["name"] = station.NetworkName
		status["title"] = w.player.getCurrentTitle()
		status["stream_url"] = w.player.getCurrentStreamURL()
		status["reception_quality"] = w.receptionQuality
	}

	if err := json.NewEncoder(resp).Encode(status); err != nil {
		w.logger.Printf("Failed to encode status JSON: %v", err)
	}
}

func (w *WebFieldPlayer) handleChannelUp(resp http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(resp, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.currentChannelIndex = (w.currentChannelIndex + 1) % len(w.manager.Stations)
	station := w.manager.Stations[w.currentChannelIndex]

	resp.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(resp).Encode(map[string]interface{}{
		"status":  "ok",
		"channel": station.ChannelNumber,
	}); err != nil {
		w.logger.Printf("Failed to encode channel up response: %v", err)
	}
}

func (w *WebFieldPlayer) handleChannelDown(resp http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(resp, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.currentChannelIndex = (w.currentChannelIndex - 1 + len(w.manager.Stations)) % len(w.manager.Stations)
	station := w.manager.Stations[w.currentChannelIndex]

	resp.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(resp).Encode(map[string]interface{}{
		"status":  "ok",
		"channel": station.ChannelNumber,
	}); err != nil {
		w.logger.Printf("Failed to encode channel down response: %v", err)
	}
}

func (w *WebFieldPlayer) handleLiveStream(resp http.ResponseWriter, req *http.Request) {
	if w.player == nil {
		http.Error(resp, "No content currently playing", http.StatusNotFound)
		return
	}

	filePath := w.player.currentPlayingFilePath

	// For now, let's use a simple approach: generate a static video file and serve it
	// This is much more reliable than real-time streaming
	tempFile := "current_video.mp4"

	// Determine the input source based on filePath
	var inputSource string
	switch filePath {
	case "guide_stream":
		inputSource = "color=black:size=1280x720:rate=30:duration=30"
	case "placeholder":
		stationName := "Unknown"
		if w.player.stationConfig != nil {
			stationName = w.player.stationConfig.NetworkName
		}
		inputSource = fmt.Sprintf("color=black:size=1280x720:rate=30:duration=30,drawtext=text='%s':fontcolor=white:fontsize=60:x=(w-text_w)/2:y=(h-text_h)/2", stationName)
	default:
		// Check if it's a local file path
		if _, err := os.Stat(filePath); err != nil {
			// Try to find the file in the station's content directory
			if w.player.stationConfig != nil && w.player.stationConfig.ContentDir != "" {
				contentPath := filepath.Join(w.player.stationConfig.ContentDir, filepath.Base(filePath))
				if _, err := os.Stat(contentPath); err == nil {
					inputSource = contentPath
				} else {
					w.logger.Printf("File not found: %s or %s", filePath, contentPath)
					http.Error(resp, "File not found", http.StatusNotFound)
					return
				}
			} else {
				w.logger.Printf("File not found: %s", filePath)
				http.Error(resp, "File not found", http.StatusNotFound)
				return
			}
		} else {
			inputSource = filePath
		}
	}

	w.logger.Printf("handleLiveStream: ffmpeg will use inputSource='%s' (original filePath='%s')", inputSource, filePath)

	// Generate video file
	var ffmpegCmd []string
	switch filePath {
	case "guide_stream", "placeholder":
		ffmpegCmd = []string{
			"ffmpeg",
			"-f", "lavfi",
			"-i", inputSource,
			"-c:v", "libx264",
			"-c:a", "aac",
			"-preset", "ultrafast",
			"-b:v", "1000k",
			"-b:a", "128k",
			"-y",
			"-loglevel", "error",
			tempFile,
		}
	default:
		ffmpegCmd = []string{
			"ffmpeg",
			"-i", inputSource,
			"-c:v", "libx264",
			"-c:a", "aac",
			"-preset", "ultrafast",
			"-b:v", "1000k",
			"-b:a", "128k",
			"-y",
			"-loglevel", "error",
			tempFile,
		}
	}

	w.logger.Printf("Generating video file with ffmpeg")
	cmd := exec.Command(ffmpegCmd[0], ffmpegCmd[1:]...)

	if err := cmd.Run(); err != nil {
		w.logger.Printf("Failed to generate video: %v", err)
		http.Error(resp, "Failed to generate video", http.StatusInternalServerError)
		return
	}

	// Check if file was created
	if _, err := os.Stat(tempFile); err != nil {
		w.logger.Printf("Video file not found: %v", err)
		http.Error(resp, "Video file not found", http.StatusInternalServerError)
		return
	}

	// Serve the file directly
	w.logger.Printf("Serving video file: %s", tempFile)
	resp.Header().Set("Content-Type", "video/mp4")
	resp.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(resp, req, tempFile)
}

func (w *WebFieldPlayer) handleGuide(resp http.ResponseWriter, req *http.Request) {
	resp.Header().Set("Content-Type", "text/html")
	if _, err := resp.Write([]byte(w.getGuideHTML())); err != nil {
		w.logger.Printf("Failed to write guide HTML response: %v", err)
	}
}

func (w *WebFieldPlayer) handleTestVideo(resp http.ResponseWriter, req *http.Request) {
	w.logger.Printf("Test video request from %s", req.RemoteAddr)

	// Try a simpler approach - generate a static video file first, then serve it
	// This avoids the complexity of real-time MP4 streaming
	tempFile := "temp_test_video.mp4"

	// Generate a test video file
	ffmpegCmd := []string{
		"ffmpeg",
		"-f", "lavfi",
		"-i", "testsrc=duration=10:size=1280x720:rate=30",
		"-f", "mp4",
		"-vcodec", "libx264",
		"-preset", "ultrafast",
		"-b:v", "1000k",
		"-y",
		"-loglevel", "error",
		tempFile,
	}

	w.logger.Printf("Generating test video file with ffmpeg")
	cmd := exec.Command(ffmpegCmd[0], ffmpegCmd[1:]...)

	if err := cmd.Run(); err != nil {
		w.logger.Printf("Failed to generate test video: %v", err)
		http.Error(resp, "Failed to generate test video", http.StatusInternalServerError)
		return
	}

	// Check if file was created
	if _, err := os.Stat(tempFile); err != nil {
		w.logger.Printf("Test video file not found: %v", err)
		http.Error(resp, "Test video file not found", http.StatusInternalServerError)
		return
	}

	// Serve the file directly
	w.logger.Printf("Serving test video file: %s", tempFile)
	http.ServeFile(resp, req, tempFile)

	// Clean up the file after serving
	go func() {
		time.Sleep(5 * time.Second) // Give time for the file to be served
		if err := os.Remove(tempFile); err != nil {
			w.logger.Printf("Failed to remove temp file: %v", err)
		}
	}()
}

func (w *WebFieldPlayer) handleWebSocket(resp http.ResponseWriter, req *http.Request) {
	conn, err := upgradeToWebSocket(resp, req)
	if err != nil {
		w.logger.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	w.connectionsMutex.Lock()
	w.connections[conn] = true
	w.connectionsMutex.Unlock()

	defer func() {
		w.connectionsMutex.Lock()
		delete(w.connections, conn)
		w.connectionsMutex.Unlock()
		_ = conn.Close()
	}()

	for {
		opcode, message, err := conn.ReadMessage()
		if err != nil {
			break
		}

		// Handle websocket messages
		if opcode == 1 { // Text message
			w.logger.Printf("Received WebSocket message: %s", string(message))
		} else if opcode == 8 { // Close message
			break
		}
	}
}

func (w *WebFieldPlayer) handleStream(resp http.ResponseWriter, req *http.Request) {
	if w.player == nil {
		http.Error(resp, "No content currently playing", http.StatusNotFound)
		return
	}

	// Parse the URL path to determine what to serve
	path := req.URL.Path
	path = strings.TrimPrefix(path, "/stream")

	// Check if this is a request for an HLS playlist
	if strings.HasSuffix(path, ".m3u8") {
		streamID := strings.TrimSuffix(filepath.Base(path), ".m3u8")
		w.hlsServer.ServePlaylist(resp, req, streamID)
		return
	}

	// Check if this is a request for an HLS segment
	if strings.HasSuffix(path, ".ts") {
		parts := strings.Split(path, "/")
		if len(parts) >= 3 {
			streamID := parts[1]
			segmentName := filepath.Base(path)
			w.hlsServer.ServeSegment(resp, req, streamID, segmentName)
			return
		}
	}

	// Default: serve the HLS playlist for the current channel
	streamID := fmt.Sprintf("channel_%d", w.currentChannelIndex)
	w.hlsServer.ServePlaylist(resp, req, streamID)
}

func (w *WebFieldPlayer) getHTMLInterface() string {
	return `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>FieldStation42 Web Player</title>
    <style>
        body {
            font-family: 'Courier New', monospace;
            background-color: #000;
            color: #0f0;
            margin: 0;
            padding: 20px;
            overflow: hidden;
        }
        .container {
            display: flex;
            flex-direction: column;
            height: 100vh;
        }
        .video-container {
            flex: 1;
            position: relative;
            background-color: #111;
            border: 2px solid #0f0;
            border-radius: 10px;
            overflow: hidden;
        }
        video {
            width: 100%;
            height: 100%;
            object-fit: contain;
        }
        .controls {
            display: flex;
            justify-content: space-between;
            align-items: center;
            padding: 10px;
            background-color: #111;
            border: 2px solid #0f0;
            border-radius: 10px;
            margin-top: 10px;
        }
        .channel-info {
            display: flex;
            flex-direction: column;
            align-items: center;
        }
        .channel-number {
            font-size: 2em;
            font-weight: bold;
        }
        .channel-name {
            font-size: 1.2em;
        }
        .show-title {
            font-size: 1em;
            color: #0a0;
        }
        .control-buttons {
            display: flex;
            gap: 10px;
        }
        button {
            background-color: #000;
            color: #0f0;
            border: 2px solid #0f0;
            padding: 10px 20px;
            font-family: 'Courier New', monospace;
            font-size: 1em;
            cursor: pointer;
            border-radius: 5px;
            transition: all 0.3s;
        }
        button:hover {
            background-color: #0f0;
            color: #000;
        }
        .reception-indicator {
            width: 100px;
            height: 20px;
            background-color: #333;
            border: 1px solid #0f0;
            border-radius: 10px;
            overflow: hidden;
        }
        .reception-bar {
            height: 100%;
            background-color: #0f0;
            transition: width 0.3s;
        }
        .status {
            position: absolute;
            top: 10px;
            right: 10px;
            background-color: rgba(0, 0, 0, 0.8);
            padding: 10px;
            border-radius: 5px;
            font-size: 0.9em;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="video-container">
            <video id="videoPlayer" controls autoplay>
                Your browser does not support the video tag.
            </video>
            <div class="status" id="status">
                Loading...
            </div>
        </div>
        <div class="controls">
            <div class="channel-info">
                <div class="channel-number" id="channelNumber">--</div>
                <div class="channel-name" id="channelName">No Signal</div>
                <div class="show-title" id="showTitle"></div>
            </div>
            <div class="control-buttons">
                <button onclick="changeChannel('down')">CH DOWN</button>
                <button onclick="changeChannel('up')">CH UP</button>
            </div>
            <div class="reception-indicator">
                <div class="reception-bar" id="receptionBar" style="width: 0%"></div>
            </div>
        </div>
    </div>

    <script>
        let currentStreamUrl = '';
        
        async function updateStatus() {
            try {
                const response = await fetch('/api/status');
                const status = await response.json();
                
                
                document.getElementById('channelNumber').textContent = status.channel || '--';
                document.getElementById('channelName').textContent = status.name || 'No Signal';
                document.getElementById('showTitle').textContent = status.title || '';
                document.getElementById('receptionBar').style.width = (status.reception_quality * 100) + '%';
                
                // Use the HLS streaming endpoint
                const video = document.getElementById('videoPlayer');
                const streamSrc = '/stream/channel_' + status.channel + '/playlist.m3u8';
                if (video.src !== streamSrc) {
                    video.src = streamSrc;
                    video.load();
                    
                    // Video event handlers
                    video.addEventListener('loadedmetadata', () => {
                        console.log('Video metadata loaded, attempting to play');
                        video.play().catch(e => console.log('Auto-play prevented:', e));
                    });
                    video.addEventListener('canplay', () => {
                        console.log('Video can play, starting playback');
                        video.play().catch(e => console.log('Play failed:', e));
                    });
                    video.addEventListener('error', (e) => {
                        console.log('Video error:', e);
                        console.log('Video error details:', video.error);
                        // Try to reload the stream
                        setTimeout(() => {
                            console.log('Reloading video stream...');
                            video.src = streamSrc + '?' + Date.now();
                            video.load();
                        }, 2000);
                    });
                    video.addEventListener('loadstart', () => {
                        console.log('Video load started');
                    });
                    video.addEventListener('progress', () => {
                        console.log('Video loading progress');
                    });
                }
                
                document.getElementById('status').textContent = 
                    'Quality: ' + Math.round(status.reception_quality * 100) + '%';
                    
            } catch (error) {
                console.error('Error updating status:', error);
                document.getElementById('status').textContent = 'Connection Error';
            }
        }
        
        async function changeChannel(direction) {
            try {
                const response = await fetch('/api/channel/' + direction, { method: 'POST' });
                const result = await response.json();
                console.log('Channel changed:', result);
            } catch (error) {
                console.error('Error changing channel:', error);
            }
        }
        
        // Update status every second
        setInterval(updateStatus, 1000);
        
        // Initial status update
        updateStatus();
        
        // Keyboard controls
        document.addEventListener('keydown', (event) => {
            switch(event.key) {
                case 'ArrowUp':
                    changeChannel('up');
                    break;
                case 'ArrowDown':
                    changeChannel('down');
                    break;
                case ' ':
                    // Toggle play/pause
                    const video = document.getElementById('videoPlayer');
                    if (video.paused) {
                        video.play();
                    } else {
                        video.pause();
                    }
                    break;
            }
        });
    </script>
</body>
</html>`
}

func (w *WebFieldPlayer) getGuideHTML() string {
	return `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>FieldStation42 Guide</title>
    <style>
        body {
            font-family: 'Courier New', monospace;
            background-color: #000;
            color: #0f0;
            margin: 0;
            padding: 20px;
            overflow: hidden;
        }
        .guide-container {
            display: flex;
            flex-direction: column;
            height: 100vh;
        }
        .header {
            text-align: center;
            padding: 20px;
            border-bottom: 2px solid #0f0;
        }
        .content {
            flex: 1;
            display: flex;
            justify-content: center;
            align-items: center;
            text-align: center;
        }
        .guide-text {
            font-size: 1.5em;
            line-height: 1.6;
        }
        .footer {
            text-align: center;
            padding: 20px;
            border-top: 2px solid #0f0;
        }
    </style>
</head>
<body>
    <div class="guide-container">
        <div class="header">
            <h1>FieldStation42 Guide</h1>
        </div>
        <div class="content">
            <div class="guide-text">
                <p>Welcome to the FieldStation42 Guide Channel</p>
                <p>Use channel up/down to navigate between stations</p>
                <p>Guide functionality coming soon...</p>
            </div>
        </div>
        <div class="footer">
            <p>FieldStation42 - Retro TV Experience</p>
        </div>
    </div>
</body>
</html>`
}

func (w *WebFieldPlayer) shutdown() {
	w.running = false
	if w.currentStreamProcess != nil {
		_ = w.currentStreamProcess.Process.Kill()
	}
	if w.player != nil {
		w.player.shutdown()
	}
	if w.hlsServer != nil {
		w.hlsServer.Stop()
	}
}

// WebStationPlayer methods
func NewWebStationPlayer(stationConfig *StationConfig, logger *log.Logger) *WebStationPlayer {
	return &WebStationPlayer{
		stationConfig:      stationConfig,
		receptionQuality:   1.0,
		skipReceptionCheck: false,
		logger:             logger,
	}
}

func (p *WebStationPlayer) shutdown() {
	p.currentPlayingFilePath = ""
	p.currentStreamURL = ""
	if p.currentProcess != nil {
		_ = p.currentProcess.Process.Kill()
		p.currentProcess = nil
	}
}

func (p *WebStationPlayer) updateFilters() {
	// Web player doesn't apply video filters directly
	// They would need to be applied at the video source level
}

func (p *WebStationPlayer) playFile(filePath string, fileDuration *int, currentTime *int, isStream bool) bool {
	p.logger.Printf("Attempting to play %s", filePath)

	// Check if file exists or is a stream
	if _, err := os.Stat(filePath); err != nil && !isStream {
		p.logger.Printf("File %s does not exist", filePath)
		return false
	}

	p.currentPlayingFilePath = filePath

	// Kill any existing stream
	if p.currentProcess != nil {
		_ = p.currentProcess.Process.Kill()
		p.currentProcess = nil
	}

	// For web streaming, we need to serve the video file via HTTP
	if isStream {
		p.currentStreamURL = filePath
	} else {
		// Convert local file path to web-accessible streaming URL
		p.currentStreamURL = "/live"
	}

	basename := filepath.Base(filePath)
	_ = strings.TrimSuffix(basename, filepath.Ext(basename)) // Extract title for potential future use

	p.logger.Printf("Playing %s via web stream at %s", filePath, p.currentStreamURL)
	return true
}

func (p *WebStationPlayer) getCurrentTitle() string {
	if p.currentPlayingFilePath != "" {
		basename := filepath.Base(p.currentPlayingFilePath)
		return strings.TrimSuffix(basename, filepath.Ext(basename))
	}
	return ""
}

func (p *WebStationPlayer) getCurrentStreamURL() string {
	return p.currentStreamURL
}

func (p *WebStationPlayer) showGuide(guideConfig *StationConfig) *PlayerOutcome {
	// Set up guide video stream to use /live endpoint
	p.currentStreamURL = "/live"
	p.currentPlayingFilePath = "guide_stream"

	// Simulate playing for 30 seconds before checking for channel changes
	// This prevents the infinite loop
	time.Sleep(30 * time.Second)

	// Check for channel change
	response := checkChannelSocket()
	if response != nil {
		return response
	}

	return &PlayerOutcome{Status: PlayStatusSuccess}
}

func (p *WebStationPlayer) playSlot(networkName string, when time.Time) *PlayerOutcome {
	// Get the actual scheduled content using Go-native scheduling
	playPoint, err := p.getPlayPointFromSchedule(networkName, when)
	if err != nil {
		p.logger.Printf("Failed to get play point from schedule: %v", err)
		// Fall back to placeholder if scheduling fails
		p.currentStreamURL = "/live"
		p.currentPlayingFilePath = "placeholder"
		time.Sleep(30 * time.Second)
		return &PlayerOutcome{Status: PlayStatusSuccess}
	}

	// Use the actual scheduled content
	if len(playPoint.Plan) > 0 && playPoint.Index < len(playPoint.Plan) {
		entry := playPoint.Plan[playPoint.Index]
		p.currentPlayingFilePath = entry.Path
		p.currentStreamURL = "/live"

		// Calculate how long to play this content
		duration := entry.Duration - playPoint.Offset
		if duration > 0 {
			time.Sleep(time.Duration(duration) * time.Second)
		}
	} else {
		// No content available, use placeholder
		p.currentStreamURL = "/live"
		p.currentPlayingFilePath = "placeholder"
		time.Sleep(30 * time.Second)
	}

	// Check for channel change
	response := checkChannelSocket()
	if response != nil {
		return response
	}

	return &PlayerOutcome{Status: PlayStatusSuccess}
}

// getPlayPointFromSchedule implements a simplified scheduling system in Go
func (p *WebStationPlayer) getPlayPointFromSchedule(networkName string, when time.Time) (*PlayPoint, error) {
	// Use the current station configuration
	if p.stationConfig == nil {
		return nil, fmt.Errorf("no station configuration available")
	}

	// For now, implement a simple content selection based on time and content directory
	// This is a simplified version that just picks content from the content directory
	if p.stationConfig.ContentDir != "" {
		contentFiles, err := p.getContentFiles(p.stationConfig.ContentDir)
		if err != nil {
			return nil, fmt.Errorf("failed to get content files: %v", err)
		}

		if len(contentFiles) > 0 {
			// Simple round-robin selection based on time
			index := int(when.Unix()/3600) % len(contentFiles) // Change every hour
			selectedFile := contentFiles[index]

			// Create a simple play point with the selected content
			playPoint := &PlayPoint{
				Index:  0,
				Offset: 0,
				Plan: []PlayEntry{
					{
						Path:     selectedFile,
						Duration: 3600, // Assume 1 hour duration
						Skip:     0,
						IsStream: false,
					},
				},
			}

			return playPoint, nil
		}
	}

	// If no content found, return a placeholder
	return &PlayPoint{
		Index:  0,
		Offset: 0,
		Plan: []PlayEntry{
			{
				Path:     "placeholder",
				Duration: 30,
				Skip:     0,
				IsStream: false,
			},
		},
	}, nil
}

// getContentFiles scans a content directory for video files
func (p *WebStationPlayer) getContentFiles(contentDir string) ([]string, error) {
	var files []string

	entries, err := os.ReadDir(contentDir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			name := entry.Name()
			// Check for common video file extensions
			if strings.HasSuffix(strings.ToLower(name), ".mp4") ||
				strings.HasSuffix(strings.ToLower(name), ".avi") ||
				strings.HasSuffix(strings.ToLower(name), ".mkv") ||
				strings.HasSuffix(strings.ToLower(name), ".mov") {
				files = append(files, filepath.Join(contentDir, name))
			}
		}
	}

	return files, nil
}

// Utility functions
func checkChannelSocket() *PlayerOutcome {
	// Simplified implementation - in a real version this would read from the channel socket
	// For now, just return nil (no channel change)
	return nil
}

func updateStatusSocket(status, networkName string, channelNumber int, title string) {
	// Simplified implementation - in a real version this would update the status socket
}

func mainLoop(webPlayer *WebFieldPlayer, logger *log.Logger) {
	logger.Println("Starting web field player main loop")

	// Clear the channel socket (or create if it doesn't exist)
	if err := os.WriteFile(channelSocket, []byte{}, 0644); err != nil {
		logger.Printf("Failed to create channel socket: %v", err)
	}

	channelIndex := 0
	if len(webPlayer.manager.Stations) == 0 {
		logger.Fatal("No stations configured")
	}

	// Create initial player
	webPlayer.player = NewWebStationPlayer(&webPlayer.manager.Stations[channelIndex], logger)
	player := webPlayer.player
	webPlayer.receptionQuality = 0.8 // Start with some degradation
	player.updateFilters()

	channelConf := webPlayer.manager.Stations[channelIndex]

	logger.Printf("Web player started at http://%s:%d", webPlayer.host, webPlayer.port)
	logger.Println("Open your browser to view the FieldStation42 web interface")

	// Debug: Show configured stations
	logger.Printf("Configured stations: %d", len(webPlayer.manager.Stations))
	for i, station := range webPlayer.manager.Stations {
		logger.Printf("  %d: %s (Channel %d, Type: %s)", i, station.NetworkName, station.ChannelNumber, station.NetworkType)
	}

	// Main loop
	outcome := &PlayerOutcome{Status: PlayStatusSuccess}
	skipPlay := false
	stuckTimer := 0

	logger.Printf("Starting main loop with channel: %s (type: %s)", channelConf.NetworkName, channelConf.NetworkType)

	for webPlayer.running {
		if channelConf.NetworkType == "guide" && !skipPlay {
			logger.Printf("Starting guide channel: %s", channelConf.NetworkName)
			outcome = player.showGuide(&channelConf)
		} else if !skipPlay {
			now := time.Now()
			logger.Printf("Starting station: %s", channelConf.NetworkName)
			outcome = player.playSlot(channelConf.NetworkName, now)
		}

		// Reset skip
		skipPlay = false

		// Check if web interface requested a channel change
		if webPlayer.currentChannelIndex != channelIndex {
			logger.Printf("Web interface requested channel change from %d to %d", channelIndex, webPlayer.currentChannelIndex)
			channelIndex = webPlayer.currentChannelIndex
			channelConf = webPlayer.manager.Stations[channelIndex]
			webPlayer.player = NewWebStationPlayer(&webPlayer.manager.Stations[channelIndex], logger)
			player = webPlayer.player
			continue
		}

		switch outcome.Status {
		case PlayStatusChannelChange:
			stuckTimer = 0
			tuneUp := true

			// Handle channel change
			if tuneUp {
				logger.Println("Starting channel change")
				channelIndex++
				if channelIndex >= len(webPlayer.manager.Stations) {
					channelIndex = 0
				}
			}

			channelConf = webPlayer.manager.Stations[channelIndex]
			player.stationConfig = &channelConf

			// Update web player
			webPlayer.currentChannelIndex = channelIndex
			webPlayer.player = NewWebStationPlayer(&webPlayer.manager.Stations[channelIndex], logger)
			player = webPlayer.player

		case PlayStatusFailed:
			stuckTimer++

			// Only put it up once after 2 seconds of being stuck
			if stuckTimer >= 2 && channelConf.StandbyImage != "" {
				player.playFile(channelConf.StandbyImage, nil, nil, false)
			}
			currentTitleOnStuck := player.getCurrentTitle()
			updateStatusSocket("stuck", channelConf.NetworkName, channelConf.ChannelNumber, currentTitleOnStuck)

			time.Sleep(1 * time.Second)
			logger.Println("Player failed to start - resting for 1 second and trying again")

			// Check for channel change so it doesn't stay stuck on a broken channel
			newOutcome := checkChannelSocket()
			if newOutcome != nil {
				outcome = newOutcome
				// Set skip play so outcome isn't overwritten
				// and the channel change can be processed next loop
				skipPlay = true
			}
		case PlayStatusSuccess:
			stuckTimer = 0
		default:
			stuckTimer = 0
		}
	}
}
