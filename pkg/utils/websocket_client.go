package utils

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"sync"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/gorilla/websocket"
)

// StatusUpdate represents the message structure sent to the ShapeBlock server
type StatusUpdate struct {
	// Resource identification
	ResourceType string `json:"resourceType"` // "AppBuild", "Project", "Service"
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`

	// Status information
	Status  string `json:"status"` // "Pending", "Building", "Completed", "Failed"
	Message string `json:"message,omitempty"`

	// Pod details for log streaming
	PodName      string `json:"podName,omitempty"`
	PodNamespace string `json:"podNamespace,omitempty"`

	// Timestamps
	Timestamp string `json:"timestamp"` // ISO 8601 format
	StartTime string `json:"startTime,omitempty"`
	EndTime   string `json:"endTime,omitempty"`

	// Additional metadata
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Build-specific details
	BuildType string `json:"buildType,omitempty"` // "buildpack", "dockerfile", "prebuilt"
	ImageTag  string `json:"imageTag,omitempty"`
	Registry  string `json:"registry,omitempty"`
}

// Helper function to create a status update
func NewStatusUpdate(resourceType string, name string, namespace string) StatusUpdate {
	return StatusUpdate{
		ResourceType: resourceType,
		Name:         name,
		Namespace:    namespace,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Labels:       make(map[string]string),
		Annotations:  make(map[string]string),
	}
}

// Example usage in the AppBuild controller:
func (r *AppBuildReconciler) sendBuildStatus(build *appsv1alpha1.AppBuild) {
	if r.WebsocketClient == nil {
		return
	}

	update := NewStatusUpdate("AppBuild", build.Name, build.Namespace)
	update.Status = build.Status.Phase
	update.Message = build.Status.Message
	update.PodName = build.Status.PodName
	update.PodNamespace = build.Status.PodNamespace
	update.BuildType = build.Spec.BuildType
	update.ImageTag = build.Spec.ImageTag
	update.Registry = build.Spec.RegistryURL

	if build.Status.StartTime != nil {
		update.StartTime = build.Status.StartTime.Format(time.RFC3339)
	}
	if build.Status.CompletionTime != nil {
		update.EndTime = build.Status.CompletionTime.Format(time.RFC3339)
	}

	// Add relevant labels
	update.Labels["app.kubernetes.io/name"] = build.Spec.AppName
	update.Labels["build.shapeblock.io/id"] = build.Name

	r.WebsocketClient.SendStatus(update)
}

type WebsocketClient struct {
	conn         *websocket.Conn
	serverURL    string
	apiKey       string
	done         chan struct{}
	messageQueue chan StatusUpdate
	mu           sync.RWMutex // Protects conn
	isConnected  bool
}

func NewWebsocketClient(serverURL, apiKey string) (*WebsocketClient, error) {
	if serverURL == "" || apiKey == "" {
		return nil, fmt.Errorf("serverURL and apiKey are required")
	}

	client := &WebsocketClient{
		serverURL:    serverURL,
		apiKey:       apiKey,
		done:         make(chan struct{}),
		messageQueue: make(chan StatusUpdate, 100), // Buffer up to 100 messages
		isConnected:  false,
	}

	// Start connection manager
	go client.connectionManager()
	// Start message processor
	go client.messageProcessor()

	return client, nil
}

func (w *WebsocketClient) connectionManager() {
	backoff := &backoff.ExponentialBackOff{
		InitialInterval:     500 * time.Millisecond,
		RandomizationFactor: 0.5,
		Multiplier:          1.5,
		MaxInterval:         30 * time.Second,
		MaxElapsedTime:      0, // Never stop trying
		Clock:               backoff.SystemClock,
	}
	backoff.Reset()

	ticker := time.NewTicker(30 * time.Second) // Heartbeat interval
	defer ticker.Stop()

	for {
		select {
		case <-w.done:
			return
		default:
			if err := w.connect(); err != nil {
				duration := backoff.NextBackOff()
				log.Printf("Connection failed, retrying in %v: %v", duration, err)
				time.Sleep(duration)
				continue
			}
			backoff.Reset()

			// Start heartbeat routine
			heartbeatDone := make(chan struct{})
			go w.heartbeat(ticker, heartbeatDone)

			// Wait for connection to fail or done signal
			select {
			case <-w.done:
				close(heartbeatDone)
				return
			case <-heartbeatDone:
				// Connection failed, loop will retry
				w.setConnected(false)
			}
		}
	}
}

func (w *WebsocketClient) connect() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	u, err := url.Parse(w.serverURL)
	if err != nil {
		return fmt.Errorf("invalid server URL: %v", err)
	}

	// Add API key as query parameter
	q := u.Query()
	q.Set("api_key", w.apiKey)
	u.RawQuery = q.Encode()

	// Connect to WebSocket server
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to connect: %v", err)
	}

	w.conn = conn
	w.isConnected = true
	return nil
}

func (w *WebsocketClient) heartbeat(ticker *time.Ticker, done chan struct{}) {
	for {
		select {
		case <-ticker.C:
			w.mu.RLock()
			if w.conn == nil {
				w.mu.RUnlock()
				close(done)
				return
			}
			err := w.conn.WriteMessage(websocket.PingMessage, nil)
			w.mu.RUnlock()

			if err != nil {
				log.Printf("Heartbeat failed: %v", err)
				w.closeConnection()
				close(done)
				return
			}
		case <-w.done:
			return
		}
	}
}

func (w *WebsocketClient) messageProcessor() {
	for {
		select {
		case <-w.done:
			return
		case update := <-w.messageQueue:
			for {
				if err := w.sendMessage(update); err != nil {
					if !w.isConnected {
						// If not connected, wait a bit and retry
						time.Sleep(time.Second)
						continue
					}
					log.Printf("Failed to send message: %v", err)
				}
				break
			}
		}
	}
}

func (w *WebsocketClient) sendMessage(update StatusUpdate) error {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if !w.isConnected || w.conn == nil {
		return fmt.Errorf("not connected")
	}

	data, err := json.Marshal(update)
	if err != nil {
		return fmt.Errorf("failed to marshal update: %v", err)
	}

	return w.conn.WriteMessage(websocket.TextMessage, data)
}

func (w *WebsocketClient) SendStatus(update StatusUpdate) {
	select {
	case w.messageQueue <- update:
		// Message queued successfully
	default:
		// Queue is full, log warning
		log.Printf("Message queue full, dropping status update for %s/%s", update.Namespace, update.Name)
	}
}

func (w *WebsocketClient) setConnected(connected bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.isConnected = connected
}

func (w *WebsocketClient) closeConnection() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn != nil {
		w.conn.Close()
		w.conn = nil
	}
	w.isConnected = false
}

func (w *WebsocketClient) Close() {
	close(w.done)
	w.closeConnection()
}
