package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/araon/adserver/internal/ads"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config := ads.ConfigFromEnv()
	if err := config.Validate(); err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	redisAddrs := config.RedisAddrs
	if len(redisAddrs) == 0 {
		redisAddrs = []string{config.RedisAddr}
	}
	store := ads.NewRedisStoreWithOptions(ads.RedisStoreOptions{
		Addrs: redisAddrs, Password: config.RedisPassword, DB: config.RedisDB, Prefix: config.RedisPrefix,
		Cluster: config.RedisMode == "cluster",
	})
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := store.Ping(ctx); err != nil {
		logger.Error("redis unavailable", "error", err)
		os.Exit(1)
	}
	cache := ads.NewBidCacheWithPolicy(store, logger, ads.CachePolicy{
		RefreshEvery: config.CacheRefresh, MaxSnapshotAge: config.MaxSnapshotAge, StaleAction: config.StaleSnapshotAction,
	})
	if err := cache.Warm(ctx); err != nil {
		logger.Error("initial bid cache warm failed", "error", err)
		os.Exit(1)
	}
	cache.Start(ctx)

	server := ads.NewServer(config, store, cache, logger)
	defer server.Close()
	if len(config.DSPBidderEndpoints) > 0 {
		bidderConfigs := make([]ads.BidderConfig, 0, len(config.DSPBidderEndpoints))
		for _, endpoint := range config.DSPBidderEndpoints {
			adapter, err := ads.NewHTTPBidderAdapter(endpoint.ID, endpoint.URL, config.DSPAuthorization)
			if err != nil {
				logger.Error("invalid DSP adapter", "error", err)
				os.Exit(2)
			}
			bidderConfigs = append(bidderConfigs, ads.BidderConfig{Adapter: adapter, Timeout: config.DSPTimeout, MaxConcurrent: config.DSPMaxConcurrent, FailureThreshold: config.DSPFailureThreshold, CircuitOpenFor: config.DSPCircuitOpenFor})
		}
		engine, err := ads.NewBidderEngine(ctx, config.BidderEngineConfig, bidderConfigs)
		if err != nil {
			logger.Error("invalid dynamic bidder engine", "error", err)
			os.Exit(2)
		}
		server.SetBidderEngine(engine)
		logger.Info("dynamic bidders enabled", "count", len(bidderConfigs))
	}
	httpServer := &http.Server{
		Addr:              config.HTTPAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      3 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	var debugServer *http.Server
	if config.DebugAddr != "" {
		debugMux := http.NewServeMux()
		debugMux.HandleFunc("GET /debug/pprof/", pprof.Index)
		debugMux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		debugMux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		debugMux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		debugMux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
		debugServer = &http.Server{Addr: config.DebugAddr, Handler: debugMux, ReadHeaderTimeout: time.Second}
		go func() {
			logger.Warn("debug profiler enabled; bind only to localhost", "addr", config.DebugAddr)
			if err := debugServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("debug server failed", "error", err)
			}
		}()
	}
	go func() {
		logger.Info("adserver listening", "addr", config.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			cancel()
		}
	}()
	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if debugServer != nil {
		_ = debugServer.Shutdown(shutdownCtx)
	}
	_ = store.Close()
}
