package geoip2

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// GeoIP2Record defines the minimal structure needed from MaxMind database
// Only includes the fields we actually use to minimize memory allocation
type GeoIP2Record struct {
	Country struct {
		ISOCode           string `maxminddb:"iso_code"`             // Two-letter country code (e.g., "DE", "US")
		IsInEuropeanUnion bool   `maxminddb:"is_in_european_union"` // Whether country is in EU
	} `maxminddb:"country"`

	City struct {
		Names map[string]string `maxminddb:"names"` // City names in different languages
	} `maxminddb:"city"`

	Location struct {
		Latitude  float64 `maxminddb:"latitude"`  // Geographic latitude
		Longitude float64 `maxminddb:"longitude"` // Geographic longitude
	} `maxminddb:"location"`

	Subdivisions []struct {
		IsoCode string `maxminddb:"iso_code"` // State/Province code (e.g., "CA", "BY")
	} `maxminddb:"subdivisions"`

	Traits struct {
		AutonomousSystemNumber       uint64 `maxminddb:"autonomous_system_number"`       // ASN number
		AutonomousSystemOrganization string `maxminddb:"autonomous_system_organization"` // ASN organization name
	} `maxminddb:"traits"`
}

// GeoIP2 is the HTTP middleware handler that provides GeoIP2 functionality
// It enriches requests with geographic information based on client IP
type GeoIP2 struct {
	// Enable controls the IP detection mode:
	// - "strict": only use remote IP address (ignore X-Forwarded-For)
	// - "wild": trust X-Forwarded-For header unconditionally
	// - "trusted_proxies": trust X-Forwarded-For only from trusted proxies (default)
	// - "off"/"false"/"0": disable GeoIP2 lookups
	Enable string `json:"enable,omitempty"`
	
	// state holds reference to the shared GeoIP2 database state
	state *GeoIP2State `json:"-"`
	
	// ctx is the Caddy context for this module instance
	ctx caddy.Context `json:"-"`
}

// IpSafeLevel defines the security level for IP address detection
type IpSafeLevel int

const (
	Wild           IpSafeLevel = 0   // Trust any X-Forwarded-For header
	TrustedProxies IpSafeLevel = 1   // Only trust X-Forwarded-For from trusted proxies
	Strict         IpSafeLevel = 100 // Never trust X-Forwarded-For, use RemoteAddr only
)

// Variable names that will be set in Caddy's replacer
// Using underscore notation instead of dots for better compatibility
const (
	VarCity         = "geoip2_city"
	VarCountryCode  = "geoip2_country_code"
	VarLatitude     = "geoip2_latitude"
	VarLongitude    = "geoip2_longitude"
	VarSubdivisions = "geoip2_subdivisions"
	VarIsInEU       = "geoip2_is_in_eu"
	VarASN          = "geoip2_asn"
	VarASOrg        = "geoip2_asorg"
)

// Module registration - called when Caddy starts
func init() {
	caddy.RegisterModule(GeoIP2{})
	httpcaddyfile.RegisterHandlerDirective("geoip2_vars", parseCaddyfile)
}

// CaddyModule returns module information for Caddy's module system
func (GeoIP2) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.geoip2",
		New: func() caddy.Module { return new(GeoIP2) },
	}
}

// ServeHTTP implements the HTTP middleware interface
// This is called for every HTTP request that passes through this middleware
func (m GeoIP2) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// Get Caddy's replacer to set variables that can be used in config
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	
	// Initialize all GeoIP2 variables with empty defaults
	// This ensures they're always available even if lookup fails
	m.initializeVariables(repl)

	// Only perform lookup if GeoIP2 is enabled
	if m.isEnabled() {
		// Try to perform GeoIP2 lookup and populate variables
		m.performLookup(r, repl)
	}

	// Continue to next handler in chain
	return next.ServeHTTP(w, r)
}

// initializeVariables sets all GeoIP2 variables to empty defaults
// This prevents undefined variable errors in Caddy config
func (m *GeoIP2) initializeVariables(repl *caddy.Replacer) {
	repl.Set(VarCity, "")
	repl.Set(VarCountryCode, "")
	repl.Set(VarLatitude, "")
	repl.Set(VarLongitude, "")
	repl.Set(VarSubdivisions, "")
	repl.Set(VarIsInEU, "")
	repl.Set(VarASN, "")
	repl.Set(VarASOrg, "")
}

// isEnabled checks if GeoIP2 lookups should be performed
func (m *GeoIP2) isEnabled() bool {
	return m.Enable != "off" && m.Enable != "false" && m.Enable != "0"
}

// performLookup does the actual GeoIP2 database lookup and sets variables
func (m *GeoIP2) performLookup(r *http.Request, repl *caddy.Replacer) {
	// Check if database is available
	if m.state == nil || m.state.DBHandler == nil {
		caddy.Log().Named("http.handlers.geoip2").Warn("GeoIP2 database not available")
		return
	}

	// Get client IP address based on configured safety level
	clientIP, err := m.getClientIP(r)
	if err != nil {
		caddy.Log().Named("http.handlers.geoip2").Debug("failed to get client IP", 
			zap.Error(err))
		return
	}

	// Perform database lookup
	var record GeoIP2Record
	if err := m.state.Lookup(clientIP, &record); err != nil {
		caddy.Log().Named("http.handlers.geoip2").Debug("GeoIP2 lookup failed",
			zap.String("ip", clientIP.String()),
			zap.Error(err))
		return
	}

	// Populate replacer variables with lookup results
	m.setGeoIPVariables(repl, &record)

	// Debug logging
	caddy.Log().Named("http.handlers.geoip2").Debug("GeoIP2 lookup successful",
		zap.String("ip", clientIP.String()),
		zap.String("country", record.Country.ISOCode),
		zap.String("city", record.City.Names["en"]))
}

// setGeoIPVariables populates all GeoIP2 variables from the lookup result
func (m *GeoIP2) setGeoIPVariables(repl *caddy.Replacer, record *GeoIP2Record) {
	// Basic country information
	repl.Set(VarCountryCode, record.Country.ISOCode)
	repl.Set(VarIsInEU, record.Country.IsInEuropeanUnion)
	
	// Geographic coordinates
	repl.Set(VarLatitude, record.Location.Latitude)
	repl.Set(VarLongitude, record.Location.Longitude)
	
	// Network information (ASN)
	repl.Set(VarASN, record.Traits.AutonomousSystemNumber)
	repl.Set(VarASOrg, record.Traits.AutonomousSystemOrganization)

	// City name (prefer English, fallback to any available)
	if cityName, exists := record.City.Names["en"]; exists && cityName != "" {
		repl.Set(VarCity, cityName)
	} else {
		// If no English name, try to get any available city name
		for _, name := range record.City.Names {
			if name != "" {
				repl.Set(VarCity, name)
				break
			}
		}
	}

	// Subdivisions (state/province) - use first available
	if len(record.Subdivisions) > 0 && record.Subdivisions[0].IsoCode != "" {
		repl.Set(VarSubdivisions, record.Subdivisions[0].IsoCode)
	}
}

// getClientIP determines the real client IP address based on configuration
// Handles X-Forwarded-For header according to security settings
func (m GeoIP2) getClientIP(r *http.Request) (net.IP, error) {
	var ipStr string

	// Determine if we're behind a trusted proxy
	trustedProxy := caddyhttp.GetVar(r.Context(), caddyhttp.TrustedProxyVarKey).(bool)

	// Convert string setting to safety level
	safeLevel := m.getSafetyLevel()

	// Get X-Forwarded-For header if present
	forwardedFor := r.Header.Get("X-Forwarded-For")

	// Decide which IP to use based on safety level and proxy trust
	if ((safeLevel == TrustedProxies && trustedProxy) || safeLevel == Wild) && forwardedFor != "" {
		// Use X-Forwarded-For header (take first IP in chain)
		ips := strings.Split(forwardedFor, ", ")
		ipStr = strings.TrimSpace(ips[0])
	} else {
		// Use direct connection IP from RemoteAddr
		var err error
		ipStr, _, err = net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			// Handle case where RemoteAddr doesn't have port
			if serr, ok := err.(*net.AddrError); ok && serr.Err == "missing port in address" {
				ipStr = r.RemoteAddr
			} else {
				log.Printf("Error parsing RemoteAddr: %v", err)
				return nil, err
			}
		}
	}

	// Parse and validate IP address
	parsedIP := net.ParseIP(ipStr)
	if parsedIP == nil {
		return nil, fmt.Errorf("invalid IP address: %s", ipStr)
	}

	return parsedIP, nil
}

// getSafetyLevel converts string configuration to IpSafeLevel enum
func (m *GeoIP2) getSafetyLevel() IpSafeLevel {
	switch strings.ToLower(m.Enable) {
	case "strict":
		return Strict
	case "wild":
		return Wild
	default:
		return TrustedProxies
	}
}

// parseCaddyfile parses the Caddyfile directive for this handler
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m GeoIP2
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return m, err
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler
// Parses: geoip2_vars <mode>
func (m *GeoIP2) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		// Parse the mode argument (strict/wild/trusted_proxies)
		if !d.Args(&m.Enable) {
			return d.ArgErr()
		}
	}
	return nil
}

// Provision sets up the module with Caddy context
// Links this handler to the shared GeoIP2 database state
func (g *GeoIP2) Provision(ctx caddy.Context) error {
	caddy.Log().Named("http.handlers.geoip2").Debug("provisioning GeoIP2 handler")
	
	// Get reference to the shared GeoIP2 app/state
	app, err := ctx.App(moduleName)
	if err != nil {
		return fmt.Errorf("getting geoip2 app: %v", err)
	}
	
	// Store reference to shared state
	g.state = app.(*GeoIP2State)
	g.ctx = ctx
	
	return nil
}

// Validate checks if the configuration is valid
func (g GeoIP2) Validate() error {
	caddy.Log().Named("http.handlers.geoip2").Debug("validating GeoIP2 handler")
	
	// Validate Enable setting
	validModes := []string{"strict", "wild", "trusted_proxies", "off", "false", "0", ""}
	mode := strings.ToLower(g.Enable)
	for _, valid := range validModes {
		if mode == valid {
			return nil
		}
	}
	
	return fmt.Errorf("invalid enable mode '%s', must be one of: %v", g.Enable, validModes)
}

// Interface guards - compile-time checks that we implement required interfaces
var (
	_ caddy.Module                = (*GeoIP2)(nil)
	_ caddy.Provisioner           = (*GeoIP2)(nil)
	_ caddy.Validator             = (*GeoIP2)(nil)
	_ caddyhttp.MiddlewareHandler = (*GeoIP2)(nil)
	_ caddyfile.Unmarshaler       = (*GeoIP2)(nil)
)
