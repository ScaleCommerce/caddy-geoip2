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
// - Support for multiple specialized databases with intelligent routing
// - Performance optimization: EU IPs use Europe-specific database, others use global database
type GeoIP2State struct {
	// CountryDBHandler is the MaxMind Country database reader instance
	// Used for country code and EU status lookups
	CountryDBHandler *maxminddb.Reader `json:"-"`

	// CityDBHandler is the MaxMind City database reader instance (Europe-focused)
	// Used for city names, subdivisions, and geographic coordinates for European IPs
	CityDBHandler *maxminddb.Reader `json:"-"`

	// GlobalCityDBHandler is the global MaxMind City database reader instance
	// Used for city data for non-European IPs as fallback
	GlobalCityDBHandler *maxminddb.Reader `json:"-"`

	// ASNDBHandler is the MaxMind ASN database reader instance
	// Used for ASN number and organization lookups
	ASNDBHandler *maxminddb.Reader `json:"-"`

	// mutex protects concurrent access to all database handlers
	mutex *sync.RWMutex `json:"-"`

	// CountryDatabasePath is the filesystem path to the Country database file
	// Example: "/etc/nginx/maxmind-geo-ip/GeoIP-Country/GeoIP2-Country.mmdb"
	CountryDatabasePath string `json:"country_database_path,omitempty"`

	// CityDatabasePath is the filesystem path to the Europe-focused City database file
	// Example: "/etc/nginx/maxmind-geo-ip/GeoIP-Country/GeoIP2-City-Europe.mmdb"
	CityDatabasePath string `json:"city_database_path,omitempty"`

	// GlobalCityDatabasePath is the filesystem path to the global City database file
	// Example: "/etc/nginx/maxmind-geo-ip/GeoLite2-City.mmdb"
	// Used as fallback for non-European IPs
	GlobalCityDatabasePath string `json:"global_city_database_path,omitempty"`

	// ASNDatabasePath is the filesystem path to the ASN database file
	// Example: "/etc/nginx/maxmind-geo-ip/GeoLite2-ASN.mmdb"
	ASNDatabasePath string `json:"asn_database_path,omitempty"`

	// ReloadInterval specifies how often to reload the databases (in hours)
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
	DefaultReloadHours = 24 // Daily reload by default
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
		zap.String("country_database_path", g.CountryDatabasePath),
		zap.String("city_database_path", g.CityDatabasePath),
		zap.String("global_city_database_path", g.GlobalCityDatabasePath),
		zap.String("asn_database_path", g.ASNDatabasePath),
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

	if g.CountryDBHandler != nil {
		if err := g.CountryDBHandler.Close(); err != nil {
			caddy.Log().Named("geoip2").Warn("error closing country database",
				zap.Error(err))
		}
		g.CountryDBHandler = nil
		caddy.Log().Named("geoip2").Debug("closed country database")
	}
	if g.CityDBHandler != nil {
		if err := g.CityDBHandler.Close(); err != nil {
			caddy.Log().Named("geoip2").Warn("error closing city database",
				zap.Error(err))
		}
		g.CityDBHandler = nil
		caddy.Log().Named("geoip2").Debug("closed city database")
	}
	if g.GlobalCityDBHandler != nil {
		if err := g.GlobalCityDBHandler.Close(); err != nil {
			caddy.Log().Named("geoip2").Warn("error closing global city database",
				zap.Error(err))
		}
		g.GlobalCityDBHandler = nil
		caddy.Log().Named("geoip2").Debug("closed global city database")
	}
	if g.ASNDBHandler != nil {
		if err := g.ASNDBHandler.Close(); err != nil {
			caddy.Log().Named("geoip2").Warn("error closing ASN database",
				zap.Error(err))
		}
		g.ASNDBHandler = nil
		caddy.Log().Named("geoip2").Debug("closed ASN database")
	}

	caddy.Log().Named("geoip2").Info("stopped GeoIP2 module")
	return nil
}

// UnmarshalCaddyfile parses the Caddyfile configuration for this app
// Expected format:
//
//	geoip2 {
//	  country_database_path /path/to/country.mmdb
//	  city_database_path /path/to/city-europe.mmdb
//	  global_city_database_path /path/to/city-global.mmdb
//	  asn_database_path /path/to/asn.mmdb  # optional
//	  reload_interval daily
//	}
func (g *GeoIP2State) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	// Initialize mutex early for thread safety
	if g.mutex == nil {
		g.mutex = &sync.RWMutex{}
	}

	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "country_database_path":
				if !d.Args(&g.CountryDatabasePath) {
					return d.ArgErr()
				}
				// Expand environment variables and resolve relative paths
				g.CountryDatabasePath = os.ExpandEnv(g.CountryDatabasePath)
				if !filepath.IsAbs(g.CountryDatabasePath) {
					g.CountryDatabasePath, _ = filepath.Abs(g.CountryDatabasePath)
				}

			case "city_database_path":
				if !d.Args(&g.CityDatabasePath) {
					return d.ArgErr()
				}
				// Expand environment variables and resolve relative paths
				g.CityDatabasePath = os.ExpandEnv(g.CityDatabasePath)
				if !filepath.IsAbs(g.CityDatabasePath) {
					g.CityDatabasePath, _ = filepath.Abs(g.CityDatabasePath)
				}

			case "global_city_database_path":
				if !d.Args(&g.GlobalCityDatabasePath) {
					return d.ArgErr()
				}
				// Expand environment variables and resolve relative paths
				g.GlobalCityDatabasePath = os.ExpandEnv(g.GlobalCityDatabasePath)
				if !filepath.IsAbs(g.GlobalCityDatabasePath) {
					g.GlobalCityDatabasePath, _ = filepath.Abs(g.GlobalCityDatabasePath)
				}

			case "asn_database_path":
				if !d.Args(&g.ASNDatabasePath) {
					return d.ArgErr()
				}
				// Expand environment variables and resolve relative paths
				g.ASNDatabasePath = os.ExpandEnv(g.ASNDatabasePath)
				if !filepath.IsAbs(g.ASNDatabasePath) {
					g.ASNDatabasePath, _ = filepath.Abs(g.ASNDatabasePath)
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
		zap.String("country_database_path", g.CountryDatabasePath),
		zap.String("city_database_path", g.CityDatabasePath),
		zap.String("global_city_database_path", g.GlobalCityDatabasePath),
		zap.String("asn_database_path", g.ASNDatabasePath),
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
	if g.CountryDatabasePath == "" {
		g.CountryDatabasePath = "/etc/nginx/maxmind-geo-ip/GeoIP-Country/GeoIP2-Country.mmdb"
	}
	if g.CityDatabasePath == "" {
		g.CityDatabasePath = "/etc/nginx/maxmind-geo-ip/GeoIP-Country/GeoIP2-City-Europe.mmdb"
	}
	if g.GlobalCityDatabasePath == "" {
		g.GlobalCityDatabasePath = "/etc/nginx/maxmind-geo-ip/GeoLite2-City.mmdb"
	}
	// Note: ReloadInterval of 0 (no auto-reload) is a valid default
}

// loadDatabase loads or reloads the GeoIP2 database from disk
// This method is thread-safe and can be called concurrently
// Supports loading all three databases
func (g *GeoIP2State) loadDatabase() error {
	// Validate country database file exists and is readable
	if err := g.validateDatabaseFile(g.CountryDatabasePath); err != nil {
		return fmt.Errorf("country database validation failed: %v", err)
	}

	// Validate city database file exists and is readable
	if err := g.validateDatabaseFile(g.CityDatabasePath); err != nil {
		return fmt.Errorf("city database validation failed: %v", err)
	}

	// Validate global city database file exists and is readable
	if err := g.validateDatabaseFile(g.GlobalCityDatabasePath); err != nil {
		caddy.Log().Named("geoip2").Warn("global city database validation failed, global city data will be empty",
			zap.String("global_city_path", g.GlobalCityDatabasePath),
			zap.Error(err))
	}

	// Validate ASN database if specified
	var asnDBValid bool
	if g.ASNDatabasePath != "" {
		if err := g.validateDatabaseFile(g.ASNDatabasePath); err != nil {
			caddy.Log().Named("geoip2").Warn("ASN database validation failed, ASN data will be empty",
				zap.String("asn_path", g.ASNDatabasePath),
				zap.Error(err))
		} else {
			asnDBValid = true
		}
	}

	// Acquire exclusive lock for database replacement
	g.mutex.Lock()
	defer g.mutex.Unlock()

	// Open new country database instance
	newCountryDB, err := maxminddb.Open(g.CountryDatabasePath)
	if err != nil {
		return fmt.Errorf("failed to open country database %s: %v", g.CountryDatabasePath, err)
	}

	// Open new city database instance
	newCityDB, err := maxminddb.Open(g.CityDatabasePath)
	if err != nil {
		return fmt.Errorf("failed to open city database %s: %v", g.CityDatabasePath, err)
	}

	// Open global city database instance
	newGlobalCityDB, err := maxminddb.Open(g.GlobalCityDatabasePath)
	if err != nil {
		caddy.Log().Named("geoip2").Warn("failed to open global city database, global city data will be empty",
			zap.String("global_city_path", g.GlobalCityDatabasePath),
			zap.Error(err))
		newGlobalCityDB = nil
	}

	// Open ASN database if valid
	var newASNDB *maxminddb.Reader
	if asnDBValid {
		newASNDB, err = maxminddb.Open(g.ASNDatabasePath)
		if err != nil {
			caddy.Log().Named("geoip2").Warn("failed to open ASN database, ASN data will be empty",
				zap.String("asn_path", g.ASNDatabasePath),
				zap.Error(err))
			newASNDB = nil
		}
	}

	// Close old databases if present
	if g.CountryDBHandler != nil {
		if err := g.CountryDBHandler.Close(); err != nil {
			caddy.Log().Named("geoip2").Warn("error closing old country database",
				zap.Error(err))
		}
	}
	if g.CityDBHandler != nil {
		if err := g.CityDBHandler.Close(); err != nil {
			caddy.Log().Named("geoip2").Warn("error closing old city database",
				zap.Error(err))
		}
	}
	if g.GlobalCityDBHandler != nil {
		if err := g.GlobalCityDBHandler.Close(); err != nil {
			caddy.Log().Named("geoip2").Warn("error closing old global city database",
				zap.Error(err))
		}
	}
	if g.ASNDBHandler != nil {
		if err := g.ASNDBHandler.Close(); err != nil {
			caddy.Log().Named("geoip2").Warn("error closing old ASN database",
				zap.Error(err))
		}
	}

	// Replace with new databases
	g.CountryDBHandler = newCountryDB
	g.CityDBHandler = newCityDB
	g.GlobalCityDBHandler = newGlobalCityDB
	g.ASNDBHandler = newASNDB

	// Log successful load with database metadata
	countryMetadata := newCountryDB.Metadata
	caddy.Log().Named("geoip2").Info("country database loaded successfully",
		zap.String("path", g.CountryDatabasePath),
		zap.Uint64("build_epoch", uint64(countryMetadata.BuildEpoch)),
		zap.String("database_type", countryMetadata.DatabaseType))

	cityMetadata := newCityDB.Metadata
	caddy.Log().Named("geoip2").Info("city database loaded successfully",
		zap.String("path", g.CityDatabasePath),
		zap.Uint64("build_epoch", uint64(cityMetadata.BuildEpoch)),
		zap.String("database_type", cityMetadata.DatabaseType))

	if newGlobalCityDB != nil {
		globalCityMetadata := newGlobalCityDB.Metadata
		caddy.Log().Named("geoip2").Info("global city database loaded successfully",
			zap.String("path", g.GlobalCityDatabasePath),
			zap.Uint64("build_epoch", uint64(globalCityMetadata.BuildEpoch)),
			zap.String("database_type", globalCityMetadata.DatabaseType))
	}

	if newASNDB != nil {
		asnMetadata := newASNDB.Metadata
		caddy.Log().Named("geoip2").Info("ASN database loaded successfully",
			zap.String("path", g.ASNDatabasePath),
			zap.Uint64("build_epoch", uint64(asnMetadata.BuildEpoch)),
			zap.String("database_type", asnMetadata.DatabaseType))
	}

	return nil
}

// validateDatabaseFile checks if the database file exists and is accessible
func (g *GeoIP2State) validateDatabaseFile(path string) error {
	// Check if file exists
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("database file not found: %s", path)
		}
		return fmt.Errorf("cannot access database file %s: %v", path, err)
	}

	// Check if it's a regular file (not directory, symlink, etc.)
	if !info.Mode().IsRegular() {
		return fmt.Errorf("database path is not a regular file: %s", path)
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

	// Check if country database is available
	if g.CountryDBHandler == nil {
		return errors.New("country database not loaded")
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
	return g.CountryDBHandler.Lookup(netIP, result)
}

// LookupCity performs a thread-safe City database lookup
// Used for city names, subdivisions, and geographic coordinates
func (g *GeoIP2State) LookupCity(ip interface{}, result interface{}) error {
	// Acquire read lock for database access
	g.mutex.RLock()
	defer g.mutex.RUnlock()

	// Check if city database is available
	if g.CityDBHandler == nil {
		return errors.New("city database not loaded")
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

	// Perform the actual city lookup
	return g.CityDBHandler.Lookup(netIP, result)
}

// LookupGlobalCity performs a thread-safe global City database lookup
// Used for city data for non-European IPs as fallback
func (g *GeoIP2State) LookupGlobalCity(ip interface{}, result interface{}) error {
	// Acquire read lock for database access
	g.mutex.RLock()
	defer g.mutex.RUnlock()

	// Check if global city database is available
	if g.GlobalCityDBHandler == nil {
		return errors.New("global city database not loaded")
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

	// Perform the actual global city lookup
	return g.GlobalCityDBHandler.Lookup(netIP, result)
}

// LookupASN performs a thread-safe ASN database lookup
// Used for ASN number and organization lookups
func (g *GeoIP2State) LookupASN(ip interface{}, result interface{}) error {
	// Acquire read lock for database access
	g.mutex.RLock()
	defer g.mutex.RUnlock()

	// Check if ASN database is available
	if g.ASNDBHandler == nil {
		return errors.New("ASN database not loaded")
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

	// Perform the actual ASN lookup
	return g.ASNDBHandler.Lookup(netIP, result)
}

// GetDatabaseInfo returns information about the currently loaded database
// Useful for monitoring and debugging
func (g *GeoIP2State) GetDatabaseInfo() map[string]interface{} {
	g.mutex.RLock()
	defer g.mutex.RUnlock()

	info := map[string]interface{}{
		"country_database_path":     g.CountryDatabasePath,
		"city_database_path":        g.CityDatabasePath,
		"global_city_database_path": g.GlobalCityDatabasePath,
		"asn_database_path":         g.ASNDatabasePath,
		"reload_interval":           g.ReloadInterval,
		"country_loaded":            g.CountryDBHandler != nil,
		"city_loaded":               g.CityDBHandler != nil,
		"global_city_loaded":        g.GlobalCityDBHandler != nil,
		"asn_loaded":                g.ASNDBHandler != nil,
	}

	if g.CountryDBHandler != nil {
		metadata := g.CountryDBHandler.Metadata
		info["country_build_epoch"] = metadata.BuildEpoch
		info["country_database_type"] = metadata.DatabaseType
		info["country_ip_version"] = metadata.IPVersion
		info["country_record_size"] = metadata.RecordSize
		info["country_node_count"] = metadata.NodeCount
	}

	if g.CityDBHandler != nil {
		metadata := g.CityDBHandler.Metadata
		info["city_build_epoch"] = metadata.BuildEpoch
		info["city_database_type"] = metadata.DatabaseType
		info["city_ip_version"] = metadata.IPVersion
		info["city_record_size"] = metadata.RecordSize
		info["city_node_count"] = metadata.NodeCount
	}

	if g.GlobalCityDBHandler != nil {
		metadata := g.GlobalCityDBHandler.Metadata
		info["global_city_build_epoch"] = metadata.BuildEpoch
		info["global_city_database_type"] = metadata.DatabaseType
		info["global_city_ip_version"] = metadata.IPVersion
		info["global_city_record_size"] = metadata.RecordSize
		info["global_city_node_count"] = metadata.NodeCount
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
	if g.CountryDatabasePath == "" {
		return fmt.Errorf("country_database_path is required")
	}
	if g.CityDatabasePath == "" {
		return fmt.Errorf("city_database_path is required")
	}
	if g.GlobalCityDatabasePath == "" {
		return fmt.Errorf("global_city_database_path is required")
	}

	// Validate reload interval
	if g.ReloadInterval < 0 {
		return fmt.Errorf("reload_interval cannot be negative")
	}

	// Validate database files
	if err := g.validateDatabaseFile(g.CountryDatabasePath); err != nil {
		return fmt.Errorf("country database validation failed: %v", err)
	}
	if err := g.validateDatabaseFile(g.CityDatabasePath); err != nil {
		return fmt.Errorf("city database validation failed: %v", err)
	}
	if err := g.validateDatabaseFile(g.GlobalCityDatabasePath); err != nil {
		return fmt.Errorf("global city database validation failed: %v", err)
	}

	// Test databases can be opened
	countryDB, err := maxminddb.Open(g.CountryDatabasePath)
	if err != nil {
		return fmt.Errorf("cannot open country database %s: %v", g.CountryDatabasePath, err)
	}
	defer countryDB.Close()

	cityDB, err := maxminddb.Open(g.CityDatabasePath)
	if err != nil {
		return fmt.Errorf("cannot open city database %s: %v", g.CityDatabasePath, err)
	}
	defer cityDB.Close()

	globalCityDB, err := maxminddb.Open(g.GlobalCityDatabasePath)
	if err != nil {
		return fmt.Errorf("cannot open global city database %s: %v", g.GlobalCityDatabasePath, err)
	}
	defer globalCityDB.Close()

	// Validate country database type (should be Country database)
	countryMetadata := countryDB.Metadata
	if countryMetadata.DatabaseType != "GeoLite2-Country" &&
		countryMetadata.DatabaseType != "GeoIP2-Country" {
		caddy.Log().Named("geoip2").Warn("unknown country database type",
			zap.String("type", countryMetadata.DatabaseType))
	}

	// Validate city database type (should be City database)
	cityMetadata := cityDB.Metadata
	if cityMetadata.DatabaseType != "GeoLite2-City" &&
		cityMetadata.DatabaseType != "GeoIP2-City" &&
		cityMetadata.DatabaseType != "GeoIP2-City-Europe" {
		caddy.Log().Named("geoip2").Warn("unknown city database type",
			zap.String("type", cityMetadata.DatabaseType))
	}

	globalCityMetadata := globalCityDB.Metadata
	if globalCityMetadata.DatabaseType != "GeoLite2-City" &&
		globalCityMetadata.DatabaseType != "GeoIP2-City" &&
		globalCityMetadata.DatabaseType != "GeoIP2-City-Europe" {
		caddy.Log().Named("geoip2").Warn("unknown global city database type",
			zap.String("type", globalCityMetadata.DatabaseType))
	}

	caddy.Log().Named("geoip2").Info("validation successful",
		zap.String("country_database_type", countryMetadata.DatabaseType),
		zap.Uint64("country_build_epoch", uint64(countryMetadata.BuildEpoch)),
		zap.String("city_database_type", cityMetadata.DatabaseType),
		zap.Uint64("city_build_epoch", uint64(cityMetadata.BuildEpoch)),
		zap.String("global_city_database_type", globalCityMetadata.DatabaseType),
		zap.Uint64("global_city_build_epoch", uint64(globalCityMetadata.BuildEpoch)))

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
