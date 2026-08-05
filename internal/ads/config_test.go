package ads

import (
	"strings"
	"testing"
	"time"
)

func TestConfigFromEnvParsesOperationalSettings(t *testing.T) {
	for key, value := range map[string]string{
		"HTTP_ADDR":                     "127.0.0.1:9000",
		"DEBUG_ADDR":                    "127.0.0.1:9001",
		"REDIS_MODE":                    "cluster",
		"REDIS_ADDRS":                   "a:6379, b:6379",
		"REDIS_PREFIX":                  "test",
		"AD_ADMIN_TOKEN":                "secret",
		"CACHE_REFRESH_INTERVAL":        "5s",
		"MAX_SNAPSHOT_AGE":              "20s",
		"STALE_SNAPSHOT_ACTION":         "no_bid",
		"AUCTION_PRICING_MODEL":         "second_price",
		"SECOND_PRICE_INCREMENT_MICROS": "7",
		"DECISION_LOGGING":              "true",
		"DECISION_LOG_BUFFER":           "32",
		"DSP_ENDPOINTS":                 "alpha=https://dsp.example/bid",
		"DSP_TIMEOUT":                   "80ms",
		"DSP_CACHE_TTL":                 "3s",
		"DSP_FAILURE_TTL":               "300ms",
		"DSP_MAX_FANOUT":                "4",
		"DSP_MAX_REFRESHES":             "16",
		"DSP_MAX_KEYS":                  "500",
		"DSP_MAX_CONCURRENT":            "8",
		"DSP_FAILURE_THRESHOLD":         "4",
		"DSP_CIRCUIT_OPEN":              "7s",
	} {
		t.Setenv(key, value)
	}
	config := ConfigFromEnv()
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	if config.HTTPAddr != "127.0.0.1:9000" || config.CacheRefresh != 5*time.Second ||
		config.MaxSnapshotAge != 20*time.Second || config.StaleSnapshotAction != NoBidOnStale ||
		config.AuctionPolicy.PricingModel != SecondPrice || config.AuctionPolicy.SecondPriceIncrementMicros != 7 ||
		!config.DecisionLogging || config.DecisionLogBuffer != 32 || strings.Join(config.RedisAddrs, ",") != "a:6379,b:6379" ||
		len(config.DSPBidderEndpoints) != 1 || config.DSPTimeout != 80*time.Millisecond || config.BidderEngineConfig.CacheTTL != 3*time.Second ||
		config.BidderEngineConfig.MaxConcurrentRefreshes != 16 || config.BidderEngineConfig.MaxKeys != 500 || config.DSPMaxConcurrent != 8 {
		t.Fatalf("unexpected config: %+v", config)
	}
}

func TestConfigValidationRejectsUnsafeCombinations(t *testing.T) {
	valid := ConfigFromEnv()
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"redis mode", func(c *Config) { c.RedisMode = "sentinel" }},
		{"cluster database", func(c *Config) { c.RedisMode, c.RedisDB = "cluster", 1 }},
		{"snapshot age", func(c *Config) { c.MaxSnapshotAge = 0 }},
		{"stale action", func(c *Config) { c.StaleSnapshotAction = "maybe" }},
		{"pricing model", func(c *Config) { c.AuctionPolicy.PricingModel = "third_price" }},
		{"bidder timeout", func(c *Config) { c.DSPTimeout = 11 * time.Second }},
		{"bidder concurrency", func(c *Config) { c.DSPMaxConcurrent = 1025 }},
		{"bidder cache", func(c *Config) { c.BidderEngineConfig.MaxKeys = 1_000_001 }},
		{"bidder ID", func(c *Config) {
			c.DSPBidderEndpoints = []DSPBidderEndpoint{{ID: "bad:id", URL: "https://dsp.example"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestInvalidEnvironmentValuesFallBackToSafeDefaults(t *testing.T) {
	t.Setenv("CACHE_REFRESH_INTERVAL", "-1s")
	t.Setenv("MAX_SNAPSHOT_AGE", "not-a-duration")
	t.Setenv("REDIS_DB", "-4")
	t.Setenv("DECISION_LOG_BUFFER", "0")
	config := ConfigFromEnv()
	if config.CacheRefresh != 30*time.Second || config.MaxSnapshotAge != time.Minute ||
		config.RedisDB != 0 || config.DecisionLogBuffer != 4096 {
		t.Fatalf("unsafe fallback: %+v", config)
	}
}
