# GeoIP2 Module for Caddy

A high-performance GeoIP2 middleware for Caddy that provides geographic information based on client IP addresses.

## Features

- **Minimal & Fast**: Only extracts the essential GeoIP2 data you actually need
- **Thread-safe**: Concurrent request handling with read/write locks
- **Auto-reload**: Configurable database reloading (daily, weekly, or custom intervals)
- **Smart IP Detection**: Flexible handling of X-Forwarded-For headers with security controls
- **Memory Efficient**: Optimized data structures to minimize allocations

## Build

```bash
xcaddy build --with github.com/ScaleCommerce/caddy-geoip2
```

## Configuration

### Global App Configuration

```caddyfile
{
  # Configure the GeoIP2 database globally
  geoip2 {
    database_path /path/to/GeoLite2-City.mmdb
    asn_database_path /path/to/GeoLite2-ASN.mmdb  # optional, for complete ASN data
    reload_interval daily  # daily, weekly, off, or hours (e.g., 24)
  }

  # Order matters - GeoIP2 must run before directives that use the variables
  order geoip2_vars before header
}
```

### Site-specific Handler

```caddyfile
example.com {
  # Enable GeoIP2 with IP detection mode:
  # - strict: only use RemoteAddr (ignore X-Forwarded-For)
  # - wild: trust any X-Forwarded-For header
  # - trusted_proxies: trust X-Forwarded-For only from trusted proxies (default)
  geoip2_vars strict

  # Use GeoIP2 variables in directives
  header Country-Code "{geoip2_country_code}"
  header Is-EU "{geoip2_is_in_eu}"

  # Conditional responses based on location
  @eu_visitors expression {geoip2_is_in_eu} == true
  respond @eu_visitors "Hello from the EU!"

  # Log geographic data
  log {
    format json
    append {
      country {geoip2_country_code}
      city {geoip2_city}
      coordinates "{geoip2_latitude},{geoip2_longitude}"
      asn {geoip2_asn}
    }
  }

  respond "Hello from {geoip2_city}, {geoip2_country_code}!"
}
```

## Available Variables

The module provides 8 essential GeoIP2 variables:

| Variable | Description | Example |
|----------|-------------|---------|
| `{geoip2_city}` | City name (English) | `"Munich"` |
| `{geoip2_country_code}` | Two-letter country code | `"DE"` |
| `{geoip2_latitude}` | Geographic latitude | `48.1374` |
| `{geoip2_longitude}` | Geographic longitude | `11.5755` |
| `{geoip2_subdivisions}` | State/Province code | `"BY"` |
| `{geoip2_is_in_eu}` | EU membership status | `true` |
| `{geoip2_asn}` | Autonomous System Number | `3320` |
| `{geoip2_asorg}` | AS Organization | `"Deutsche Telekom AG"` |

## Advanced Examples

### Geographic Access Control

```caddyfile
{
  geoip2 {
    database_path /etc/geoip/GeoLite2-City.mmdb
    reload_interval daily
  }
}

api.example.com {
  geoip2_vars trusted_proxies

  # Block requests from certain countries
  @blocked_countries expression {geoip2_country_code} in ["CN", "RU", "KP"]
  respond @blocked_countries "Access denied" 403

  # Rate limit by geographic region
  @high_traffic expression {geoip2_country_code} in ["US", "GB", "DE"]
  rate_limit @high_traffic 100r/m

  @low_traffic expression !({geoip2_country_code} in ["US", "GB", "DE"])
  rate_limit @low_traffic 10r/m

  reverse_proxy localhost:8080
}
```

### Development vs Production Database

```caddyfile
{
  geoip2 {
    database_path {$GEOIP_DATABASE_PATH:/opt/geoip/GeoLite2-City.mmdb}
    reload_interval {$GEOIP_RELOAD_INTERVAL:daily}
  }
}

localhost {
  geoip2_vars wild  # More permissive for development

  respond `
  IP: {remote_host}
  Location: {geoip2_city}, {geoip2_country_code}
  Coordinates: {geoip2_latitude}, {geoip2_longitude}
  EU Member: {geoip2_is_in_eu}
  ASN: {geoip2_asn} ({geoip2_asorg})
  `
}
```

### Comprehensive Logging

```caddyfile
{
  geoip2 {
    database_path /var/lib/geoip/GeoLite2-City.mmdb
    reload_interval weekly
  }
}

example.com {
  geoip2_vars trusted_proxies

  log {
    output file /var/log/caddy/geo.log
    format json
    append {
      # Geographic data
      geo_country {geoip2_country_code}
      geo_city {geoip2_city}
      geo_lat {geoip2_latitude}
      geo_lng {geoip2_longitude}
      geo_subdivision {geoip2_subdivisions}
      geo_eu {geoip2_is_in_eu}

      # Network data
      asn {geoip2_asn}
      as_org {geoip2_asorg}

      # Request data
      user_agent {>User-Agent}
      real_ip {remote_host}
    }
  }

  reverse_proxy backend:8080
}
```

## Understanding Execution Order

### Why Order Matters

**GeoIP2 is a middleware** that sets variables, but **directives** (like `header`, `log`, `respond`) need those variables to be available:

```caddyfile
{
  # GeoIP2 middleware must run BEFORE directives that use the variables
  order geoip2_vars before header
  # Alternative: order geoip2_vars first (runs before everything)
}

example.com {
  # 1. geoip2_vars middleware runs first → sets variables
  geoip2_vars strict

  # 2. Then header directive can use the variables
  header Country-Code "{geoip2_country_code}"  # ✅ Variable available

  # 3. respond directive can also use them
  respond "Hello from {geoip2_city}!"  # ✅ Variable available
}
```

### Common Order Configurations

```caddyfile
# Option 1: Run before specific directives
order geoip2_vars before header

# Option 2: Run first (safest, works with everything)
order geoip2_vars first

# Option 3: Run before multiple directives
order geoip2_vars before header rewrite respond
```

### What Happens Without Correct Order

```caddyfile
# ❌ BAD: No order specified
example.com {
  header Country-Code "{geoip2_country_code}"  # Variable = "" (empty)
  geoip2_vars strict                           # Runs too late!
}

# ✅ GOOD: Correct order
{
  order geoip2_vars before header
}
example.com {
  geoip2_vars strict                           # Sets variables first
  header Country-Code "{geoip2_country_code}"  # Variable has value
}
```

## IP Detection Modes

### `strict` Mode
- **Use case**: High-security environments, direct connections
- **Behavior**: Only uses `RemoteAddr`, ignores all forwarded headers
- **Pros**: Most secure, no spoofing possible
- **Cons**: Won't work behind proxies/load balancers

### `trusted_proxies` Mode (Default)
- **Use case**: Production with known proxy infrastructure
- **Behavior**: Uses `X-Forwarded-For` only from Caddy's trusted proxies
- **Pros**: Secure and works with proper proxy setup
- **Cons**: Requires correct `trusted_proxies` configuration

### `wild` Mode
- **Use case**: Development, testing, or when you can't control proxy headers
- **Behavior**: Trusts any `X-Forwarded-For` header
- **Pros**: Works everywhere, easy setup
- **Cons**: Vulnerable to IP spoofing

## Database Reload Options

| Value | Description |
|-------|-------------|
| `daily` or `24h` | Reload every 24 hours |
| `weekly` or `168h` | Reload every 7 days |
| `48` | Reload every 48 hours |
| `off` or `0` | No automatic reload |

## Performance Optimizations

1. **Minimal Structure**: Only parses fields you actually use
2. **Constant Variables**: Uses constants for variable names (compiler optimization)
3. **Method Decomposition**: Separates concerns for better caching
4. **Smart Fallbacks**: English city names with fallback to any available language
5. **Early Returns**: Fails fast on errors without unnecessary processing
6. **Read Locks**: Multiple concurrent lookups without blocking

## Database Compatibility

Works with MaxMind databases:
- **GeoLite2-City** (free, recommended for geographic data)
- **GeoIP2-City** (commercial)
- **GeoLite2-Country** (limited data)
- **GeoIP2-Country** (limited data)
- **GeoLite2-ASN** (free, recommended for ASN data)

### Dual-Database Setup for Complete Data Coverage

For performance-critical applications that need all 8 variables populated, use the dual-database approach:

```caddyfile
{
  geoip2 {
    database_path /opt/geoip/GeoLite2-City.mmdb      # 61MB - provides geographic data
    asn_database_path /opt/geoip/GeoLite2-ASN.mmdb   # 10MB - provides ASN data
    reload_interval daily
  }
}
```

**Why dual databases?**
- **GeoLite2-City**: Contains city, country, coordinates, subdivisions, EU status - but NO ASN data
- **GeoLite2-ASN**: Contains ASN number and organization - but NO geographic data
- **Combined**: All 8 variables populated with optimal performance (71MB total vs 108MB+ for commercial alternatives)

**Performance benefits:**
- Faster lookups than single large database
- Only 71MB total memory usage
- Automatic fallback if ASN database unavailable
- Free alternative to expensive commercial databases

## Error Handling

The module gracefully handles:
- Missing database files
- Corrupted databases
- Invalid IP addresses
- Database reload failures
- Network interruptions

Variables are always available (empty strings if lookup fails) to prevent template errors.

## Monitoring

You can monitor the GeoIP2 module via Caddy's admin API:

```bash
# Check current status
curl localhost:2019/config/apps/geoip2

# Reload database manually
curl -X POST localhost:2019/load \
  -H "Content-Type: application/json" \
  -d '{"module": "geoip2"}'
```

## License

This project is licensed under the Apache License 2.0.

## Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## References

- [Caddy Documentation](https://caddyserver.com/docs/)
- [MaxMind GeoIP2 Documentation](https://dev.maxmind.com/geoip/docs/)
- [MaxMind Database Format](https://maxmind.github.io/MaxMind-DB/)
