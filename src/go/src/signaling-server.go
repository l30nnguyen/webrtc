package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	InactivityTimeout    = 90 * time.Second
	KeepaliveInterval    = 30 * time.Second
	KeepalivePingTimeout = 10 * time.Second
	WriteWait            = 10 * time.Second
)

type DeviceInfo struct {
	ID          string    `json:"id"`
	ConnectedAt string    `json:"connected_at"`
	Path        string    `json:"path"`
	Type        string    `json:"type"`
	RTT         float64   `json:"rtt"`
	LastSeen    time.Time `json:"-"`
}

type Client struct {
	ID        string
	Conn      *websocket.Conn
	Info      *DeviceInfo
	Send      chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

func (c *Client) Close() {
	c.closeOnce.Do(func() { close(c.done) })
}

type Server struct {
	clients    map[string]*Client
	mu         sync.RWMutex
	upgrader   websocket.Upgrader
	playerPath string
	rootDir    string
}

func NewServer(playerPath string, rootDir string) *Server {
	return &Server{
		clients: make(map[string]*Client),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
		playerPath: playerPath,
		rootDir:    rootDir,
	}
}

func (s *Server) loadTurnConfig() []map[string]interface{} {
	data, err := os.ReadFile(s.rootDir + "cfg/turn.conf")
	if err != nil {
		return nil
	}
	var servers []map[string]interface{}
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil
	}
	return servers
}

func (s *Server) loadVersion() string {
	data, err := os.ReadFile(s.rootDir + "cfg/version")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(data))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/":
		http.ServeFile(w, r, s.playerPath)
	case r.URL.Path == "/api/devices":
		s.handleDevices(w, r)
	case r.URL.Path == "/api/health":
		s.handleHealth(w, r)
	case strings.HasPrefix(r.URL.Path, "/downloads/"):
		s.handleDownloads(w, r)
	default:
		s.handleWebSocket(w, r)
	}
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	clientID := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
	if clientID == "" {
		http.Error(w, "Client ID required", http.StatusBadRequest)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Upgrade failed: %v", err)
		return
	}

	log.Printf("New connection from %s path=%s", r.RemoteAddr, r.URL.Path)

	deviceType := "consumer"
	if !strings.HasPrefix(clientID, "Player") {
		deviceType = "producer"
	}

	c := &Client{
		ID:   clientID,
		Conn: conn,
		Info: &DeviceInfo{
			ID:          clientID,
			ConnectedAt: time.Now().UTC().Format(time.RFC3339),
			Path:        r.URL.Path,
			Type:        deviceType,
			LastSeen:    time.Now(),
		},
		Send: make(chan []byte, 256),
		done: make(chan struct{}),
	}

	s.mu.Lock()
	if old, exists := s.clients[clientID]; exists {
		old.Close()
		old.Conn.Close()
	}
	s.clients[clientID] = c
	s.mu.Unlock()

	log.Printf("Client %s connected", clientID)

	go s.writePump(c)
	s.readPump(c)
}

func (s *Server) readPump(c *Client) {
	defer func() {
		s.mu.Lock()
		if s.clients[c.ID] == c {
			delete(s.clients, c.ID)
		}
		s.mu.Unlock()
		c.Conn.Close()
		c.Close()
		log.Printf("Client %s disconnected", c.ID)
	}()

	pongWait := KeepaliveInterval + KeepalivePingTimeout
	c.Conn.SetReadLimit(512 * 1024)
	c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		c.Info.LastSeen = time.Now()
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[%s] Read error: %v", c.ID, err)
			}
			break
		}

		c.Info.LastSeen = time.Now()
		log.Printf("[%s] << %s", c.ID, string(message))

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		if msg["type"] == "bye" {
			log.Printf("[%s] sent bye, closing cleanly", c.ID)
			c.Conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(WriteWait))
			break
		}

		destID, _ := msg["id"].(string)
		if destID == "" {
			continue
		}

		s.mu.RLock()
		dest, exists := s.clients[destID]
		s.mu.RUnlock()

		if exists {
			msg["id"] = c.ID
			if strings.HasPrefix(c.ID, "Player") && msg["type"] == "request" {
				if servers := s.loadTurnConfig(); len(servers) > 0 {
					msg["turn"] = servers
				}
			}
			data, _ := json.Marshal(msg)
			select {
			case dest.Send <- data:
				log.Printf("[%s] >> %s", destID, string(data))
			default:
			}
		} else {
			data, _ := json.Marshal(map[string]string{"type": "error", "msg": "Device offline"})
			select {
			case c.Send <- data:
			default:
			}
		}
	}
}

func (s *Server) writePump(c *Client) {
	ticker := time.NewTicker(KeepaliveInterval)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(WriteWait))
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.Conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(WriteWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (s *Server) sweepInactiveClients() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		s.mu.RLock()
		var stale []*Client
		for _, c := range s.clients {
			if now.Sub(c.Info.LastSeen) > InactivityTimeout {
				stale = append(stale, c)
			}
		}
		s.mu.RUnlock()

		for _, c := range stale {
			log.Printf("[%s] Inactive for %v, disconnecting", c.ID, InactivityTimeout)
			c.Close()
		}
	}
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	typeFilter := r.URL.Query().Get("type")
	s.mu.RLock()
	defer s.mu.RUnlock()

	devices := make([]DeviceInfo, 0)
	for _, c := range s.clients {
		if typeFilter != "" && c.Info.Type != typeFilter {
			continue
		}
		devices = append(devices, *c.Info)
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"count":   len(devices),
		"devices": devices,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	n := len(s.clients)
	s.mu.RUnlock()

	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"clients": n,
		"version": s.loadVersion(),
	})
}

func (s *Server) handleDownloads(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimPrefix(r.URL.Path, "/downloads/")
	if filename == "" || strings.Contains(filename, "..") {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}
	filepath := s.rootDir + "downloads/" + filename
	http.ServeFile(w, r, filepath)
}

func main() {
	port := flag.Int("port", 8000, "Listen port")
	rootDir := flag.String("root", "", "Root directory for config and downloads (default: current dir)")
	https := flag.Bool("https", false, "Enable HTTPS (uses cert.pem from root directory)")
	flag.Parse()

	root := *rootDir
	if root != "" && !strings.HasSuffix(root, "/") {
		root += "/"
	}

	playerPath := root + "player/ws.html"
	srv := NewServer(playerPath, root)
	go srv.sweepInactiveClients()

	addr := fmt.Sprintf(":%d", *port)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: srv,
	}

	if *https {
		certPath := root + "cert.pem"
		cert, err := tls.LoadX509KeyPair(certPath, certPath)
		if err != nil {
			log.Fatalf("TLS: %v", err)
		}
		httpServer.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		log.Printf("Listening on wss://0.0.0.0%s", addr)
		log.Printf("  GET https://0.0.0.0%s/         — WebRTC Player", addr)
		log.Printf("  GET https://0.0.0.0%s/api/devices  — list online devices", addr)
		log.Printf("  GET https://0.0.0.0%s/api/health   — health check", addr)
		log.Fatal(httpServer.ListenAndServeTLS("", ""))
	} else {
		log.Printf("Listening on ws://0.0.0.0%s", addr)
		log.Printf("  GET http://0.0.0.0%s/         — WebRTC Player", addr)
		log.Printf("  GET http://0.0.0.0%s/api/devices  — list online devices", addr)
		log.Printf("  GET http://0.0.0.0%s/api/health   — health check", addr)
		log.Fatal(httpServer.ListenAndServe())
	}
}
