package management

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// ProxyServer represents a saved proxy configuration.
type ProxyServer struct {
	ID       string `json:"id"`
	Prefix   string `json:"prefix"`
	ProxyURL string `json:"proxy_url"`
	ProxyDNS string `json:"proxy_dns,omitempty"`
}

type proxyServerStore struct {
	mu      sync.RWMutex
	path    string
	servers []ProxyServer
}

func (h *Handler) proxyStorePath() string {
	dir := ""
	if h.cfg != nil && strings.TrimSpace(h.cfg.AuthDir) != "" {
		dir = h.cfg.AuthDir
	}
	if dir == "" {
		dir = "."
	}
	if dir[0] == '~' {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, dir[1:])
		}
	}
	return filepath.Join(dir, "proxy-servers.json")
}

func (s *proxyServerStore) load(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.servers = []ProxyServer{}
			return nil
		}
		return err
	}
	var list []ProxyServer
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	s.servers = list
	return nil
}

func (s *proxyServerStore) save() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.servers, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

func (s *proxyServerStore) list() []ProxyServer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]ProxyServer, len(s.servers))
	copy(result, s.servers)
	return result
}

func (s *proxyServerStore) get(id string) *ProxyServer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.servers {
		if s.servers[i].ID == id {
			cp := s.servers[i]
			return &cp
		}
	}
	return nil
}

func (s *proxyServerStore) add(p ProxyServer) (ProxyServer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	p.ProxyURL = strings.TrimSpace(p.ProxyURL)
	p.ProxyDNS = strings.TrimSpace(p.ProxyDNS)
	p.Prefix = strings.TrimSpace(p.Prefix)
	s.servers = append(s.servers, p)
	err := s.save()
	return p, err
}

func (s *proxyServerStore) update(id string, p ProxyServer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.ID = id
	p.ProxyURL = strings.TrimSpace(p.ProxyURL)
	p.ProxyDNS = strings.TrimSpace(p.ProxyDNS)
	p.Prefix = strings.TrimSpace(p.Prefix)
	for i := range s.servers {
		if s.servers[i].ID == id {
			s.servers[i] = p
			return s.save()
		}
	}
	return nil
}

func (s *proxyServerStore) delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.servers {
		if s.servers[i].ID == id {
			s.servers = append(s.servers[:i], s.servers[i+1:]...)
			return s.save()
		}
	}
	return nil
}

var (
	proxyStore     *proxyServerStore
	proxyStoreOnce sync.Once
)

func (h *Handler) getProxyStore() *proxyServerStore {
	proxyStoreOnce.Do(func() {
		proxyStore = &proxyServerStore{}
		path := h.proxyStorePath()
		if err := proxyStore.load(path); err != nil {
			log.Warnf("proxy store load failed: %v", err)
		}
	})
	return proxyStore
}

// ListProxyServers returns all saved proxy servers.
func (h *Handler) ListProxyServers(c *gin.Context) {
	store := h.getProxyStore()
	list := store.list()
	c.JSON(http.StatusOK, gin.H{
		"total":   len(list),
		"servers": list,
	})
}

// GetProxyServer returns a single proxy server by ID.
func (h *Handler) GetProxyServer(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id is required"})
		return
	}
	store := h.getProxyStore()
	p := store.get(id)
	if p == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "proxy server not found"})
		return
	}
	c.JSON(http.StatusOK, p)
}

// CreateProxyServer creates a new proxy server.
func (h *Handler) CreateProxyServer(c *gin.Context) {
	var req ProxyServer
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.ProxyURL) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "proxy_url is required"})
		return
	}
	store := h.getProxyStore()
	created, err := store.add(req)
	if err != nil {
		log.Errorf("create proxy server failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, created)
}

// UpdateProxyServer updates an existing proxy server.
func (h *Handler) UpdateProxyServer(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id is required"})
		return
	}
	var req ProxyServer
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.ProxyURL) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "proxy_url is required"})
		return
	}
	store := h.getProxyStore()
	if store.get(id) == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "proxy server not found"})
		return
	}
	if err := store.update(id, req); err != nil {
		log.Errorf("update proxy server failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, store.get(id))
}

// DeleteProxyServer deletes a proxy server.
func (h *Handler) DeleteProxyServer(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id is required"})
		return
	}
	store := h.getProxyStore()
	if err := store.delete(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ProxyCheckResult is a single proxy health check result for SSE streaming.
type ProxyCheckResult struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	Message   string  `json:"message,omitempty"`
	LatencyMs int64   `json:"latency_ms,omitempty"`
}

// CheckAllProxyServersStream runs connectivity checks for all proxy servers and streams results via SSE.
func (h *Handler) CheckAllProxyServersStream(c *gin.Context) {
	store := h.getProxyStore()
	list := store.list()
	if len(list) == 0 {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		flusher, ok := c.Writer.(http.Flusher)
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
			return
		}
		c.SSEvent("meta", gin.H{"total": 0})
		c.SSEvent("done", gin.H{})
		flusher.Flush()
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	c.SSEvent("meta", gin.H{"total": len(list)})
	flusher.Flush()

	type result struct {
		id        string
		status    string
		message   string
		latencyMs int64
	}
	resultCh := make(chan result, len(list))
	for _, p := range list {
		proxy := p
		go func() {
			start := time.Now()
			err := executor.CheckProxyConnectivity(c.Request.Context(), proxy.ProxyURL, proxy.ProxyDNS)
			latencyMs := time.Since(start).Milliseconds()
			res := result{id: proxy.ID, latencyMs: latencyMs}
			if err != nil {
				res.status = "unhealthy"
				res.message = err.Error()
			} else {
				res.status = "healthy"
			}
			resultCh <- res
		}()
	}

	received := 0
	for received < len(list) {
		r := <-resultCh
		received++
		c.SSEvent("result", ProxyCheckResult{
			ID:        r.id,
			Status:    r.status,
			Message:   r.message,
			LatencyMs: r.latencyMs,
		})
		flusher.Flush()
	}

	c.SSEvent("done", gin.H{})
	flusher.Flush()
}

// CheckProxyServerConnectivity tests proxy connectivity.
func (h *Handler) CheckProxyServerConnectivity(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id is required"})
		return
	}
	store := h.getProxyStore()
	p := store.get(id)
	if p == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "proxy server not found"})
		return
	}
	start := time.Now()
	err := executor.CheckProxyConnectivity(c.Request.Context(), p.ProxyURL, p.ProxyDNS)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"status":     "unhealthy",
			"message":    err.Error(),
			"latency_ms": latencyMs,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":     "healthy",
		"latency_ms": latencyMs,
	})
}

// ApplyProxyToAuthFile applies a proxy server's config to an auth file (credential).
func (h *Handler) ApplyProxyToAuthFile(c *gin.Context) {
	proxyID := strings.TrimSpace(c.Param("id"))
	if proxyID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "proxy id is required"})
		return
	}
	var req struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	store := h.getProxyStore()
	p := store.get(proxyID)
	if p == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "proxy server not found"})
		return
	}
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	var targetAuth *coreauth.Auth
	if auth, ok := h.authManager.GetByID(name); ok {
		targetAuth = auth
	} else {
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth.FileName == name {
				targetAuth = auth
				break
			}
		}
	}
	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	targetAuth.ProxyURL = p.ProxyURL
	targetAuth.ProxyDNS = p.ProxyDNS
	if p.Prefix != "" {
		targetAuth.Prefix = p.Prefix
	}
	targetAuth.UpdatedAt = time.Now()
	if _, err := h.authManager.Update(c.Request.Context(), targetAuth); err != nil {
		log.Errorf("apply proxy to auth file failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "proxy applied"})
}
