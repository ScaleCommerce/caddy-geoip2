package geoip2

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/oschwald/maxminddb-golang"
	"go.uber.org/zap"
)

// GeoIP2State manages the shared GeoIP2 database state across all handler instances
// This is a Caddy app that provides centralized database management with features like:
// - Thread-safe database access
// - Automatic database reloading
// - Shared state across multiple handler instances
type GeoIP2State struct {
	// DBHandler is the MaxMind database reader instance
	// Protected by mutex for thread-safe access
	DBHandler *maxminddb.Reader `json:"-"`
	
	// mutex protects concurrent access to DBHandler
	mutex *sync.RWMutex `json:"-"`
	
	// DatabasePath is the filesystem path to the GeoIP2 database file
	// Example: "/etc/geoip/GeoLite2-City.mmdb"
	DatabasePath string `json:"database_path,omitempty"`
	
	// ReloadInterval specifies how often to reload the database (in hours)
	// 0 = no automatic reloading, manual reload via caddy admin API only
	ReloadInterval int `json:"reload_interval,omitempty"`
	
	// done channel signals the reload timer goroutine to stop
	done chan bool `json:"-"`
}

// Module name for Caddy's app registry
const (
	moduleName = "geoip2"
)

// Default configuration values
const (
	DefaultDatabasePath = "/etc/geoip/GeoLite2-City.mmdb"
	DefaultReloadHours  = 24 // Daily reload by default
)

// Module registration - called when Caddy starts
func init() {
	caddy.RegisterModule(GeoIP2State{})
	httpcaddyfile.RegisterGlobalOption("geoip2", parseGeoip2)
}

// CaddyModule returns module information for Caddy's module system
func (GeoIP2State) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "geoip2",
		New: func() caddy.Module { return new(GeoIP2State) },
	}
}

// parseGeoip2 handles the global "geoip2" directive in Caddyfile
// This creates the app configuration that will be shared across all sites
func parseGeoip2(d *caddyfile.Dispenser, _ any) (any, error) {
	state := GeoIP2State{}
	err := state.UnmarshalCaddyfile(d)
	return httpcaddyfile.App{
		Name:  "geoip2",
		Value: caddyconfig.JSON(state, nil),
	}, err
}

// Start initializes the GeoIP2 app when Caddy starts
// This method is called once when the server starts up
func (g *GeoIP2State) Start() error {
	// Initialize mutex if not already done
	if g.mutex == nil {
		g.mutex = &sync.RWMutex{}
	}
	
	caddy.Log().Named("geoip2").Info("starting GeoIP2 module",
		zap.String("database_path", g.DatabasePath),
		zap.String("reload_interval", fmt.Sprintf("%dh", g.ReloadInterval)))
	
	// Load database for the first time
	if err := g.loadDatabase(); err != nil {
		return fmt.Errorf("failed to load initial database: %v", err)
	}
	
	// Start automatic reload timer if configured
	if g.ReloadInterval > 0 {
		g.startReloadTimer()
	}
	
	return nil
}

// Stop cleanly shuts down the GeoIP2 app when Caddy stops
// This method is called when the server is shutting down
func (g *GeoIP2State) Stop() error {
	// Stop the reload timer if running
	if g.done != nil {
		close(g.done)
		caddy.Log().Named("geoip2").Debug("stopped reload timer")
	}
	
	// Close database connection
	g.mutex.Lock()
	defer g.mutex.Unlock()
	
	if g.DBHandler != nil {
		if err := g.DBHandler.Close(); err != nil {
			caddy.Log().Named("geoip2").Warn("error closing database", 
				zap.Error(err))
		}
		g.DBHandler = nil
		caddy.Log().Named("geoip2").Debug("closed database")
	}
	
	caddy.Log().Named("geoip2").Info("stopped GeoIP2 module")
	return nil
}

// UnmarshalCaddyfile parses the Caddyfile configuration for this app
// Expected format:
//   geoip2 {
//     database_path /path/to/database.mmdb
//     reload_interval daily
//   }
func (g *GeoIP2State) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	// Initialize mutex early for thread safety
	if g.mutex == nil {
		g.mutex = &sync.RWMutex{}
	}
	
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "database_path":
				if !d.Args(&g.DatabasePath) {
					return d.ArgErr()
				}
				// Expand environment variables and resolve relative paths
				g.DatabasePath = os.ExpandEnv(g.DatabasePath)
				if !filepath.IsAbs(g.DatabasePath) {
					g.DatabasePath, _ = filepath.Abs(g.DatabasePath)
				}
				
			case "reload_interval":
				var intervalStr string
				if !d.Args(&intervalStr) {
					return d.ArgErr()
				}
				
				// Parse reload interval with flexible formats
				interval, err := g.parseReloadInterval(intervalStr)
				if err != nil {
					return d.Errf("invalid reload_interval '%s': %v", intervalStr, err)
				}
				g.ReloadInterval = interval
				
			default:
				return d.Errf("unknown directive: %s", d.Val())
			}
		}
	}
	
	// Set defaults if not specified
	g.setDefaults()
	
	caddy.Log().Named("geoip2").Info("configured GeoIP2 app",
		zap.String("database_path", g.DatabasePath),
		zap.String("reload_interval", fmt.Sprintf("%dh", g.ReloadInterval)))
	
	return nil
}

// parseReloadInterval converts various interval formats to hours
// Supported formats: "daily", "24h", "1d", "2", "48"
func (g *GeoIP2State) parseReloadInterval(intervalStr string) (int, error) {
	switch intervalStr {
	case "daily", "1d", "24h":
		return 24, nil
	case "weekly", "7d", "168h":
		return 168, nil
	case "off", "disable", "0":
		return 0, nil
	default:
		// Try to parse as number of hours
		if hours, err := strconv.Atoi(intervalStr); err == nil {
			if hours < 0 {
				return 0, fmt.Errorf("reload interval cannot be negative")
			}
			return hours, nil
		}
		return 0, fmt.Errorf("invalid format, use 'daily', 'weekly', 'off', or number of hours")
	}
}

// setDefaults applies default values for unspecified configuration
func (g *GeoIP2State) setDefaults() {
	if g.DatabasePath == "" {
		g.DatabasePath = DefaultDatabasePath
	}
	// Note: ReloadInterval of 0 (no auto-reload) is a valid default
}

// loadDatabase loads or reloads the GeoIP2 database from disk
// This method is thread-safe and can be called concurrently
func (g *GeoIP2State) loadDatabase() error {
	// Validate database file exists and is readable
	if err := g.validateDatabaseFile(); err != nil {
		return err
	}
	
	// Acquire exclusive lock for database replacement
	g.mutex.Lock()
	defer g.mutex.Unlock()
	
	// Open new database instance
	newDB, err := maxminddb.Open(g.DatabasePath)
	if err != nil {
		return fmt.Errorf("failed to open database %s: %v", g.DatabasePath, err)
	}
	
	// Close old database if present
	if g.DBHandler != nil {
		if err := g.DBHandler.Close(); err != nil {
			caddy.Log().Named("geoip2").Warn("error closing old database", 
				zap.Error(err))
		}
	}
	
	// Replace with new database
	g.DBHandler = newDB
	
	// Log successful load with database metadata
	metadata := newDB.Metadata
	caddy.Log().Named("geoip2").Info("database loaded successfully",
		zap.String("path", g.DatabasePath),
		zap.Uint64("build_epoch", uint64(metadata.BuildEpoch)),
		zap.String("database_type", metadata.DatabaseType))
	
	return nil
}

// validateDatabaseFile checks if the database file exists and is accessible
func (g *GeoIP2State) validateDatabaseFile() error {
	// Check if file exists
	info, err := os.Stat(g.DatabasePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("database file not found: %s", g.DatabasePath)
		}
		return fmt.Errorf("cannot access database file %s: %v", g.DatabasePath, err)
	}
	
	// Check if it's a regular file (not directory, symlink, etc.)
	if !info.Mode().IsRegular() {
		return fmt.Errorf("database path is not a regular file: %s", g.DatabasePath)
	}
	
	// Check minimum file size (MaxMind databases are at least a few MB)
	if info.Size() < 1024*1024 { // Less than 1MB is suspicious
		return fmt.Errorf("database file appears too small: %d bytes", info.Size())
	}
	
	return nil
}

// startReloadTimer starts a background goroutine that periodically reloads the database
func (g *GeoIP2State) startReloadTimer() {
	g.done = make(chan bool, 1)
	
	go func() {
		interval := time.Duration(g.ReloadInterval) * time.Hour
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		
		caddy.Log().Named("geoip2").Info("started database reload timer",
			zap.Duration("interval", interval),
			zap.String("next_reload", time.Now().Add(interval).Format(time.RFC3339)))
		
		for {
			select {
			case <-ticker.C:
				g.performScheduledReload()
				
			case <-g.done:
				caddy.Log().Named("geoip2").Debug("reload timer stopped")
				return
			}
		}
	}()
}

// performScheduledReload handles the actual database reload with error handling
func (g *GeoIP2State) performScheduledReload() {
	caddy.Log().Named("geoip2").Info("performing scheduled database reload")
	
	startTime := time.Now()
	if err := g.loadDatabase(); err != nil {
		caddy.Log().Named("geoip2").Error("scheduled database reload failed", 
			zap.Error(err),
			zap.Duration("duration", time.Since(startTime)))
	} else {
		caddy.Log().Named("geoip2").Info("scheduled database reload completed",
			zap.Duration("duration", time.Since(startTime)),
			zap.String("next_reload", time.Now().Add(time.Duration(g.ReloadInterval)*time.Hour).Format(time.RFC3339)))
	}
}

// Lookup performs a thread-safe GeoIP lookup
// This is the main API used by the HTTP handlers
func (g *GeoIP2State) Lookup(ip interface{}, result interface{}) error {
	// Acquire read lock for database access
	g.mutex.RLock()
	defer g.mutex.RUnlock()
	
	// Check if database is available
	if g.DBHandler == nil {
		return errors.New("GeoIP2 database not loaded")
	}
	
	// Convert interface{} to net.IP if needed
	var netIP net.IP
	switch v := ip.(type) {
	case net.IP:
		netIP = v
	case string:
		netIP = net.ParseIP(v)
		if netIP == nil {
			return fmt.Errorf("invalid IP address: %s", v)
		}
	default:
		return fmt.Errorf("unsupported IP type: %T", ip)
	}
	
	// Perform the actual lookup
	return g.DBHandler.Lookup(netIP, result)
}

// GetDatabaseInfo returns information about the currently loaded database
// Useful for monitoring and debugging
func (g *GeoIP2State) GetDatabaseInfo() map[string]interface{} {
	g.mutex.RLock()
	defer g.mutex.RUnlock()
	
	info := map[string]interface{}{
		"database_path":    g.DatabasePath,
		"reload_interval":  g.ReloadInterval,
		"database_loaded":  g.DBHandler != nil,
	}
	
	if g.DBHandler != nil {
		metadata := g.DBHandler.Metadata
		info["build_epoch"] = metadata.BuildEpoch
		info["database_type"] = metadata.DatabaseType
		info["ip_version"] = metadata.IPVersion
		info["record_size"] = metadata.RecordSize
		info["node_count"] = metadata.NodeCount
	}
	
	return info
}

// Provision is called by Caddy to set up the module
func (g *GeoIP2State) Provision(ctx caddy.Context) error {
	caddy.Log().Named("geoip2").Debug("provisioning GeoIP2 app")
	return nil
}

// Validate checks if the app configuration is valid
// This is called before Start() to catch configuration errors early
func (g GeoIP2State) Validate() error {
	// Validate required configuration
	if g.DatabasePath == "" {
		return fmt.Errorf("database_path is required")
	}
	
	// Validate reload interval
	if g.ReloadInterval < 0 {
		return fmt.Errorf("reload_interval cannot be negative")
	}
	
	// Validate database file
	if err := g.validateDatabaseFile(); err != nil {
		return fmt.Errorf("database validation failed: %v", err)
	}
	
	// Test database can be opened
	db, err := maxminddb.Open(g.DatabasePath)
	if err != nil {
		return fmt.Errorf("cannot open database %s: %v", g.DatabasePath, err)
	}
	defer db.Close()
	
	// Validate database type (should be City or Country database)
	metadata := db.Metadata
	if metadata.DatabaseType != "GeoLite2-City" && 
	   metadata.DatabaseType != "GeoIP2-City" && 
	   metadata.DatabaseType != "GeoLite2-Country" && 
	   metadata.DatabaseType != "GeoIP2-Country" {
		caddy.Log().Named("geoip2").Warn("unknown database type", 
			zap.String("type", metadata.DatabaseType))
	}
	
	caddy.Log().Named("geoip2").Info("validation successful",
		zap.String("database_type", metadata.DatabaseType),
		zap.Uint64("build_epoch", uint64(metadata.BuildEpoch)))
	
	return nil
}

// Interface guards - compile-time checks that we implement required interfaces
var (
	_ caddyfile.Unmarshaler = (*GeoIP2State)(nil)
	_ caddy.Module          = (*GeoIP2State)(nil)
	_ caddy.Provisioner     = (*GeoIP2State)(nil)
	_ caddy.Validator       = (*GeoIP2State)(nil)
	_ caddy.App             = (*GeoIP2State)(nil)
)
