package unifiedrouting

import (
	"context"
	"sync"
	"time"
)

// StateManager manages runtime state for unified routing.
type StateManager interface {
	// State queries
	GetOverview(ctx context.Context) (*StateOverview, error)
	GetRouteState(ctx context.Context, routeID string) (*RouteState, error)
	GetTargetState(ctx context.Context, targetID string) (*TargetState, error)
	ListTargetStates(ctx context.Context) ([]*TargetState, error)

	// State changes (called by engine and health checker)
	RecordSuccess(ctx context.Context, targetID string, latency time.Duration)
	RecordFailure(ctx context.Context, targetID string, reason string)
	StartCooldownTimed(ctx context.Context, targetID string)   // next check in CheckIntervalSeconds
	StartCooldownUntimed(ctx context.Context, targetID string)
	StartChecking(ctx context.Context, targetID string)        // health check in progress
	EndCooldown(ctx context.Context, targetID string)
	SetCooldownNextCheckIn(ctx context.Context, targetID string, d time.Duration) // when cooling or checking

	// Manual operations
	ResetTarget(ctx context.Context, targetID string) error
	ForceCooldown(ctx context.Context, targetID string) error

	// Initialize/cleanup
	InitializeTarget(ctx context.Context, targetID string) error
	RemoveTarget(ctx context.Context, targetID string) error
}

// DefaultStateManager implements StateManager.
type DefaultStateManager struct {
	store     StateStore
	configSvc ConfigService
	mu        sync.RWMutex
	stopChan  chan struct{}
}

// NewStateManager creates a new state manager.
func NewStateManager(store StateStore, configSvc ConfigService) *DefaultStateManager {
	return &DefaultStateManager{
		store:     store,
		configSvc: configSvc,
		stopChan:  make(chan struct{}),
	}
}

func (m *DefaultStateManager) GetOverview(ctx context.Context) (*StateOverview, error) {
	settings, err := m.configSvc.GetSettings(ctx)
	if err != nil {
		return nil, err
	}

	routes, err := m.configSvc.ListRoutes(ctx)
	if err != nil {
		return nil, err
	}

	overview := &StateOverview{
		UnifiedRoutingEnabled: settings.Enabled,
		HideOriginalModels:    settings.HideOriginalModels,
		TotalRoutes:           len(routes),
		Routes:                make([]RouteState, 0, len(routes)),
	}

	for _, route := range routes {
		routeState, err := m.GetRouteState(ctx, route.ID)
		if err != nil {
			continue
		}

		switch routeState.Status {
		case "healthy":
			overview.HealthyRoutes++
		case "degraded":
			overview.DegradedRoutes++
		case "unhealthy":
			overview.UnhealthyRoutes++
		}

		overview.Routes = append(overview.Routes, *routeState)
	}

	return overview, nil
}

func (m *DefaultStateManager) GetRouteState(ctx context.Context, routeID string) (*RouteState, error) {
	route, err := m.configSvc.GetRoute(ctx, routeID)
	if err != nil {
		return nil, err
	}

	pipeline, err := m.configSvc.GetPipeline(ctx, routeID)
	if err != nil {
		return nil, err
	}

	routeState := &RouteState{
		RouteID:     route.ID,
		RouteName:   route.Name,
		ActiveLayer: 1,
		LayerStates: make([]LayerState, 0, len(pipeline.Layers)),
	}

	healthyTargets := 0
	totalTargets := 0
	activeLayerFound := false

	for _, layer := range pipeline.Layers {
		layerState := LayerState{
			Level:        layer.Level,
			Status:       "standby",
			TargetStates: make([]*TargetState, 0, len(layer.Targets)),
		}

		healthyInLayer := 0
		for _, target := range layer.Targets {
			totalTargets++
			state, _ := m.store.GetTargetState(ctx, target.ID)
			if state == nil {
				state = &TargetState{
					TargetID: target.ID,
					Status:   StatusHealthy,
				}
			}

			if state.Status == StatusHealthy {
				healthyTargets++
				healthyInLayer++
			}

			layerState.TargetStates = append(layerState.TargetStates, state)
		}

		// Determine layer status
		if healthyInLayer > 0 && !activeLayerFound {
			layerState.Status = "active"
			routeState.ActiveLayer = layer.Level
			activeLayerFound = true
		} else if healthyInLayer == 0 {
			layerState.Status = "exhausted"
		}

		routeState.LayerStates = append(routeState.LayerStates, layerState)
	}

	// Determine overall route status
	if healthyTargets == totalTargets {
		routeState.Status = "healthy"
	} else if healthyTargets == 0 {
		routeState.Status = "unhealthy"
	} else {
		routeState.Status = "degraded"
	}

	return routeState, nil
}

func (m *DefaultStateManager) GetTargetState(ctx context.Context, targetID string) (*TargetState, error) {
	state, err := m.store.GetTargetState(ctx, targetID)
	if err != nil {
		return nil, err
	}

	return state, nil
}

func (m *DefaultStateManager) ListTargetStates(ctx context.Context) ([]*TargetState, error) {
	return m.store.ListTargetStates(ctx)
}

func (m *DefaultStateManager) RecordSuccess(ctx context.Context, targetID string, latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, _ := m.store.GetTargetState(ctx, targetID)
	if state == nil {
		state = &TargetState{TargetID: targetID}
	}

	now := time.Now()
	state.Status = StatusHealthy
	state.ConsecutiveFailures = 0
	state.LastSuccessAt = &now
	state.CooldownEndsAt = nil
	state.TotalRequests++
	state.SuccessfulRequests++

	_ = m.store.SetTargetState(ctx, state)
}

func (m *DefaultStateManager) RecordFailure(ctx context.Context, targetID string, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, _ := m.store.GetTargetState(ctx, targetID)
	if state == nil {
		state = &TargetState{TargetID: targetID}
	}

	now := time.Now()
	state.ConsecutiveFailures++
	state.LastFailureAt = &now
	state.LastFailureReason = reason
	state.TotalRequests++

	_ = m.store.SetTargetState(ctx, state)
}

func (m *DefaultStateManager) StartCooldownTimed(ctx context.Context, targetID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, _ := m.store.GetTargetState(ctx, targetID)
	if state == nil {
		state = &TargetState{TargetID: targetID}
	}

	interval := 30 * time.Second
	if cfg, _ := m.configSvc.GetHealthCheckConfig(ctx); cfg != nil && cfg.CheckIntervalSeconds > 0 {
		interval = time.Duration(cfg.CheckIntervalSeconds) * time.Second
	}
	nextCheck := time.Now().Add(interval)
	state.Status = StatusCooling
	state.CooldownEndsAt = &nextCheck

	_ = m.store.SetTargetState(ctx, state)
}

func (m *DefaultStateManager) StartCooldownUntimed(ctx context.Context, targetID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, _ := m.store.GetTargetState(ctx, targetID)
	if state == nil {
		state = &TargetState{TargetID: targetID}
	}

	state.Status = StatusCooling
	state.CooldownEndsAt = nil

	_ = m.store.SetTargetState(ctx, state)
}

func (m *DefaultStateManager) StartChecking(ctx context.Context, targetID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, _ := m.store.GetTargetState(ctx, targetID)
	if state == nil {
		state = &TargetState{TargetID: targetID}
	}

	state.Status = StatusChecking
	state.CooldownEndsAt = nil

	_ = m.store.SetTargetState(ctx, state)
}

func (m *DefaultStateManager) SetCooldownNextCheckIn(ctx context.Context, targetID string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, _ := m.store.GetTargetState(ctx, targetID)
	if state == nil || (state.Status != StatusCooling && state.Status != StatusChecking) {
		return
	}
	next := time.Now().Add(d)
	state.Status = StatusCooling
	state.CooldownEndsAt = &next
	_ = m.store.SetTargetState(ctx, state)
}

func (m *DefaultStateManager) EndCooldown(ctx context.Context, targetID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, _ := m.store.GetTargetState(ctx, targetID)
	if state == nil {
		return
	}

	state.Status = StatusHealthy
	state.CooldownEndsAt = nil

	_ = m.store.SetTargetState(ctx, state)
}

func (m *DefaultStateManager) ResetTarget(ctx context.Context, targetID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	state := &TargetState{
		TargetID:            targetID,
		Status:              StatusHealthy,
		ConsecutiveFailures: 0,
		CooldownEndsAt:      nil,
	}

	return m.store.SetTargetState(ctx, state)
}

func (m *DefaultStateManager) ForceCooldown(ctx context.Context, targetID string) error {
	m.StartCooldownUntimed(ctx, targetID)
	return nil
}

func (m *DefaultStateManager) InitializeTarget(ctx context.Context, targetID string) error {
	state := &TargetState{
		TargetID: targetID,
		Status:   StatusHealthy,
	}
	return m.store.SetTargetState(ctx, state)
}

func (m *DefaultStateManager) RemoveTarget(ctx context.Context, targetID string) error {
	return m.store.DeleteTargetState(ctx, targetID)
}

// Stop stops the state manager background tasks.
func (m *DefaultStateManager) Stop() {
	close(m.stopChan)
}

// IsTargetAvailable checks if a target is available for routing.
func (m *DefaultStateManager) IsTargetAvailable(ctx context.Context, targetID string) bool {
	state, err := m.GetTargetState(ctx, targetID)
	if err != nil {
		return true // Default to available if error
	}
	return state.Status == StatusHealthy
}

// GetAvailableTargetsInLayer returns available targets in a layer.
func (m *DefaultStateManager) GetAvailableTargetsInLayer(ctx context.Context, layer *Layer) []Target {
	available := make([]Target, 0, len(layer.Targets))
	for _, target := range layer.Targets {
		if !target.Enabled {
			continue
		}
		if m.IsTargetAvailable(ctx, target.ID) {
			available = append(available, target)
		}
	}
	return available
}
