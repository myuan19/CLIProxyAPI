package unifiedrouting

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ConfigChangeEvent represents a configuration change event.
type ConfigChangeEvent struct {
	Type    string // "route_created", "route_updated", "route_deleted", "settings_updated", "pipeline_updated"
	RouteID string
	Payload any
}

// ConfigChangeHandler is a callback function for configuration changes.
type ConfigChangeHandler func(event ConfigChangeEvent)

// ConfigService manages unified routing configuration.
type ConfigService interface {
	// Settings
	GetSettings(ctx context.Context) (*Settings, error)
	UpdateSettings(ctx context.Context, settings *Settings) error

	// Health check config
	GetHealthCheckConfig(ctx context.Context) (*HealthCheckConfig, error)
	UpdateHealthCheckConfig(ctx context.Context, config *HealthCheckConfig) error

	// Routes
	ListRoutes(ctx context.Context) ([]*Route, error)
	GetRoute(ctx context.Context, id string) (*Route, error)
	CreateRoute(ctx context.Context, route *Route) error
	UpdateRoute(ctx context.Context, route *Route) error
	DeleteRoute(ctx context.Context, id string) error

	// Pipelines
	GetPipeline(ctx context.Context, routeID string) (*Pipeline, error)
	UpdatePipeline(ctx context.Context, routeID string, pipeline *Pipeline) error

	// Export/Import
	Export(ctx context.Context) (*ExportData, error)
	Import(ctx context.Context, data *ExportData, merge bool) error

	// Validation
	Validate(ctx context.Context, route *Route, pipeline *Pipeline) []ValidationError

	// Subscriptions
	Subscribe(handler ConfigChangeHandler)
}

// DefaultConfigService implements ConfigService.
type DefaultConfigService struct {
	store    ConfigStore
	mu       sync.RWMutex
	handlers []ConfigChangeHandler
}

// NewConfigService creates a new configuration service.
func NewConfigService(store ConfigStore) *DefaultConfigService {
	return &DefaultConfigService{
		store:    store,
		handlers: make([]ConfigChangeHandler, 0),
	}
}

func (s *DefaultConfigService) GetSettings(ctx context.Context) (*Settings, error) {
	return s.store.LoadSettings(ctx)
}

func (s *DefaultConfigService) UpdateSettings(ctx context.Context, settings *Settings) error {
	if err := s.store.SaveSettings(ctx, settings); err != nil {
		return err
	}

	s.notify(ConfigChangeEvent{
		Type:    "settings_updated",
		Payload: settings,
	})

	return nil
}

func (s *DefaultConfigService) GetHealthCheckConfig(ctx context.Context) (*HealthCheckConfig, error) {
	return s.store.LoadHealthCheckConfig(ctx)
}

func (s *DefaultConfigService) UpdateHealthCheckConfig(ctx context.Context, config *HealthCheckConfig) error {
	if err := s.store.SaveHealthCheckConfig(ctx, config); err != nil {
		return err
	}

	s.notify(ConfigChangeEvent{
		Type:    "health_config_updated",
		Payload: config,
	})

	return nil
}

func (s *DefaultConfigService) ListRoutes(ctx context.Context) ([]*Route, error) {
	return s.store.ListRoutes(ctx)
}

func (s *DefaultConfigService) GetRoute(ctx context.Context, id string) (*Route, error) {
	return s.store.GetRoute(ctx, id)
}

func (s *DefaultConfigService) CreateRoute(ctx context.Context, route *Route) error {
	// Generate ID if not provided
	if route.ID == "" {
		route.ID = "route-" + generateShortID()
	}

	// Validate route name
	if route.Name == "" {
		return fmt.Errorf("route name is required")
	}

	// Deduplicate and clean aliases (remove empty, remove duplicates with name)
	route.Aliases = cleanAliases(route.Name, route.Aliases)

	// Check for duplicate name/aliases across existing routes
	routes, err := s.store.ListRoutes(ctx)
	if err != nil {
		return err
	}
	if err := checkNameConflicts(route, routes); err != nil {
		return err
	}

	route.CreatedAt = time.Now()
	route.UpdatedAt = route.CreatedAt

	if err := s.store.CreateRoute(ctx, route); err != nil {
		return err
	}

	s.notify(ConfigChangeEvent{
		Type:    "route_created",
		RouteID: route.ID,
		Payload: route,
	})

	return nil
}

func (s *DefaultConfigService) UpdateRoute(ctx context.Context, route *Route) error {
	existing, err := s.store.GetRoute(ctx, route.ID)
	if err != nil {
		return err
	}

	// Deduplicate and clean aliases
	route.Aliases = cleanAliases(route.Name, route.Aliases)

	// Check for duplicate name/aliases across existing routes (exclude self)
	routes, err := s.store.ListRoutes(ctx)
	if err != nil {
		return err
	}
	if err := checkNameConflicts(route, routes); err != nil {
		return err
	}

	route.CreatedAt = existing.CreatedAt
	route.UpdatedAt = time.Now()

	if err := s.store.UpdateRoute(ctx, route); err != nil {
		return err
	}

	s.notify(ConfigChangeEvent{
		Type:    "route_updated",
		RouteID: route.ID,
		Payload: route,
	})

	return nil
}

func (s *DefaultConfigService) DeleteRoute(ctx context.Context, id string) error {
	if err := s.store.DeleteRoute(ctx, id); err != nil {
		return err
	}

	s.notify(ConfigChangeEvent{
		Type:    "route_deleted",
		RouteID: id,
	})

	return nil
}

func (s *DefaultConfigService) GetPipeline(ctx context.Context, routeID string) (*Pipeline, error) {
	return s.store.GetPipeline(ctx, routeID)
}

func (s *DefaultConfigService) UpdatePipeline(ctx context.Context, routeID string, pipeline *Pipeline) error {
	// Validate pipeline
	if errs := s.validatePipeline(pipeline); len(errs) > 0 {
		return fmt.Errorf("pipeline validation failed: %s", errs[0].Message)
	}

	// Normalise layer defaults – fill zero cooldown with the configured default
	// so the stored value always reflects the effective setting.
	healthCfg, _ := s.store.LoadHealthCheckConfig(ctx)
	defaultCooldown := DefaultHealthCheckConfig().DefaultCooldownSeconds
	if healthCfg != nil && healthCfg.DefaultCooldownSeconds > 0 {
		defaultCooldown = healthCfg.DefaultCooldownSeconds
	}

	// Ensure target IDs are set
	for i := range pipeline.Layers {
		if pipeline.Layers[i].CooldownSeconds <= 0 {
			pipeline.Layers[i].CooldownSeconds = defaultCooldown
		}
		for j := range pipeline.Layers[i].Targets {
			if pipeline.Layers[i].Targets[j].ID == "" {
				pipeline.Layers[i].Targets[j].ID = "target-" + generateShortID()
			}
			// Default weight to 1
			if pipeline.Layers[i].Targets[j].Weight <= 0 {
				pipeline.Layers[i].Targets[j].Weight = 1
			}
		}
	}

	if err := s.store.SavePipeline(ctx, routeID, pipeline); err != nil {
		return err
	}

	s.notify(ConfigChangeEvent{
		Type:    "pipeline_updated",
		RouteID: routeID,
		Payload: pipeline,
	})

	return nil
}

func (s *DefaultConfigService) Export(ctx context.Context) (*ExportData, error) {
	settings, err := s.store.LoadSettings(ctx)
	if err != nil {
		return nil, err
	}

	healthConfig, err := s.store.LoadHealthCheckConfig(ctx)
	if err != nil {
		return nil, err
	}

	routes, err := s.store.ListRoutes(ctx)
	if err != nil {
		return nil, err
	}

	var routesWithPipelines []RouteWithPipeline
	for _, route := range routes {
		pipeline, err := s.store.GetPipeline(ctx, route.ID)
		if err != nil {
			pipeline = &Pipeline{RouteID: route.ID, Layers: []Layer{}}
		}
		routesWithPipelines = append(routesWithPipelines, RouteWithPipeline{
			Route:    *route,
			Pipeline: *pipeline,
		})
	}

	return &ExportData{
		Version:    "1.0",
		ExportedAt: time.Now(),
		Config: ExportedConfig{
			Settings:    *settings,
			HealthCheck: *healthConfig,
			Routes:      routesWithPipelines,
		},
	}, nil
}

func (s *DefaultConfigService) Import(ctx context.Context, data *ExportData, merge bool) error {
	if !merge {
		// Delete all existing routes first
		routes, _ := s.store.ListRoutes(ctx)
		for _, route := range routes {
			_ = s.store.DeleteRoute(ctx, route.ID)
		}
	}

	// Import settings
	if err := s.store.SaveSettings(ctx, &data.Config.Settings); err != nil {
		return fmt.Errorf("failed to import settings: %w", err)
	}

	// Import health config
	if err := s.store.SaveHealthCheckConfig(ctx, &data.Config.HealthCheck); err != nil {
		return fmt.Errorf("failed to import health config: %w", err)
	}

	// Import routes and pipelines
	for _, rwp := range data.Config.Routes {
		route := rwp.Route

		if merge {
			// Update if exists, create if not
			_, err := s.store.GetRoute(ctx, route.ID)
			if err != nil {
				_ = s.store.CreateRoute(ctx, &route)
			} else {
				_ = s.store.UpdateRoute(ctx, &route)
			}
		} else {
			_ = s.store.CreateRoute(ctx, &route)
		}

		_ = s.store.SavePipeline(ctx, route.ID, &rwp.Pipeline)
	}

	s.notify(ConfigChangeEvent{
		Type:    "config_imported",
		Payload: data,
	})

	return nil
}

func (s *DefaultConfigService) Validate(ctx context.Context, route *Route, pipeline *Pipeline) []ValidationError {
	var errors []ValidationError

	// Validate route
	if route != nil {
		if route.Name == "" {
			errors = append(errors, ValidationError{Field: "name", Message: "route name is required"})
		}
		if len(route.Name) > 64 {
			errors = append(errors, ValidationError{Field: "name", Message: "route name must be 64 characters or less"})
		}
		// Route name should be a valid model identifier
		if !isValidModelName(route.Name) {
			errors = append(errors, ValidationError{Field: "name", Message: "route name must be alphanumeric with dashes/underscores"})
		}

		// Validate aliases
		seen := map[string]bool{strings.ToLower(route.Name): true}
		for i, alias := range route.Aliases {
			lower := strings.ToLower(alias)
			if alias == "" {
				continue // skip empty aliases
			}
			if len(alias) > 64 {
				errors = append(errors, ValidationError{Field: fmt.Sprintf("aliases[%d]", i), Message: "alias must be 64 characters or less"})
			}
			if !isValidModelName(alias) {
				errors = append(errors, ValidationError{Field: fmt.Sprintf("aliases[%d]", i), Message: fmt.Sprintf("alias '%s' must be alphanumeric with dashes/underscores/dots", alias)})
			}
			if seen[lower] {
				errors = append(errors, ValidationError{Field: fmt.Sprintf("aliases[%d]", i), Message: fmt.Sprintf("duplicate alias '%s'", alias)})
			}
			seen[lower] = true
		}
	}

	// Validate pipeline
	if pipeline != nil {
		errors = append(errors, s.validatePipeline(pipeline)...)
	}

	return errors
}

func (s *DefaultConfigService) validatePipeline(pipeline *Pipeline) []ValidationError {
	var errors []ValidationError

	if len(pipeline.Layers) == 0 {
		errors = append(errors, ValidationError{Field: "layers", Message: "at least one layer is required"})
		return errors
	}

	seenLevels := make(map[int]bool)
	for i, layer := range pipeline.Layers {
		// Check level uniqueness
		if seenLevels[layer.Level] {
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("layers[%d].level", i),
				Message: fmt.Sprintf("duplicate level %d", layer.Level),
			})
		}
		seenLevels[layer.Level] = true

		// Check targets
		if len(layer.Targets) == 0 {
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("layers[%d].targets", i),
				Message: "at least one target is required per layer",
			})
		}

		for j, target := range layer.Targets {
			if target.CredentialID == "" {
				errors = append(errors, ValidationError{
					Field:   fmt.Sprintf("layers[%d].targets[%d].credential_id", i, j),
					Message: "credential_id is required",
				})
			}
			if target.Model == "" {
				errors = append(errors, ValidationError{
					Field:   fmt.Sprintf("layers[%d].targets[%d].model", i, j),
					Message: "model is required",
				})
			}
		}

		// Validate strategy
		switch layer.Strategy {
		case StrategyRoundRobin, StrategyWeightedRound, StrategyLeastConn, StrategyRandom, StrategyFirstAvailable, "":
			// Valid
		default:
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("layers[%d].strategy", i),
				Message: fmt.Sprintf("invalid strategy: %s", layer.Strategy),
			})
		}
	}

	return errors
}

func (s *DefaultConfigService) Subscribe(handler ConfigChangeHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers = append(s.handlers, handler)
}

func (s *DefaultConfigService) notify(event ConfigChangeEvent) {
	s.mu.RLock()
	handlers := s.handlers
	s.mu.RUnlock()

	for _, handler := range handlers {
		go handler(event)
	}
}

// Helper functions

func generateShortID() string {
	id := uuid.New().String()
	return id[:8]
}

// cleanAliases deduplicates and filters aliases, removing empty strings
// and any alias that matches the route name (case-insensitive).
func cleanAliases(name string, aliases []string) []string {
	if len(aliases) == 0 {
		return nil
	}
	seen := map[string]bool{strings.ToLower(name): true}
	var result []string
	for _, a := range aliases {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		lower := strings.ToLower(a)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		result = append(result, a)
	}
	return result
}

// checkNameConflicts checks that the route's name and aliases don't conflict
// with any other route's name or aliases. Skips routes with the same ID (for updates).
func checkNameConflicts(route *Route, allRoutes []*Route) error {
	// Collect all names from the new/updated route
	newNames := make(map[string]string) // lowercase -> original
	newNames[strings.ToLower(route.Name)] = route.Name
	for _, a := range route.Aliases {
		newNames[strings.ToLower(a)] = a
	}

	for _, r := range allRoutes {
		if r.ID == route.ID {
			continue
		}
		// Check against existing route's name
		if original, ok := newNames[strings.ToLower(r.Name)]; ok {
			return fmt.Errorf("name/alias '%s' conflicts with route '%s'", original, r.Name)
		}
		// Check against existing route's aliases
		for _, a := range r.Aliases {
			if original, ok := newNames[strings.ToLower(a)]; ok {
				return fmt.Errorf("name/alias '%s' conflicts with alias '%s' on route '%s'", original, a, r.Name)
			}
		}
	}
	return nil
}

func isValidModelName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
