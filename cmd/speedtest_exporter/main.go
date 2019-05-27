package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	_ "net/http/pprof"
	"os"
	"sort"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	promlogflag "github.com/prometheus/common/promlog/flag"
	"github.com/zpeters/speedtest"
)

const (
	version   = "0.2.0"
	namespace = "speedtest"
)

var (
	// A believable Firefox User-Agent string
	// see also: https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/User-Agent/Firefox
	userAgent = fmt.Sprintf("Mozilla/5.0 (Windows NT %.1f; Win64; x64; rv:%.1f) Gecko/20100101 Firefox/%.1[2]f",
		rand.Float32()*1e3+1, rand.Float32()*1e3+1)
)

var (
	showVersion   = flag.Bool("version", false, "Print version information.")
	listenAddress = flag.String("web.listen-address", ":9112", "Address to listen on for web interface and telemetry.")
	timeoutOffset = flag.Float64("timeout-offset", 0.5, "Offset to subtract from timeout in seconds.")
	configURL     = flag.String("speedtest.config-url", speedtest.SpeedtestNetConfigURL, "speedtest.net configuration URL")
	serversURL    = flag.String("speedtest.servers-url", speedtest.SpeedtestNetServersURL, "speedtest.net server list URL")
)

type client struct {
	w    http.ResponseWriter
	r    *http.Request
	done chan<- struct{}
}

type testRunner struct {
	logger                  log.Logger
	newClient, deleteClient chan client
	refreshServers          chan struct{}
}

func (t *testRunner) updateServers() (closestServers []speedtest.Server) {
	const numFastestServers = 5
	stClient := speedtest.NewClient(http.DefaultClient)
	stClient.UserAgent = userAgent

	level.Info(t.logger).Log("msg", "Getting new server list from speedtest.net")

	config, err := stClient.GetSpeedtestConfig(*configURL)
	if err != nil {
		level.Error(t.logger).Log("msg", "Error getting speedtest.net config", "err", err)
		return
	}

	allServers, err := stClient.GetServers(*serversURL)
	if err != nil {
		level.Error(t.logger).Log("msg", "Error getting speedtest.net servers", "err", err)
		return
	}

	speedtest.ComputeServersDistanceTo(allServers, config.Client.Latitude, config.Client.Longitude)
	sort.Sort(speedtest.ByDistance(allServers))

	closestServers = make([]speedtest.Server, numFastestServers)
	copy(closestServers, allServers[:numFastestServers])
	return
}

func (t *testRunner) probe(ctx context.Context, registry *prometheus.Registry, servers []speedtest.Server) (success bool) {
	start := time.Now()

	probeSuccessGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_success",
		Help: "Displays whether or not the probe was a success",
	})
	registry.MustRegister(probeSuccessGauge)

	probeDurationGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_duration_seconds",
		Help: "Returns how long the probe took to complete in seconds",
	})
	registry.MustRegister(probeDurationGauge)

	defer func() {
		duration := time.Since(start).Seconds()
		probeDurationGauge.Set(duration)
		if success {
			probeSuccessGauge.Set(1)
			level.Info(t.logger).Log("msg", "Probe succeeded", "duration_seconds", duration)
		} else {
			level.Error(t.logger).Log("msg", "Probe failed", "duration_seconds", duration)
		}
	}()

	latency := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "latency_seconds",
		Help:      "Latency (seconds)",
	})
	registry.MustRegister(latency)

	upload := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "upload",
		Help:      "Upload bandwidth (bytes/s)",
	})
	registry.MustRegister(upload)

	download := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "download",
		Help:      "Download bandwidth (bytes/s)",
	})
	registry.MustRegister(download)

	stClient := speedtest.NewClient(http.DefaultClient)
	stClient.UserAgent = userAgent

	config, err := stClient.GetSpeedtestConfig(*configURL)
	if err != nil {
		level.Error(t.logger).Log("msg", "Error getting speedtest.net config", "err", err)
		return false
	}
	tester := speedtest.NewTester(config)
	tester.UserAgent = userAgent

	var testServer speedtest.Server
	{
		timeout := 20 * time.Second
		if config.Latency.TestLengthSeconds != 0 {
			timeout = time.Duration(config.Latency.TestLengthSeconds) * time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		const numFastestServers = 5
		for i, srv := range servers[:numFastestServers] {
			ch, errch := tester.Latency(ctx, srv, 3)

			for m := range ch {
				if srv.Latency < m.Seconds || srv.Latency == 0 {
					srv.Latency = m.Seconds
				}
			}

			if err := <-errch; err != nil {
				level.Error(t.logger).Log("msg", "Error testing latency for server", "server", fmt.Sprintf("%s - %s (%s)", srv.ID, srv.Name, srv.Sponsor), "err", err)
				continue
			}
			servers[i] = srv
		}

		sort.Sort(speedtest.ByLatency(servers[:numFastestServers]))
		testServer = servers[0]
	}

	var zeroServer speedtest.Server
	if testServer == zeroServer {
		// either all the servers errored out or we weren't given any to start with
		return false
	}

	level.Info(t.logger).Log("msg", "Testing against server", "name", testServer.Name, "sponsor", testServer.Sponsor, "latency_seconds", testServer.Latency)
	latency.Set(testServer.Latency)

	defaultDLSizes := []uint{350, 500, 750, 1000, 1500, 2000, 2500, 3000, 3500, 4000}
	defaultULSizes := []uint{0.25 * 1e6, 0.5 * 1e6, 1.0 * 1e6, 1.5 * 1e6, 2.0 * 1e6}

	var downspeed float64
	{
		timeout := 50 * time.Second
		if config.Download.TestLengthSeconds != 0 {
			timeout = time.Duration(config.Download.TestLengthSeconds) * time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		ch, errch := tester.Download(ctx, testServer, defaultDLSizes)

		var ticks float64
		for m := range ch {
			ticks++
			downspeed += m.Bytes / m.Seconds
		}
		downspeed /= ticks

		if err := <-errch; err != nil && err != context.DeadlineExceeded {
			level.Error(t.logger).Log("msg", "Error testing download speed", "err", err)
			return false
		}
	}

	var upspeed float64
	{
		timeout := 50 * time.Second
		if config.Upload.TestLengthSeconds != 0 {
			timeout = time.Duration(config.Upload.TestLengthSeconds) * time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		ch, errch := tester.Upload(ctx, testServer, defaultULSizes)

		var ticks float64
		for m := range ch {
			ticks++
			upspeed += m.Bytes / m.Seconds
		}
		upspeed /= ticks

		if err := <-errch; err != nil && err != context.DeadlineExceeded {
			level.Error(t.logger).Log("msg", "Error testing upload speed", "err", err)
			return false
		}
	}

	download.Set(downspeed)
	upload.Set(upspeed)
	return true
}

func (t *testRunner) start() {
	testFinished := make(chan bool)
	refreshServersTimer := time.NewTimer(24 * time.Hour)

	closestServers := t.updateServers()

	go func() {
		clients := make(map[client]bool)
		var isRunning bool
		var cancel context.CancelFunc
		var registry *prometheus.Registry

		for {
			select {
			case <-t.refreshServers:
				closestServers = t.updateServers()
			case <-refreshServersTimer.C:
				closestServers = t.updateServers()

			case <-testFinished:
				isRunning = false

				h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
				for c := range clients {
					h.ServeHTTP(c.w, c.r)
					close(c.done)
					delete(clients, c)
				}

			case c := <-t.newClient:
				clients[c] = true
				level.Debug(t.logger).Log("msg", "New client", "client", fmt.Sprintf("%#v", c.done))

				if !isRunning {
					var ctx context.Context
					registry = prometheus.NewRegistry()
					ctx, cancel = context.WithCancel(context.Background())

					isRunning = true
					go func() {
						// TODO: also keep an in-memory copy of the log and an endpoint to access it
						testFinished <- t.probe(ctx, registry, closestServers)
					}()
				}

			case c := <-t.deleteClient:
				level.Debug(t.logger).Log("msg", "Deleting client", "client", fmt.Sprintf("%#v", c.done))

				// is the client still valid?
				if _, ok := clients[c]; ok {
					close(c.done)
					delete(clients, c)
				}

				// if no more clients are around cancel the test
				if len(clients) == 0 {
					cancel()
				}
			}
		}
	}()
}

func (t *testRunner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// TODO: parse the X-Prometheus-Scrape-Timeout-Seconds header and have the tests be that length

	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-r.Context().Done():
			t.deleteClient <- client{r: r, w: w, done: done}
		}
	}()

	t.newClient <- client{w: w, r: r, done: done}
	<-done
}

func main() {
	loggerConfig := promlog.Config{
		Level:  new(promlog.AllowedLevel),
		Format: new(promlog.AllowedFormat),
	}
	loggerConfig.Level.Set("info")
	loggerConfig.Format.Set("logfmt")

	flag.Var(loggerConfig.Level, promlogflag.LevelFlagName, promlogflag.LevelFlagHelp)
	flag.Var(loggerConfig.Format, promlogflag.FormatFlagName, promlogflag.FormatFlagHelp)

	flag.Parse()

	logger := promlog.New(&loggerConfig)

	runner := &testRunner{
		logger:         logger,
		newClient:      make(chan client),
		deleteClient:   make(chan client),
		refreshServers: make(chan struct{}),
	}
	runner.start()

	if *showVersion {
		fmt.Printf("Speedtest Prometheus exporter. v%s\n", version)
		os.Exit(0)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html>
             <head><title>Speedtest Exporter</title></head>
             <body>
             <h1>Speedtest Exporter</h1>
             <p><a href="/speedtest">Speedtest</a></p>
             <p><a href="/metrics">Metrics</a></p>
             </body>
             </html>`)
	})
	// http.Handle("/metrics", promhttp.Handler())

	http.HandleFunc("/-/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "This endpoint requires a POST request", http.StatusMethodNotAllowed)
			return
		}

		runner.refreshServers <- struct{}{}
	})

	// http.Handle("/speedtest", runner)
	http.Handle("/metrics", runner)

	level.Info(logger).Log("msg", "Listening on address", "address", *listenAddress)
	if err := http.ListenAndServe(*listenAddress, nil); err != http.ErrServerClosed {
		level.Error(logger).Log("msg", "Error starting HTTP server", "err", err)
		os.Exit(1)
	}
}
