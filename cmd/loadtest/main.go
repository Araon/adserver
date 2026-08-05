// Command loadtest sends requests to a warm auction endpoint.
// It reports throughput and client latency.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	endpoint := flag.String("url", "http://127.0.0.1:8080/v1/auction", "auction endpoint")
	concurrency := flag.Int("c", 64, "concurrent clients")
	duration := flag.Duration("duration", 15*time.Second, "test duration")
	body := flag.String("body", `{"placement_id":"homepage_top","country":"IN","device":"mobile"}`, "auction JSON")
	flag.Parse()
	if *concurrency < 1 || *duration <= 0 {
		fmt.Fprintln(os.Stderr, "concurrency and duration must be positive")
		os.Exit(2)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns, transport.MaxIdleConnsPerHost, transport.MaxConnsPerHost = *concurrency*2, *concurrency*2, *concurrency*2
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	deadline := time.Now().Add(*duration)
	latencies := make(chan time.Duration, *concurrency*16)
	samples := make([]time.Duration, 0, *concurrency*1024)
	var collector sync.WaitGroup
	collector.Add(1)
	go func() {
		defer collector.Done()
		for latency := range latencies {
			samples = append(samples, latency)
		}
	}()
	var completed, failures atomic.Uint64
	var workers sync.WaitGroup
	for range *concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for time.Now().Before(deadline) {
				started := time.Now()
				request, err := http.NewRequest(http.MethodPost, *endpoint, bytes.NewBufferString(*body))
				if err != nil {
					failures.Add(1)
					continue
				}
				request.Header.Set("Content-Type", "application/json")
				response, err := client.Do(request)
				elapsed := time.Since(started)
				if err != nil || response.StatusCode != http.StatusOK {
					failures.Add(1)
					if response != nil {
						_ = response.Body.Close()
					}
					continue
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				completed.Add(1)
				latencies <- elapsed
			}
		}()
	}
	workers.Wait()
	close(latencies)
	collector.Wait()
	if len(samples) == 0 {
		fmt.Fprintln(os.Stderr, "no successful requests")
		os.Exit(1)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	seconds := duration.Seconds()
	fmt.Printf("requests=%d failures=%d rps=%.0f p50=%s p95=%s p99=%s max=%s\n", completed.Load(), failures.Load(), float64(completed.Load())/seconds, percentile(samples, .50), percentile(samples, .95), percentile(samples, .99), samples[len(samples)-1])
}

func percentile(samples []time.Duration, percentile float64) time.Duration {
	index := int(float64(len(samples)-1) * percentile)
	return samples[index]
}
