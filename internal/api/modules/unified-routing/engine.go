package unifiedrouting

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// RoutingEngine is the core routing engine for unified routing.
type RoutingEngine interface {
	// Route determines the routing decision for a given model name.
	Route(ctx context.Context, modelName string) (*RoutingDecision, error)

	// IsEnabled returns whether unified routing is enabled.
	IsEnabled(ctx context.Context) bool

	// ShouldHideOriginalModels returns whether original models should be hidden.
	ShouldHideOriginalModels(ctx context.Context) bool

	// GetRouteNames returns all configured route names.
	GetRouteNames(ctx context.Context) []string

	// Reload reloads the engine configuration.
	Reload(ctx context.Context) error

	// GetRoutingTarget returns the target model and credential for a route alias.
	// Returns the target model name, credential ID, and any error.
	// If modelName is not a route alias, returns RouteNotFoundError.
	GetRoutingTarget(ctx context.Context, modelName string) (targetModel string, credentialID string, err error)

	// SelectTarget selects the next target from a layer based on the load balancing strategy.
	SelectTarget(ctx context.Context, routeID string, layer *Layer) (*Target, error)

	// AdvanceRoundRobin increments the round-robin counter for a layer.
	// Call once per new request before the retry loop.
	AdvanceRoundRobin(routeID string, level int)
}

// RoutingDecision represents the decision made by the routing engine.
type RoutingDecision struct {
	RouteID    string
	RouteName  string
	InputModel string
	TraceID    string
	Pipeline   *Pipeline
}

// DefaultRoutingEngine implements RoutingEngine.
type DefaultRoutingEngine struct {
	configSvc     ConfigService
	stateMgr      StateManager
	metrics       MetricsCollector
	authManager   *coreauth.Manager
	routeActivity *RouteActivityTracker
	healthChecker HealthChecker

	mu            sync.RWMutex
	routeIndex    map[string]*Route    // name -> route
	pipelineIndex map[string]*Pipeline // routeID -> pipeline
	rrCounters    map[string]*atomic.Uint64
}

// NewRoutingEngine creates a new routing engine.
func NewRoutingEngine(
	configSvc ConfigService,
	stateMgr StateManager,
	metrics MetricsCollector,
	authManager *coreauth.Manager,
	routeActivity *RouteActivityTracker,
	healthChecker HealthChecker,
) *DefaultRoutingEngine {
	if routeActivity == nil {
		routeActivity = NewRouteActivityTracker()
	}
	engine := &DefaultRoutingEngine{
		configSvc:     configSvc,
		stateMgr:      stateMgr,
		metrics:       metrics,
		authManager:   authManager,
		routeActivity: routeActivity,
		healthChecker: healthChecker,
		routeIndex:    make(map[string]*Route),
		pipelineIndex: make(map[string]*Pipeline),
		rrCounters:    make(map[string]*atomic.Uint64),
	}

	// Subscribe to config changes
	configSvc.Subscribe(func(event ConfigChangeEvent) {
		_ = engine.Reload(context.Background())
	})

	// Initial load
	_ = engine.Reload(context.Background())

	return engine
}

func (e *DefaultRoutingEngine) Route(ctx context.Context, modelName string) (*RoutingDecision, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	// Look up route by name (case-insensitive)
	route, ok := e.routeIndex[strings.ToLower(modelName)]
	if !ok {
		return nil, &RouteNotFoundError{ModelName: modelName}
	}

	if !route.Enabled {
		return nil, &RouteDisabledError{RouteName: route.Name}
	}

	pipeline, ok := e.pipelineIndex[route.ID]
	if !ok || len(pipeline.Layers) == 0 {
		return nil, &PipelineEmptyError{RouteID: route.ID}
	}

	return &RoutingDecision{
		RouteID:    route.ID,
		RouteName:  route.Name,
		InputModel: modelName,
		TraceID:    "trace-" + generateShortID(),
		Pipeline:   pipeline,
	}, nil
}

func (e *DefaultRoutingEngine) IsEnabled(ctx context.Context) bool {
	settings, err := e.configSvc.GetSettings(ctx)
	if err != nil {
		return false
	}
	return settings.Enabled
}

func (e *DefaultRoutingEngine) ShouldHideOriginalModels(ctx context.Context) bool {
	settings, err := e.configSvc.GetSettings(ctx)
	if err != nil {
		return false
	}
	return settings.Enabled && settings.HideOriginalModels
}

func (e *DefaultRoutingEngine) GetRouteNames(ctx context.Context) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	// Deduplicate: routeIndex maps multiple keys (name + aliases) to the same route
	seen := make(map[string]bool)
	var names []string
	for _, route := range e.routeIndex {
		if !route.Enabled {
			continue
		}
		for _, name := range route.AllNames() {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	return names
}

// GetRoutingTarget returns the target model and credential for a route alias.
func (e *DefaultRoutingEngine) GetRoutingTarget(ctx context.Context, modelName string) (string, string, error) {
	decision, err := e.Route(ctx, modelName)
	if err != nil {
		return "", "", err
	}

	// Select target from the first available layer
	for _, layer := range decision.Pipeline.Layers {
		target, err := e.SelectTarget(ctx, decision.RouteID, &layer)
		if err != nil {
			continue // Try next layer
		}
		if target != nil {
			return target.Model, target.CredentialID, nil
		}
	}

	return "", "", &NoAvailableTargetsError{Layer: 0}
}

// GetRoutingDecision returns the full routing decision for a model name.
func (e *DefaultRoutingEngine) GetRoutingDecision(ctx context.Context, modelName string) (*RoutingDecision, error) {
	return e.Route(ctx, modelName)
}

func (e *DefaultRoutingEngine) Reload(ctx context.Context) error {
	routes, err := e.configSvc.ListRoutes(ctx)
	if err != nil {
		return err
	}

	newRouteIndex := make(map[string]*Route, len(routes))
	newPipelineIndex := make(map[string]*Pipeline, len(routes))

	for _, route := range routes {
		// Index by primary name and all aliases
		for _, name := range route.AllNames() {
			newRouteIndex[strings.ToLower(name)] = route
		}

		pipeline, err := e.configSvc.GetPipeline(ctx, route.ID)
		if err != nil {
			pipeline = &Pipeline{RouteID: route.ID, Layers: []Layer{}}
		}
		newPipelineIndex[route.ID] = pipeline
	}

	e.mu.Lock()
	e.routeIndex = newRouteIndex
	e.pipelineIndex = newPipelineIndex
	e.mu.Unlock()

	log.Debugf("unified routing engine reloaded: %d routes", len(routes))
	return nil
}

// SelectTarget selects the next target from a layer based on the strategy.
// AdvanceRoundRobin increments the round-robin counter for a layer.
// Call once per new request before entering the retry loop;
// SelectTarget itself reads the counter without incrementing.
func (e *DefaultRoutingEngine) AdvanceRoundRobin(routeID string, level int) {
	key := fmt.Sprintf("%s:%d", routeID, level)

	e.mu.Lock()
	counter, ok := e.rrCounters[key]
	if !ok {
		counter = &atomic.Uint64{}
		e.rrCounters[key] = counter
	}
	e.mu.Unlock()

	counter.Add(1)
}

func (e *DefaultRoutingEngine) SelectTarget(ctx context.Context, routeID string, layer *Layer) (*Target, error) {
	availableTargets := make([]Target, 0)
	for _, target := range layer.Targets {
		if !target.Enabled {
			continue
		}
		state, _ := e.stateMgr.GetTargetState(ctx, target.ID)
		if state != nil && state.Status != StatusHealthy {
			continue
		}
		availableTargets = append(availableTargets, target)
	}

	if len(availableTargets) == 0 {
		return nil, &NoAvailableTargetsError{Layer: layer.Level}
	}

	var selected *Target
	switch layer.Strategy {
	case StrategyRoundRobin, "":
		selected = e.selectRoundRobin(routeID, layer.Level, availableTargets)
	case StrategyWeightedRound:
		selected = e.selectWeightedRoundRobin(routeID, layer.Level, availableTargets)
	case StrategyRandom:
		selected = e.selectRandom(availableTargets)
	case StrategyFirstAvailable:
		selected = &availableTargets[0]
	case StrategyLeastConn:
		selected = e.selectLeastConnections(ctx, availableTargets)
	default:
		selected = e.selectRoundRobin(routeID, layer.Level, availableTargets)
	}

	return selected, nil
}

func (e *DefaultRoutingEngine) selectRoundRobin(routeID string, level int, targets []Target) *Target {
	key := fmt.Sprintf("%s:%d", routeID, level)

	e.mu.Lock()
	counter, ok := e.rrCounters[key]
	if !ok {
		counter = &atomic.Uint64{}
		e.rrCounters[key] = counter
	}
	e.mu.Unlock()

	val := counter.Load()
	if val == 0 {
		val = 1
	}
	return &targets[int(val-1)%len(targets)]
}

func (e *DefaultRoutingEngine) selectWeightedRoundRobin(routeID string, level int, targets []Target) *Target {
	// Calculate total weight
	totalWeight := 0
	for _, t := range targets {
		weight := t.Weight
		if weight <= 0 {
			weight = 1
		}
		totalWeight += weight
	}

	key := fmt.Sprintf("%s:%d:weighted", routeID, level)

	e.mu.Lock()
	counter, ok := e.rrCounters[key]
	if !ok {
		counter = &atomic.Uint64{}
		e.rrCounters[key] = counter
	}
	e.mu.Unlock()

	val := counter.Load()
	if val == 0 {
		val = 1
	}
	idx := int(val-1) % totalWeight

	// Find the target
	cumulative := 0
	for i := range targets {
		weight := targets[i].Weight
		if weight <= 0 {
			weight = 1
		}
		cumulative += weight
		if idx < cumulative {
			return &targets[i]
		}
	}

	return &targets[0]
}

func (e *DefaultRoutingEngine) selectRandom(targets []Target) *Target {
	idx := rand.Intn(len(targets))
	return &targets[idx]
}

func (e *DefaultRoutingEngine) selectLeastConnections(ctx context.Context, targets []Target) *Target {
	var minConn int64 = -1
	var selected *Target

	for i := range targets {
		state, _ := e.stateMgr.GetTargetState(ctx, targets[i].ID)
		conn := int64(0)
		if state != nil {
			conn = state.ActiveConnections
		}

		if minConn < 0 || conn < minConn {
			minConn = conn
			selected = &targets[i]
		}
	}

	if selected == nil {
		return &targets[0]
	}
	return selected
}

// failoverFirstChunkTimeout is the maximum time to wait for the first stream chunk
// during failover. If the target doesn't return any data within this period,
// it is considered unresponsive and the next target is tried.
const failoverFirstChunkTimeout = 15 * time.Second

// failoverNonStreamTimeout is the maximum time for a single non-streaming request
// attempt during failover. Non-streaming requests must receive the full response
// body, so this is set higher than the streaming first-chunk timeout.
const failoverNonStreamTimeout = 30 * time.Second

// filterAvailableTargets returns enabled, healthy targets from a layer.
func (e *DefaultRoutingEngine) filterAvailableTargets(ctx context.Context, layer *Layer) []Target {
	available := make([]Target, 0, len(layer.Targets))
	for _, target := range layer.Targets {
		if !target.Enabled {
			continue
		}
		state, _ := e.stateMgr.GetTargetState(ctx, target.ID)
		if state != nil && state.Status != StatusHealthy {
			continue
		}
		available = append(available, target)
	}
	return available
}

// selectStartIndex determines the starting index in the available targets
// slice based on the layer's load-balancing strategy. This is called once
// per layer; the failover loop then iterates sequentially from this position.
func (e *DefaultRoutingEngine) selectStartIndex(routeID string, level int, strategy LoadStrategy, ctx context.Context, targets []Target) int {
	if len(targets) == 0 {
		return 0
	}

	switch strategy {
	case StrategyRoundRobin, "":
		key := fmt.Sprintf("%s:%d", routeID, level)
		e.mu.Lock()
		counter, ok := e.rrCounters[key]
		if !ok {
			counter = &atomic.Uint64{}
			e.rrCounters[key] = counter
		}
		e.mu.Unlock()
		val := counter.Load()
		if val == 0 {
			val = 1
		}
		return int(val-1) % len(targets)

	case StrategyWeightedRound:
		selected := e.selectWeightedRoundRobin(routeID, level, targets)
		for i := range targets {
			if targets[i].ID == selected.ID {
				return i
			}
		}
		return 0

	case StrategyRandom:
		return rand.Intn(len(targets))

	case StrategyFirstAvailable:
		return 0

	case StrategyLeastConn:
		selected := e.selectLeastConnections(ctx, targets)
		for i := range targets {
			if targets[i].ID == selected.ID {
				return i
			}
		}
		return 0

	default:
		key := fmt.Sprintf("%s:%d", routeID, level)
		e.mu.Lock()
		counter, ok := e.rrCounters[key]
		if !ok {
			counter = &atomic.Uint64{}
			e.rrCounters[key] = counter
		}
		e.mu.Unlock()
		val := counter.Load()
		if val == 0 {
			val = 1
		}
		return int(val-1) % len(targets)
	}
}

// ExecuteWithFailover executes a request with automatic failover.
func (e *DefaultRoutingEngine) ExecuteWithFailover(
	ctx context.Context,
	decision *RoutingDecision,
	executeFunc func(ctx context.Context, auth *coreauth.Auth, model string) error,
) error {
	if decision == nil || decision.Pipeline == nil {
		return fmt.Errorf("invalid routing decision")
	}

	e.routeActivity.Mark(decision.RouteID)
	if e.healthChecker != nil {
		go e.healthChecker.TriggerCheckUntimedCoolingTargets(ctx, decision.RouteID)
	}

	traceBuilder := NewTraceBuilder(decision.RouteID, decision.RouteName, decision.InputModel)
	startTime := time.Now()

	// Try each layer in order
	for layerIdx, layer := range decision.Pipeline.Layers {
		e.AdvanceRoundRobin(decision.RouteID, layer.Level)

		availableTargets := e.filterAvailableTargets(ctx, &layer)
		idx := e.selectStartIndex(decision.RouteID, layer.Level, layer.Strategy, ctx, availableTargets)

		for len(availableTargets) > 0 {
			if idx >= len(availableTargets) {
				idx = 0
			}
			target := availableTargets[idx]

			auth := e.findAuth(target.CredentialID)
			if auth == nil {
				traceBuilder.AddAttempt(layer.Level, target.ID, target.CredentialID, target.Model).
					Failed("credential not found")
				e.stateMgr.StartCooldownTimed(ctx, target.ID)
				e.healthChecker.ScheduleTargetCheck(target.ID)
				availableTargets = append(availableTargets[:idx], availableTargets[idx+1:]...)
				continue
			}

			attemptStart := time.Now()
			execCtx, execCancel := context.WithTimeout(ctx, failoverNonStreamTimeout)
			err := executeFunc(execCtx, auth, target.Model)
			execCancel()
			attemptLatency := time.Since(attemptStart).Milliseconds()

			if err == nil {
				e.stateMgr.RecordSuccess(ctx, target.ID, time.Since(attemptStart))
				traceBuilder.AddAttempt(layer.Level, target.ID, target.CredentialID, target.Model).
					Success(attemptLatency)

				trace := traceBuilder.Build(time.Since(startTime).Milliseconds())
				e.metrics.RecordRequest(trace)
				return nil
			}

			errClass := ClassifyError(err)

			if errClass == ErrorClassNonRetryable {
				traceBuilder.AddAttempt(layer.Level, target.ID, target.CredentialID, target.Model).
					Failed(err.Error(), attemptLatency)
				e.metrics.RecordEvent(&RoutingEvent{
					Type:    EventNonRetryableError,
					RouteID: decision.RouteID,
					Details: map[string]any{
						"error":       err.Error(),
						"error_class": errClass.String(),
					},
				})
				log.Debugf("[UnifiedRouting] Non-retryable error, returning immediately: %v", err)
				trace := traceBuilder.Build(time.Since(startTime).Milliseconds())
				e.metrics.RecordRequest(trace)
				return err
			}

			e.stateMgr.RecordFailure(ctx, target.ID, err.Error())
			traceBuilder.AddAttempt(layer.Level, target.ID, target.CredentialID, target.Model).
				Failed(err.Error(), attemptLatency)
			e.stateMgr.StartCooldownTimed(ctx, target.ID)
			e.healthChecker.ScheduleTargetCheck(target.ID)
			e.metrics.RecordEvent(&RoutingEvent{
				Type:     EventCooldownStarted,
				RouteID:  decision.RouteID,
				TargetID: target.ID,
				Details: map[string]any{
					"reason":      err.Error(),
					"error_class": errClass.String(),
				},
			})

			availableTargets = append(availableTargets[:idx], availableTargets[idx+1:]...)
		}

		// Record layer fallback event when moving to next layer
		if layerIdx < len(decision.Pipeline.Layers)-1 {
			e.metrics.RecordEvent(&RoutingEvent{
				Type:    EventLayerFallback,
				RouteID: decision.RouteID,
				Details: map[string]any{
					"from_layer": layer.Level,
					"to_layer":   layer.Level + 1,
				},
			})
		}
	}

	// All layers exhausted
	trace := traceBuilder.Build(time.Since(startTime).Milliseconds())
	e.metrics.RecordRequest(trace)

	return &AllTargetsExhaustedError{RouteID: decision.RouteID}
}

// StreamExecuteFunc is the function type for streaming execution.
// It returns a channel of StreamChunks and an error if connection fails.
type StreamExecuteFunc func(ctx context.Context, auth *coreauth.Auth, model string) (<-chan cliproxyexecutor.StreamChunk, error)

// ExecuteStreamWithFailover executes a streaming request with automatic failover.
// Failover only occurs before the first successful chunk is received.
// Once streaming begins, the target is committed and cannot be changed.
func (e *DefaultRoutingEngine) ExecuteStreamWithFailover(
	ctx context.Context,
	decision *RoutingDecision,
	executeFunc StreamExecuteFunc,
) (<-chan cliproxyexecutor.StreamChunk, error) {
	if decision == nil || decision.Pipeline == nil {
		return nil, fmt.Errorf("invalid routing decision")
	}

	e.routeActivity.Mark(decision.RouteID)
	if e.healthChecker != nil {
		go e.healthChecker.TriggerCheckUntimedCoolingTargets(ctx, decision.RouteID)
	}

	traceBuilder := NewTraceBuilder(decision.RouteID, decision.RouteName, decision.InputModel)
	startTime := time.Now()

	// Try each layer in order
	for layerIdx, layer := range decision.Pipeline.Layers {
		e.AdvanceRoundRobin(decision.RouteID, layer.Level)

		availableTargets := e.filterAvailableTargets(ctx, &layer)
		idx := e.selectStartIndex(decision.RouteID, layer.Level, layer.Strategy, ctx, availableTargets)

		for len(availableTargets) > 0 {
			if idx >= len(availableTargets) {
				idx = 0
			}
			target := availableTargets[idx]

			auth := e.findAuth(target.CredentialID)
			if auth == nil {
				traceBuilder.AddAttempt(layer.Level, target.ID, target.CredentialID, target.Model).
					Failed("credential not found")
				e.stateMgr.StartCooldownTimed(ctx, target.ID)
				e.healthChecker.ScheduleTargetCheck(target.ID)
				availableTargets = append(availableTargets[:idx], availableTargets[idx+1:]...)
				continue
			}

			attemptStart := time.Now()

			type streamConnResult struct {
				chunks <-chan cliproxyexecutor.StreamChunk
				err    error
			}
			connCh := make(chan streamConnResult, 1)
			go func() {
				c, e := executeFunc(ctx, auth, target.Model)
				connCh <- streamConnResult{c, e}
			}()

			firstChunkTimer := time.NewTimer(failoverFirstChunkTimeout)

			var chunks <-chan cliproxyexecutor.StreamChunk
			var connTimedOut bool
			select {
			case res := <-connCh:
				if res.err != nil {
					firstChunkTimer.Stop()
					errClass := ClassifyError(res.err)

					if errClass == ErrorClassNonRetryable {
						connLatency := time.Since(attemptStart).Milliseconds()
						traceBuilder.AddAttempt(layer.Level, target.ID, target.CredentialID, target.Model).
							Failed(res.err.Error(), connLatency)
						e.metrics.RecordEvent(&RoutingEvent{
							Type:    EventNonRetryableError,
							RouteID: decision.RouteID,
							Details: map[string]any{
								"error":       res.err.Error(),
								"error_class": errClass.String(),
							},
						})
						log.Debugf("[UnifiedRouting] Stream: non-retryable error, returning immediately: %v", res.err)
						trace := traceBuilder.Build(time.Since(startTime).Milliseconds())
						e.metrics.RecordRequest(trace)
						return nil, res.err
					}

					connLatency := time.Since(attemptStart).Milliseconds()
					e.stateMgr.RecordFailure(ctx, target.ID, res.err.Error())
					traceBuilder.AddAttempt(layer.Level, target.ID, target.CredentialID, target.Model).
						Failed(res.err.Error(), connLatency)
					e.stateMgr.StartCooldownTimed(ctx, target.ID)
					e.healthChecker.ScheduleTargetCheck(target.ID)
					e.metrics.RecordEvent(&RoutingEvent{
						Type:     EventCooldownStarted,
						RouteID:  decision.RouteID,
						TargetID: target.ID,
						Details: map[string]any{
							"reason":      res.err.Error(),
							"error_class": errClass.String(),
							"latency_ms":  time.Since(attemptStart).Milliseconds(),
						},
					})
					availableTargets = append(availableTargets[:idx], availableTargets[idx+1:]...)
					continue
				}
				chunks = res.chunks
			case <-firstChunkTimer.C:
				connTimedOut = true
			}

			if connTimedOut {
				attemptLatency := time.Since(attemptStart).Milliseconds()
				errMsg := fmt.Sprintf("connection timeout (%s)", failoverFirstChunkTimeout)
				e.stateMgr.RecordFailure(ctx, target.ID, errMsg)
				traceBuilder.AddAttempt(layer.Level, target.ID, target.CredentialID, target.Model).
					Failed(errMsg, attemptLatency)
				e.stateMgr.StartCooldownTimed(ctx, target.ID)
				e.healthChecker.ScheduleTargetCheck(target.ID)
				e.metrics.RecordEvent(&RoutingEvent{
					Type:     EventCooldownStarted,
					RouteID:  decision.RouteID,
					TargetID: target.ID,
					Details: map[string]any{
						"reason":     errMsg,
						"latency_ms": attemptLatency,
					},
				})
				go func() {
					res := <-connCh
					if res.chunks != nil {
						for range res.chunks {
						}
					}
				}()
				availableTargets = append(availableTargets[:idx], availableTargets[idx+1:]...)
				continue
			}

			var firstChunk cliproxyexecutor.StreamChunk
			var ok bool

			select {
			case firstChunk, ok = <-chunks:
				firstChunkTimer.Stop()
			case <-firstChunkTimer.C:
				attemptLatency := time.Since(attemptStart).Milliseconds()
				errMsg := fmt.Sprintf("first chunk timeout (%s)", failoverFirstChunkTimeout)
				e.stateMgr.RecordFailure(ctx, target.ID, errMsg)
				traceBuilder.AddAttempt(layer.Level, target.ID, target.CredentialID, target.Model).
					Failed(errMsg, attemptLatency)
				e.stateMgr.StartCooldownTimed(ctx, target.ID)
				e.healthChecker.ScheduleTargetCheck(target.ID)
				e.metrics.RecordEvent(&RoutingEvent{
					Type:     EventCooldownStarted,
					RouteID:  decision.RouteID,
					TargetID: target.ID,
					Details: map[string]any{
						"reason":     errMsg,
						"latency_ms": attemptLatency,
					},
				})
				go func() {
					for range chunks {
					}
				}()
				availableTargets = append(availableTargets[:idx], availableTargets[idx+1:]...)
				continue
			}

			if !ok {
				attemptLatency := time.Since(attemptStart).Milliseconds()
				e.stateMgr.RecordFailure(ctx, target.ID, "stream closed without data")
				traceBuilder.AddAttempt(layer.Level, target.ID, target.CredentialID, target.Model).
					Failed("stream closed without data", attemptLatency)
				e.stateMgr.StartCooldownTimed(ctx, target.ID)
				e.healthChecker.ScheduleTargetCheck(target.ID)
				e.metrics.RecordEvent(&RoutingEvent{
					Type:     EventCooldownStarted,
					RouteID:  decision.RouteID,
					TargetID: target.ID,
					Details: map[string]any{
						"reason":     "stream closed without data",
						"latency_ms": attemptLatency,
					},
				})
				availableTargets = append(availableTargets[:idx], availableTargets[idx+1:]...)
				continue
			}

			if firstChunk.Err != nil {
				chunkErrClass := ClassifyError(firstChunk.Err)
				errMsg := firstChunk.Err.Error()

				go func() {
					for range chunks {
					}
				}()

				attemptLatency := time.Since(attemptStart).Milliseconds()

				if chunkErrClass == ErrorClassNonRetryable {
					traceBuilder.AddAttempt(layer.Level, target.ID, target.CredentialID, target.Model).
						Failed(errMsg, attemptLatency)
					e.metrics.RecordEvent(&RoutingEvent{
						Type:    EventNonRetryableError,
						RouteID: decision.RouteID,
						Details: map[string]any{
							"error":       errMsg,
							"error_class": chunkErrClass.String(),
						},
					})
					log.Debugf("[UnifiedRouting] Stream first chunk: non-retryable error, returning immediately: %v", firstChunk.Err)
					trace := traceBuilder.Build(time.Since(startTime).Milliseconds())
					e.metrics.RecordRequest(trace)
					return nil, firstChunk.Err
				}

				e.stateMgr.RecordFailure(ctx, target.ID, errMsg)
				traceBuilder.AddAttempt(layer.Level, target.ID, target.CredentialID, target.Model).
					Failed(errMsg, attemptLatency)
				e.stateMgr.StartCooldownTimed(ctx, target.ID)
				e.healthChecker.ScheduleTargetCheck(target.ID)
				e.metrics.RecordEvent(&RoutingEvent{
					Type:     EventCooldownStarted,
					RouteID:  decision.RouteID,
					TargetID: target.ID,
					Details: map[string]any{
						"reason":      errMsg,
						"error_class": chunkErrClass.String(),
						"latency_ms":  attemptLatency,
					},
				})
				availableTargets = append(availableTargets[:idx], availableTargets[idx+1:]...)
				continue
			}

			outputChan := make(chan cliproxyexecutor.StreamChunk, 100)
			outputChan <- firstChunk

			capturedTarget := target
			capturedAttemptStart := attemptStart

			go func() {
				defer close(outputChan)

				var streamErr error
				for chunk := range chunks {
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
					outputChan <- chunk
				}

				attemptLatency := time.Since(capturedAttemptStart).Milliseconds()
				if streamErr != nil {
					log.Warnf("[UnifiedRouting] Stream error after successful start: %v", streamErr)
				}
				e.stateMgr.RecordSuccess(ctx, capturedTarget.ID, time.Since(capturedAttemptStart))
				traceBuilder.AddAttempt(layer.Level, capturedTarget.ID, capturedTarget.CredentialID, capturedTarget.Model).
					Success(attemptLatency)

				trace := traceBuilder.Build(time.Since(startTime).Milliseconds())
				e.metrics.RecordRequest(trace)
			}()

			return outputChan, nil
		}

		// Record layer fallback event when moving to next layer
		if layerIdx < len(decision.Pipeline.Layers)-1 {
			e.metrics.RecordEvent(&RoutingEvent{
				Type:    EventLayerFallback,
				RouteID: decision.RouteID,
				Details: map[string]any{
					"from_layer": layer.Level,
					"to_layer":   layer.Level + 1,
				},
			})
		}
	}

	// All layers exhausted
	trace := traceBuilder.Build(time.Since(startTime).Milliseconds())
	e.metrics.RecordRequest(trace)

	return nil, &AllTargetsExhaustedError{RouteID: decision.RouteID}
}

func (e *DefaultRoutingEngine) findAuth(credentialID string) *coreauth.Auth {
	if e.authManager == nil {
		return nil
	}

	auths := e.authManager.List()
	for _, auth := range auths {
		if auth.ID == credentialID {
			return auth
		}
	}
	return nil
}

// Error types

// RouteNotFoundError is returned when a route is not found.
type RouteNotFoundError struct {
	ModelName string
}

func (e *RouteNotFoundError) Error() string {
	return fmt.Sprintf("route not found for model: %s", e.ModelName)
}

// RouteDisabledError is returned when a route is disabled.
type RouteDisabledError struct {
	RouteName string
}

func (e *RouteDisabledError) Error() string {
	return fmt.Sprintf("route is disabled: %s", e.RouteName)
}

// PipelineEmptyError is returned when a pipeline has no layers.
type PipelineEmptyError struct {
	RouteID string
}

func (e *PipelineEmptyError) Error() string {
	return fmt.Sprintf("pipeline is empty for route: %s", e.RouteID)
}

// NoAvailableTargetsError is returned when no targets are available in a layer.
type NoAvailableTargetsError struct {
	Layer int
}

func (e *NoAvailableTargetsError) Error() string {
	return fmt.Sprintf("no available targets in layer %d", e.Layer)
}

// AllTargetsExhaustedError is returned when all targets in all layers are exhausted.
type AllTargetsExhaustedError struct {
	RouteID string
}

func (e *AllTargetsExhaustedError) Error() string {
	return fmt.Sprintf("all targets exhausted for route: %s", e.RouteID)
}
