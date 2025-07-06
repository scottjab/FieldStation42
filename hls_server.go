package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HLSServer manages HLS streaming with on-demand transcoding
type HLSServer struct {
	outputDir    string
	segmentTime  int
	playlistSize int
	logger       *log.Logger
	mutex        sync.RWMutex
	streams      map[string]*HLSStream
}

// HLSStream represents an active HLS stream
type HLSStream struct {
	ID           string
	InputSource  string
	OutputDir    string
	PlaylistPath string
	Process      *exec.Cmd
	LastAccess   time.Time
	Active       bool
	mutex        sync.Mutex
}

// NewHLSServer creates a new HLS server
func NewHLSServer(logger *log.Logger) *HLSServer {
	server := &HLSServer{
		outputDir:    "hls_output",
		segmentTime:  2,
		playlistSize: 5,
		logger:       logger,
		streams:      make(map[string]*HLSStream),
	}

	// Create output directory
	if err := os.MkdirAll(server.outputDir, 0755); err != nil {
		logger.Printf("Failed to create HLS output directory: %v", err)
	}

	return server
}

// GetOrCreateStream gets an existing stream or creates a new one
func (h *HLSServer) GetOrCreateStream(streamID, inputSource string) (*HLSStream, error) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	// Check if stream already exists
	if stream, exists := h.streams[streamID]; exists && stream.Active {
		stream.LastAccess = time.Now()
		return stream, nil
	}

	// Create new stream
	stream := &HLSStream{
		ID:          streamID,
		InputSource: inputSource,
		OutputDir:   filepath.Join(h.outputDir, streamID),
		LastAccess:  time.Now(),
		Active:      false,
	}

	// Create stream directory
	if err := os.MkdirAll(stream.OutputDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create stream directory: %v", err)
	}

	stream.PlaylistPath = filepath.Join(stream.OutputDir, "playlist.m3u8")

	// Start transcoding
	if err := h.startTranscoding(stream); err != nil {
		return nil, err
	}

	stream.Active = true
	h.streams[streamID] = stream

	// Start cleanup goroutine
	go h.cleanupInactiveStreams()

	return stream, nil
}

// startTranscoding starts the ffmpeg transcoding process
func (h *HLSServer) startTranscoding(stream *HLSStream) error {
	stream.mutex.Lock()
	defer stream.mutex.Unlock()

	// Kill existing process if any
	if stream.Process != nil && stream.Process.Process != nil {
		if err := stream.Process.Process.Kill(); err != nil {
			h.logger.Printf("Failed to kill existing process: %v", err)
		}
	}

	// Determine input source
	var ffmpegInput string
	switch stream.InputSource {
	case "guide_stream":
		ffmpegInput = "color=black:size=1280x720:rate=30:duration=30"
	case "placeholder":
		ffmpegInput = "color=black:size=1280x720:rate=30:duration=30"
	default:
		// Local file
		if _, err := os.Stat(stream.InputSource); err != nil {
			return fmt.Errorf("input file not found: %s", stream.InputSource)
		}
		ffmpegInput = stream.InputSource
	}

	// Build ffmpeg command
	var ffmpegCmd []string
	if stream.InputSource == "guide_stream" || stream.InputSource == "placeholder" {
		ffmpegCmd = []string{
			"ffmpeg",
			"-f", "lavfi",
			"-i", ffmpegInput,
			"-c:v", "libx264",
			"-c:a", "aac",
			"-preset", "ultrafast",
			"-b:v", "1000k",
			"-b:a", "128k",
			"-hls_time", strconv.Itoa(h.segmentTime),
			"-hls_list_size", strconv.Itoa(h.playlistSize),
			"-hls_flags", "delete_segments",
			"-f", "hls",
			"-y",
			"-loglevel", "error",
			stream.PlaylistPath,
		}
	} else {
		ffmpegCmd = []string{
			"ffmpeg",
			"-i", ffmpegInput,
			"-c:v", "libx264",
			"-c:a", "aac",
			"-preset", "ultrafast",
			"-b:v", "1000k",
			"-b:a", "128k",
			"-hls_time", strconv.Itoa(h.segmentTime),
			"-hls_list_size", strconv.Itoa(h.playlistSize),
			"-hls_flags", "delete_segments",
			"-f", "hls",
			"-y",
			"-loglevel", "error",
			stream.PlaylistPath,
		}
	}

	h.logger.Printf("Starting HLS transcoding for stream %s: %s", stream.ID, strings.Join(ffmpegCmd, " "))

	stream.Process = exec.Command(ffmpegCmd[0], ffmpegCmd[1:]...)
	stream.Process.Stderr = os.Stderr

	if err := stream.Process.Start(); err != nil {
		return fmt.Errorf("failed to start ffmpeg: %v", err)
	}

	// Wait for playlist to be created
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(stream.PlaylistPath); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	return nil
}

// ServePlaylist serves the HLS playlist
func (h *HLSServer) ServePlaylist(w http.ResponseWriter, r *http.Request, streamID string) {
	stream, err := h.GetOrCreateStream(streamID, h.getInputSourceForStream(streamID))
	if err != nil {
		h.logger.Printf("Failed to get/create stream %s: %v", streamID, err)
		http.Error(w, "Stream not available", http.StatusNotFound)
		return
	}

	// Check if playlist exists
	if _, err := os.Stat(stream.PlaylistPath); err != nil {
		h.logger.Printf("Playlist not found for stream %s: %v", streamID, err)
		http.Error(w, "Playlist not found", http.StatusNotFound)
		return
	}

	// Serve the playlist
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeFile(w, r, stream.PlaylistPath)
}

// ServeSegment serves an HLS segment
func (h *HLSServer) ServeSegment(w http.ResponseWriter, r *http.Request, streamID, segmentName string) {
	h.mutex.RLock()
	stream, exists := h.streams[streamID]
	h.mutex.RUnlock()

	if !exists || !stream.Active {
		http.Error(w, "Stream not found", http.StatusNotFound)
		return
	}

	segmentPath := filepath.Join(stream.OutputDir, segmentName)
	if _, err := os.Stat(segmentPath); err != nil {
		h.logger.Printf("Segment not found: %s", segmentPath)
		http.Error(w, "Segment not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeFile(w, r, segmentPath)
}

// getInputSourceForStream determines the input source based on stream ID
func (h *HLSServer) getInputSourceForStream(streamID string) string {
	// Parse stream ID to determine input source
	if strings.HasPrefix(streamID, "guide") {
		return "guide_stream"
	} else if strings.HasPrefix(streamID, "channel_") {
		// Extract channel number from stream ID
		parts := strings.Split(streamID, "_")
		if len(parts) >= 2 {
			// channelNum := parts[1] // Will be used in future implementation
			// For now, return placeholder. In a real implementation,
			// you would look up the actual content for this channel
			return "placeholder"
		}
		return "placeholder"
	}
	return "placeholder" // Default fallback
}

// cleanupInactiveStreams removes streams that haven't been accessed recently
func (h *HLSServer) cleanupInactiveStreams() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		h.mutex.Lock()
		now := time.Now()
		for id, stream := range h.streams {
			if now.Sub(stream.LastAccess) > 5*time.Minute {
				h.logger.Printf("Cleaning up inactive stream: %s", id)
				if stream.Process != nil && stream.Process.Process != nil {
					if err := stream.Process.Process.Kill(); err != nil {
						h.logger.Printf("Failed to kill process during cleanup: %v", err)
					}
				}
				delete(h.streams, id)
			}
		}
		h.mutex.Unlock()
	}
}

// Stop stops all streams and cleans up
func (h *HLSServer) Stop() {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	for id, stream := range h.streams {
		h.logger.Printf("Stopping stream: %s", id)
		if stream.Process != nil && stream.Process.Process != nil {
			if err := stream.Process.Process.Kill(); err != nil {
				h.logger.Printf("Failed to kill process during shutdown: %v", err)
			}
		}
	}
	h.streams = make(map[string]*HLSStream)
}
