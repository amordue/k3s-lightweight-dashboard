package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"k3s-dashboard/internal/collector"
	"k3s-dashboard/internal/model"
	"k3s-dashboard/internal/storage"
	"k3s-dashboard/internal/ui"
)

type Server struct {
	store     *storage.Store
	collector *collector.Collector
	logger    *log.Logger
}

func NewServer(store *storage.Store, metricCollector *collector.Collector, logger *log.Logger) *Server {
	return &Server{store: store, collector: metricCollector, logger: logger}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/workloads", s.handleWorkloadsPage)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/api/v1/summary", s.handleSummary)
	mux.HandleFunc("/api/v1/cluster-history", s.handleClusterHistory)
	mux.HandleFunc("/api/v1/workloads", s.handleWorkloadList)
	mux.HandleFunc("/api/v1/workload-history", s.handleWorkloadHistory)
	mux.HandleFunc("/api/v1/namespaces/", s.handleNamespaceDetail)
	mux.HandleFunc("/api/v1/workloads/", s.handleWorkloadDetail)
	mux.HandleFunc("/api/v1/pods/", s.handlePodDetail)

	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	s.servePage(w, "index.html")
}

func (s *Server) handleWorkloadsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/workloads" {
		http.NotFound(w, r)
		return
	}

	s.servePage(w, "workloads.html")
}

func (s *Server) servePage(w http.ResponseWriter, name string) {
	content, err := ui.Files.ReadFile(name)
	if err != nil {
		http.Error(w, "ui unavailable", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(content)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	status := s.collector.Status()
	code := http.StatusOK
	if !status.Ready {
		code = http.StatusServiceUnavailable
	}

	writeJSON(w, code, status)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := s.store.LatestSummary(r.Context())
	if err != nil {
		s.writeStorageError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleClusterHistory(w http.ResponseWriter, r *http.Request) {
	window, err := parseWindow(r, 168*time.Hour)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	history, err := s.store.ClusterHistory(r.Context(), window)
	if err != nil {
		s.writeStorageError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, history)
}

func (s *Server) handleWorkloadList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	workloads, err := s.store.ListWorkloads(r.Context())
	if err != nil {
		s.writeStorageError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"workloads": workloads})
}

func (s *Server) handleWorkloadHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read request body"})
		return
	}

	var payload struct {
		Window    string                 `json:"window"`
		Workloads []model.WorkloadSelector `json:"workloads"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if len(payload.Workloads) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one workload is required"})
		return
	}

	window := 24 * time.Hour
	if payload.Window != "" {
		window, err = time.ParseDuration(payload.Window)
		if err != nil || window <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "window must be a positive duration"})
			return
		}
	}

	response, err := s.store.WorkloadHistory(r.Context(), payload.Workloads, window)
	if err != nil {
		s.writeStorageError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleNamespaceDetail(w http.ResponseWriter, r *http.Request) {
	namespace := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/")
	if namespace == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "namespace is required"})
		return
	}

	window, err := parseWindow(r, 24*time.Hour)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	detail, err := s.store.NamespaceDetail(r.Context(), namespace, window)
	if err != nil {
		s.writeStorageError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleWorkloadDetail(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/api/v1/workloads/"))
	if len(parts) != 3 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected /api/v1/workloads/{namespace}/{kind}/{name}"})
		return
	}

	window, err := parseWindow(r, 24*time.Hour)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	detail, err := s.store.WorkloadDetail(r.Context(), parts[0], parts[1], parts[2], window)
	if err != nil {
		s.writeStorageError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handlePodDetail(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/api/v1/pods/"))
	if len(parts) != 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected /api/v1/pods/{namespace}/{name}"})
		return
	}

	window, err := parseWindow(r, 24*time.Hour)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	detail, err := s.store.PodDetail(r.Context(), parts[0], parts[1], window)
	if err != nil {
		s.writeStorageError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) writeStorageError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := err.Error()
	if errors.Is(err, storage.ErrNoData) {
		status = http.StatusServiceUnavailable
		message = "metrics collection has not produced data yet"
	}

	writeJSON(w, status, map[string]string{"error": message})
}

func parseWindow(r *http.Request, fallback time.Duration) (time.Duration, error) {
	value := r.URL.Query().Get("window")
	if value == "" {
		return fallback, nil
	}

	window, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}

	if window <= 0 {
		return 0, errors.New("window must be positive")
	}

	return window, nil
}

func splitPath(value string) []string {
	trimmed := strings.Trim(value, "/")
	if trimmed == "" {
		return nil
	}

	return strings.Split(trimmed, "/")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}