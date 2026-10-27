package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"signaling-server/internal/fleet"
	"signaling-server/internal/stats"
)

// The admin socket serves matrix-fleet on the Center host only: it has no
// network port, and the socket file is readable and writable by the service
// user alone (root reaches it through sudo).
//
//	GET  /v1/status          nodes and the active configuration version
//	GET  /v1/stats?days=N    viewing counters, fleet and per node
//	POST /v1/reload          re-read the configuration file
//	POST /v1/moves           (body: a configuration) cameras it would move

// FleetStatus is the answer of GET /v1/status.
type FleetStatus struct {
	ConfigVersion int                 `json:"config_version"`
	Fleet         bool                `json:"fleet"`
	Nodes         []fleet.NodeStatus  `json:"nodes"`
	Relays        []fleet.RelayStatus `json:"relays,omitempty"`
	// Set on a control node: its link to Center.
	Center string `json:"center,omitempty"`
	Linked bool   `json:"linked,omitempty"`
}

// MovesResult is the answer of POST /v1/moves.
type MovesResult struct {
	Moved   int `json:"moved"`
	Online  int `json:"online"`
	Unknown int `json:"unknown"`
}

func listenAdminSocket(path string) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

func (s *Server) serveAdmin(listener net.Listener, configPath string) {
	server := &http.Server{Handler: s.adminHandler(configPath)}
	configureHTTPServer(server)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("admin socket: %v", err)
	}
}

func (s *Server) adminHandler(configPath string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", only(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		writeAdminJSON(w, http.StatusOK, s.fleetStatus())
	}))
	mux.HandleFunc("/v1/stats", only(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		days := 7
		if text := r.URL.Query().Get("days"); text != "" {
			n, err := strconv.Atoi(text)
			if err != nil || n < 1 || n > stats.KeepDays {
				writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("days must be 1 to %d", stats.KeepDays))
				return
			}
			days = n
		}
		if s.centerAPI == nil {
			writeAdminError(w, http.StatusNotFound, "statistics are kept by Center; this server is not one")
			return
		}
		writeAdminJSON(w, http.StatusOK, s.centerAPI.Stats(days))
	}))
	mux.HandleFunc("/v1/reload", only(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		if s.fleet == nil {
			writeAdminError(w, http.StatusConflict, "this server runs without a fleet configuration (-config-file); on Center run setup.sh upgrade to start using config.json")
			return
		}
		if err := s.reloadFleet(configPath); err != nil {
			log.Printf("fleet: configuration not applied, keeping version %d: %v", s.fleet.Version(), err)
			writeAdminError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		log.Printf("fleet: configuration version %d applied", s.fleet.Version())
		writeAdminJSON(w, http.StatusOK, map[string]int{"version": s.fleet.Version()})
	}))
	mux.HandleFunc("/v1/moves", only(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		if s.fleet == nil {
			writeAdminError(w, http.StatusConflict, "this server runs without a fleet configuration (-config-file); on Center run setup.sh upgrade to start using config.json")
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, fleet.MaxConfigBytes+1))
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		config, err := fleet.ParseConfig(data)
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		moved, online, unknown, err := s.fleet.Moves(config)
		if err != nil {
			writeAdminError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeAdminJSON(w, http.StatusOK, MovesResult{Moved: moved, Online: online, Unknown: unknown})
	}))
	return mux
}

// only rejects other methods (the module's go version predates ServeMux
// method patterns).
func only(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		next(w, r)
	}
}

func (s *Server) fleetStatus() FleetStatus {
	if s.fleet != nil {
		return FleetStatus{ConfigVersion: s.fleet.Version(), Fleet: true, Nodes: s.fleet.Status(), Relays: s.fleet.Relays(s.relayStatuses())}
	}
	report := s.LocalReport("")
	status := FleetStatus{Nodes: []fleet.NodeStatus{{Address: "(this server)", Healthy: true,
		Cameras: len(report.Cameras), Clients: report.Clients, LastSeen: time.Now().UTC().Format(time.RFC3339)}}}
	if s.nodeLink != nil {
		_, version := s.nodeLink.Config()
		status.ConfigVersion, status.Center, status.Linked = version, s.nodeLink.serverName, s.nodeLink.connected.Load()
		status.Nodes[0].Address, status.Nodes[0].ConfigVersion = s.nodeLink.address, version
		status.Relays = s.relayStatuses()
	}
	return status
}

func writeAdminJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

func writeAdminError(w http.ResponseWriter, code int, message string) {
	writeAdminJSON(w, code, map[string]string{"error": message})
}
