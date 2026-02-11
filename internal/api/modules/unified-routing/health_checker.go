package unifiedrouting

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// HealthChecker performs health checks on routing targets.
type HealthChecker interface {
	// Trigger checks
	CheckAll(ctx context.Context) ([]*HealthResult, error)
	CheckRoute(ctx context.Context, routeID string) ([]*HealthResult, error)
	CheckTarget(ctx context.Context, targetID string) (*HealthResult, error)
	// TriggerCheckUntimedCoolingTargets runs health checks on untimed-cooling targets for the route (async).
	TriggerCheckUntimedCoolingTargets(ctx context.Context, routeID string)

	// ScheduleTargetCheck schedules a health check for a target at its CooldownEndsAt time.
	// Called after StartCooldownTimed to set up the per-target timer.
	// Safe to call multiple times; replaces any existing scheduled check.
	ScheduleTargetCheck(targetID string)

	// Configuration
	GetSettings(ctx context.Context) (*HealthCheckConfig, error)
	UpdateSettings(ctx context.Context, settings *HealthCheckConfig) error

	// History
	GetHistory(ctx context.Context, filter HealthHistoryFilter) ([]*HealthResult, error)

	// Background task control
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// DefaultHealthChecker implements HealthChecker.
type DefaultHealthChecker struct {
	configSvc       ConfigService
	stateMgr        StateManager
	metrics         MetricsCollector
	authManager     *coreauth.Manager
	routeActivity   *RouteActivityTracker

	mu        sync.RWMutex
	history   []*HealthResult
	maxHistory int

	// Per-target scheduled health check timers.
	// Each target in timed cooling gets its own timer that fires at CooldownEndsAt.
	timerMu         sync.Mutex
	scheduledTimers map[string]*time.Timer

	running  bool
}

// NewHealthChecker creates a new health checker.
func NewHealthChecker(
	configSvc ConfigService,
	stateMgr StateManager,
	metrics MetricsCollector,
	authManager *coreauth.Manager,
	routeActivity *RouteActivityTracker,
) *DefaultHealthChecker {
	if routeActivity == nil {
		routeActivity = NewRouteActivityTracker()
	}
	return &DefaultHealthChecker{
		configSvc:       configSvc,
		stateMgr:        stateMgr,
		metrics:         metrics,
		authManager:     authManager,
		routeActivity:   routeActivity,
		history:         make([]*HealthResult, 0, 1000),
		maxHistory:      1000,
		scheduledTimers: make(map[string]*time.Timer),
	}
}

func (h *DefaultHealthChecker) CheckAll(ctx context.Context) ([]*HealthResult, error) {
	routes, err := h.configSvc.ListRoutes(ctx)
	if err != nil {
		return nil, err
	}

	var results []*HealthResult
	for _, route := range routes {
		routeResults, err := h.CheckRoute(ctx, route.ID)
		if err != nil {
			continue
		}
		results = append(results, routeResults...)
	}

	return results, nil
}

func (h *DefaultHealthChecker) CheckRoute(ctx context.Context, routeID string) ([]*HealthResult, error) {
	pipeline, err := h.configSvc.GetPipeline(ctx, routeID)
	if err != nil {
		return nil, err
	}

	var results []*HealthResult
	for _, layer := range pipeline.Layers {
		for _, target := range layer.Targets {
			if !target.Enabled {
				continue
			}
			result, err := h.CheckTarget(ctx, target.ID)
			if err != nil {
				results = append(results, &HealthResult{
					TargetID:     target.ID,
					CredentialID: target.CredentialID,
					Model:        target.Model,
					Status:       "unhealthy",
					Message:      err.Error(),
					CheckedAt:    time.Now(),
				})
				continue
			}
			results = append(results, result)
		}
	}

	return results, nil
}

func (h *DefaultHealthChecker) CheckTarget(ctx context.Context, targetID string) (*HealthResult, error) {
	// Find the target configuration
	routes, err := h.configSvc.ListRoutes(ctx)
	if err != nil {
		return nil, err
	}

	var target *Target
	for _, route := range routes {
		pipeline, err := h.configSvc.GetPipeline(ctx, route.ID)
		if err != nil {
			continue
		}
		for _, layer := range pipeline.Layers {
			for i := range layer.Targets {
				if layer.Targets[i].ID == targetID {
					target = &layer.Targets[i]
					break
				}
			}
			if target != nil {
				break
			}
		}
		if target != nil {
			break
		}
	}

	if target == nil {
		return nil, &TargetNotFoundError{TargetID: targetID}
	}

	// Perform health check
	result := h.performHealthCheck(ctx, target)

	// Record result
	h.recordResult(result)

	// Update state based on result
	if result.Status == "healthy" {
		h.stateMgr.RecordSuccess(ctx, targetID, time.Duration(result.LatencyMs)*time.Millisecond)
	} else {
		h.stateMgr.RecordFailure(ctx, targetID, result.Message)
	}

	// Record event
	eventType := EventTargetRecovered
	if result.Status == "unhealthy" {
		eventType = EventTargetFailed
	}
	h.metrics.RecordEvent(&RoutingEvent{
		Type:     eventType,
		RouteID:  "",
		TargetID: targetID,
		Details: map[string]any{
			"status":     result.Status,
			"latency_ms": result.LatencyMs,
			"message":    result.Message,
		},
	})

	return result, nil
}

func (h *DefaultHealthChecker) performHealthCheck(ctx context.Context, target *Target) *HealthResult {
	result := &HealthResult{
		TargetID:     target.ID,
		CredentialID: target.CredentialID,
		Model:        target.Model,
		CheckedAt:    time.Now(),
	}

	if h.authManager == nil {
		result.Status = "unhealthy"
		result.Message = "auth manager unavailable"
		return result
	}

	// Find the auth entry for this credential
	auths := h.authManager.List()
	var targetAuth *coreauth.Auth
	for _, auth := range auths {
		if auth.ID == target.CredentialID {
			targetAuth = auth
			break
		}
	}

	if targetAuth == nil {
		result.Status = "unhealthy"
		result.Message = "credential not found"
		return result
	}

	// Build minimal request for health check
	openAIRequest := map[string]interface{}{
		"model": target.Model,
		"messages": []map[string]interface{}{
			{"role": "user", "content": "hi"},
		},
		"stream":     true,
		"max_tokens": 1,
	}

	requestJSON, err := json.Marshal(openAIRequest)
	if err != nil {
		result.Status = "unhealthy"
		result.Message = "failed to build request"
		return result
	}

	// Get health check config for timeout
	healthConfig, _ := h.configSvc.GetHealthCheckConfig(ctx)
	if healthConfig == nil {
		cfg := DefaultHealthCheckConfig()
		healthConfig = &cfg
	}

	checkCtx, cancel := context.WithTimeout(usage.WithSkipUsage(ctx), time.Duration(healthConfig.CheckTimeoutSeconds)*time.Second)
	defer cancel()

	startTime := time.Now()

	// Execute health check request
	req := cliproxyexecutor.Request{
		Model:   target.Model,
		Payload: requestJSON,
		Format:  sdktranslator.FormatOpenAI,
	}

	opts := cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatOpenAI,
		OriginalRequest: requestJSON,
	}

	stream, err := h.authManager.ExecuteStreamWithAuth(checkCtx, targetAuth, req, opts)
	if err != nil {
		result.Status = "unhealthy"
		result.Message = err.Error()
		return result
	}

	// Wait for first chunk
	select {
	case chunk, ok := <-stream:
		if ok {
			if chunk.Err != nil {
				result.Status = "unhealthy"
				result.Message = chunk.Err.Error()
			} else {
				result.Status = "healthy"
				result.LatencyMs = time.Since(startTime).Milliseconds()
			}
			// Drain remaining chunks
			cancel()
			go func() {
				for range stream {
				}
			}()
		} else {
			result.Status = "unhealthy"
			result.Message = "stream closed without data"
		}
	case <-checkCtx.Done():
		result.Status = "unhealthy"
		result.Message = "health check timeout"
	}

	return result
}

func (h *DefaultHealthChecker) recordResult(result *HealthResult) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Ring buffer behavior
	if len(h.history) >= h.maxHistory {
		h.history = h.history[1:]
	}
	h.history = append(h.history, result)
}

func (h *DefaultHealthChecker) GetSettings(ctx context.Context) (*HealthCheckConfig, error) {
	return h.configSvc.GetHealthCheckConfig(ctx)
}

func (h *DefaultHealthChecker) UpdateSettings(ctx context.Context, settings *HealthCheckConfig) error {
	return h.configSvc.UpdateHealthCheckConfig(ctx, settings)
}

func (h *DefaultHealthChecker) GetHistory(ctx context.Context, filter HealthHistoryFilter) ([]*HealthResult, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var results []*HealthResult
	for i := len(h.history) - 1; i >= 0; i-- {
		result := h.history[i]

		// Apply filters
		if filter.TargetID != "" && result.TargetID != filter.TargetID {
			continue
		}
		if filter.Status != "" && result.Status != filter.Status {
			continue
		}
		if !filter.Since.IsZero() && result.CheckedAt.Before(filter.Since) {
			continue
		}

		results = append(results, result)

		if filter.Limit > 0 && len(results) >= filter.Limit {
			break
		}
	}

	return results, nil
}

func (h *DefaultHealthChecker) Start(ctx context.Context) error {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return nil
	}
	h.running = true
	h.mu.Unlock()

	// Schedule checks for any targets already in timed cooling (e.g. after restart).
	h.scheduleExistingCoolingTargets()
	return nil
}

func (h *DefaultHealthChecker) Stop(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.running {
		return nil
	}

	h.running = false

	// Cancel all per-target timers.
	h.timerMu.Lock()
	for id, t := range h.scheduledTimers {
		t.Stop()
		delete(h.scheduledTimers, id)
	}
	h.timerMu.Unlock()

	return nil
}

// scheduleExistingCoolingTargets scans all target states on startup and schedules
// timers for any targets already in timed cooling.
func (h *DefaultHealthChecker) scheduleExistingCoolingTargets() {
	ctx := context.Background()
	states, err := h.stateMgr.ListTargetStates(ctx)
	if err != nil {
		return
	}
	for _, state := range states {
		if state.Status == StatusCooling && state.CooldownEndsAt != nil {
			h.ScheduleTargetCheck(state.TargetID)
		}
	}
}

// ScheduleTargetCheck schedules a health check for the given target at its CooldownEndsAt time.
// It reads the target's current state to determine when to fire.
// Safe to call multiple times — replaces any existing scheduled check for this target.
func (h *DefaultHealthChecker) ScheduleTargetCheck(targetID string) {
	ctx := context.Background()

	// Read the target state to get CooldownEndsAt.
	state, _ := h.stateMgr.GetTargetState(ctx, targetID)
	if state == nil || state.Status != StatusCooling || state.CooldownEndsAt == nil {
		return
	}

	delay := time.Until(*state.CooldownEndsAt)
	if delay < 0 {
		delay = 0 // already expired, check immediately
	}

	h.timerMu.Lock()
	defer h.timerMu.Unlock()

	// Cancel existing timer for this target.
	if t, ok := h.scheduledTimers[targetID]; ok {
		t.Stop()
	}

	h.scheduledTimers[targetID] = time.AfterFunc(delay, func() {
		h.onTargetCheckDue(targetID)
	})
}

// onTargetCheckDue is the callback when a per-target timer fires.
// It runs the health check and either recovers the target, reschedules, or moves to untimed.
func (h *DefaultHealthChecker) onTargetCheckDue(targetID string) {
	// Clean up timer reference.
	h.timerMu.Lock()
	delete(h.scheduledTimers, targetID)
	h.timerMu.Unlock()

	// Check if we've been stopped.
	h.mu.RLock()
	running := h.running
	h.mu.RUnlock()
	if !running {
		return
	}

	ctx := context.Background()

	// Verify target is still in timed cooling.
	state, _ := h.stateMgr.GetTargetState(ctx, targetID)
	if state == nil || state.Status != StatusCooling || state.CooldownEndsAt == nil {
		return // already recovered or switched to untimed
	}

	// Transition to "checking" so the frontend shows "检查中" instead of "冷却中".
	h.stateMgr.StartChecking(ctx, targetID)

	// Run health check.
	result, err := h.CheckTarget(ctx, targetID)
	if err != nil {
		log.Debugf("scheduled health check failed for target %s: %v", targetID, err)
		// Reschedule with the same interval so we retry later.
		interval := h.getCheckInterval(ctx)
		h.stateMgr.SetCooldownNextCheckIn(ctx, targetID, interval)
		h.ScheduleTargetCheck(targetID)
		return
	}

	if result.Status == "healthy" {
		h.stateMgr.EndCooldown(ctx, targetID)
		log.Infof("target %s recovered after scheduled health check", targetID)
		return
	}

	// Still unhealthy — decide timed vs untimed by route activity.
	routeID := h.getRouteIDForTarget(ctx, targetID)
	if h.routeActivity.IsProcessing(routeID) {
		// Route active → schedule next check after another interval.
		interval := h.getCheckInterval(ctx)
		h.stateMgr.SetCooldownNextCheckIn(ctx, targetID, interval)
		h.ScheduleTargetCheck(targetID) // reschedule
	} else {
		// Route not active → switch to untimed cooling (checked only on request).
		h.stateMgr.StartCooldownUntimed(ctx, targetID)
	}
}

// getCheckInterval returns the configured health check interval.
func (h *DefaultHealthChecker) getCheckInterval(ctx context.Context) time.Duration {
	if cfg, _ := h.configSvc.GetHealthCheckConfig(ctx); cfg != nil && cfg.CheckIntervalSeconds > 0 {
		return time.Duration(cfg.CheckIntervalSeconds) * time.Second
	}
	return 30 * time.Second
}

// getRouteIDForTarget returns the route ID that contains the given target, or "" if not found.
func (h *DefaultHealthChecker) getRouteIDForTarget(ctx context.Context, targetID string) string {
	routes, err := h.configSvc.ListRoutes(ctx)
	if err != nil {
		return ""
	}
	for _, route := range routes {
		pipeline, err := h.configSvc.GetPipeline(ctx, route.ID)
		if err != nil {
			continue
		}
		for _, layer := range pipeline.Layers {
			for _, t := range layer.Targets {
				if t.ID == targetID {
					return route.ID
				}
			}
		}
	}
	return ""
}

// TriggerCheckUntimedCoolingTargets runs health checks on cooling targets that need
// on-request checking for the given route. This includes:
// - Untimed cooling targets (CooldownEndsAt == nil)
// - Timed cooling targets whose CooldownEndsAt has already passed (expired)
// Called when a request arrives so these targets get a chance to recover.
// Runs async; does not block the request.
func (h *DefaultHealthChecker) TriggerCheckUntimedCoolingTargets(ctx context.Context, routeID string) {
	// Use background context since this runs asynchronously and must not be
	// cancelled when the originating HTTP request finishes.
	bgCtx := context.Background()

	pipeline, err := h.configSvc.GetPipeline(bgCtx, routeID)
	if err != nil {
		return
	}
	now := time.Now()
	var checkTargetIDs []string
	for _, layer := range pipeline.Layers {
		for _, target := range layer.Targets {
			if !target.Enabled {
				continue
			}
			state, _ := h.stateMgr.GetTargetState(bgCtx, target.ID)
			if state != nil && state.Status == StatusCooling &&
				(state.CooldownEndsAt == nil || !now.Before(*state.CooldownEndsAt)) {
				checkTargetIDs = append(checkTargetIDs, target.ID)
			}
		}
	}
	if len(checkTargetIDs) == 0 {
		return
	}
	go func() {
		var wg sync.WaitGroup
		for _, targetID := range checkTargetIDs {
			wg.Add(1)
			go func(tid string) {
				defer wg.Done()
				// Transition to "checking" so the frontend shows "检查中".
				h.stateMgr.StartChecking(bgCtx, tid)
				result, err := h.CheckTarget(bgCtx, tid)
				if err != nil {
					return
				}
				if result.Status == "healthy" {
					h.stateMgr.EndCooldown(bgCtx, tid)
					log.Infof("target %s recovered after on-request health check", tid)
				} else {
					h.stateMgr.StartCooldownTimed(bgCtx, tid)
					h.ScheduleTargetCheck(tid)
				}
			}(targetID)
		}
		wg.Wait()
	}()
}

// TargetNotFoundError is returned when a target is not found.
type TargetNotFoundError struct {
	TargetID string
}

func (e *TargetNotFoundError) Error() string {
	return "target not found: " + e.TargetID
}
