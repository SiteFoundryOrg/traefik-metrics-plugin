// Package trafficplugin a traffic monitoring plugin.
package trafficplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// Metric represents a single request/response metric.
type Metric struct {
	AppID         string `json:"app_id"`
	RequestBytes  int64  `json:"req_bytes"`
	ResponseBytes int64  `json:"resp_bytes"`
	Timestamp     int64  `json:"timestamp"`
	logger        *log.Logger
}

func createConfig() *Config {
	return &Config{}
}

// Config the plugin configuration.
type Config struct {
	CollectorAddr string `json:"collector_addr,omitempty"` // UDP collector address (e.g., "127.0.0.1:8125")
	ChannelSize   int    `json:"channel_size,omitempty"`   // Bounded channel buffer size
	AppIDHeader   string `json:"app_id_header,omitempty"`  // Header name to extract app ID (e.g., "X-App-ID")
}

// responseWriter wraps http.ResponseWriter to capture response bytes.
type responseWriter struct {
	http.ResponseWriter
	bytesWritten int64
	statusCode   int
}

func (responseWriter *responseWriter) Write(b []byte) (int, error) {
	n, err := responseWriter.ResponseWriter.Write(b)
	responseWriter.bytesWritten += int64(n)
	return n, err
}

func (responseWriter *responseWriter) WriteHeader(statusCode int) {
	responseWriter.statusCode = statusCode
	responseWriter.ResponseWriter.WriteHeader(statusCode)
}

// TrafficPlugin a traffic monitoring plugin.
type TrafficPlugin struct {
	next          http.Handler
	name          string
	config        *Config
	metricsChan   chan *Metric
	udpConnection *net.UDPConn
	stopChan      chan struct{}
	wg            sync.WaitGroup
}

// New created a new TrafficPlugin plugin.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config.ChannelSize <= 0 {
		config.ChannelSize = 1000
	}
	if config.CollectorAddr == "" {
		config.CollectorAddr = "127.0.0.1:8125"
	}
	if config.AppIDHeader == "" {
		config.AppIDHeader = "X-App-ID"
	}

	log.Println("level", "info", "msg", "request-metrics plugin initialized", "endpoint", config.CollectorAddr)

	// Parse UDP address
	udpAddress, err := net.ResolveUDPAddr("udp", config.CollectorAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid collector address: %w", err)
	}

	// Create UDP connection
	udpConnection, err := net.DialUDP("udp", nil, udpAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to create UDP connection: %w", err)
	}

	plugin := &TrafficPlugin{
		next:          next,
		name:          name,
		config:        config,
		metricsChan:   make(chan *Metric, config.ChannelSize),
		udpConnection: udpConnection,
		stopChan:      make(chan struct{}),
	}

	// Start background worker goroutine
	plugin.wg.Add(1)
	go plugin.worker()

	return plugin, nil
}

// getRequestSize calculates the size of the HTTP request.
func getRequestSize(request *http.Request) int64 {
	var size int64

	// Request line
	size += int64(len(request.Method) + len(request.URL.Path) + len(request.Proto) + 4)

	// Headers
	for key, values := range request.Header {
		for _, value := range values {
			size += int64(len(key) + len(value) + 4) // key: value\r\n
		}
	}

	// Body
	if request.Body != nil {
		bodyBytes, _ := io.ReadAll(request.Body)
		size += int64(len(bodyBytes))
		request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	return size
}

// getAppID extracts app ID from request headers.
func (plugin *TrafficPlugin) getAppID(request *http.Request) string {
	if appID := request.Header.Get(plugin.config.AppIDHeader); appID != "" {
		return appID
	}
	// Fallback: use remote address if no app ID header
	return request.RemoteAddr
}

func (plugin *TrafficPlugin) ServeHTTP(httpResponseWriter http.ResponseWriter, request *http.Request) {
	// Calculate request size
	requestBytes := getRequestSize(request)

	// Wrap response writer to capture response bytes
	wrappedResponseWriter := &responseWriter{
		ResponseWriter: httpResponseWriter,
		statusCode:     http.StatusOK,
	}

	// Process request
	plugin.next.ServeHTTP(wrappedResponseWriter, request)

	// Build metric struct
	metric := &Metric{
		AppID:         plugin.getAppID(request),
		RequestBytes:  requestBytes,
		ResponseBytes: wrappedResponseWriter.bytesWritten,
		Timestamp:     time.Now().Unix(),
	}

	// Enqueue to channel (non-blocking, bounded)
	select {
	case plugin.metricsChan <- metric:
		// Successfully enqueued
	default:
		// Channel is full, drop metric (non-blocking behavior)
		// Optionally log or handle dropped metrics
	}
}

// worker is the background goroutine that processes metrics.
func (plugin *TrafficPlugin) worker() {
	defer plugin.wg.Done()

	for {
		select {
		case <-plugin.stopChan:
			return

		case metric := <-plugin.metricsChan:
			plugin.sendMetric(metric)
		}
	}
}

// sendMetric sends a metric to the collector via UDP.
func (plugin *TrafficPlugin) sendMetric(metric *Metric) {
	// Serialize metric to JSON
	metricData, err := json.Marshal(metric)
	if err != nil {
		log.Printf("level", "error", "msg", "failed to marshal metric", "error", err)
		return
	}

	// Send UDP packet (non-blocking)
	log.Println("level", "debug", "msg", "sending metric", "data", string(metricData))
	_, err = plugin.udpConnection.Write(metricData)
	if err != nil {
		// Log error (in production, use proper logging)
		log.Printf("level", "error", "msg", "failed to send metric via UDP", "error", err)
		return
	}
}

// Close cleans up resources.
func (plugin *TrafficPlugin) Close() error {
	close(plugin.stopChan)
	plugin.wg.Wait()
	if plugin.udpConnection != nil {
		return plugin.udpConnection.Close()
	}
	return nil
}
