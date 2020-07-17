/*
 * Copyright 2019 Martin Helmich <martin@helmich.me>
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/satyrius/gonx"
	"github.com/tokopedia/prometheus-nginxlog-exporter/config"
	"github.com/tokopedia/prometheus-nginxlog-exporter/discovery"
	"github.com/tokopedia/prometheus-nginxlog-exporter/prof"
	"github.com/tokopedia/prometheus-nginxlog-exporter/relabeling"
	"github.com/tokopedia/prometheus-nginxlog-exporter/syslog"
	"github.com/tokopedia/prometheus-nginxlog-exporter/tail"
)

type NSMetrics struct {
	cfg      *config.NamespaceConfig
	registry *prometheus.Registry
	Metrics
}

func NewNSMetrics(cfg *config.NamespaceConfig, ddog *statsd.Client) *NSMetrics {
	m := &NSMetrics{
		cfg:      cfg,
		registry: prometheus.NewRegistry(),
	}
	m.Init(cfg)

	m.registry.MustRegister(m.countTotal)
	m.registry.MustRegister(m.bytesTotal)
	m.registry.MustRegister(m.upstreamSeconds)
	m.registry.MustRegister(m.upstreamSecondsHist)
	m.registry.MustRegister(m.responseSeconds)
	m.registry.MustRegister(m.responseSecondsHist)
	m.registry.MustRegister(m.parseErrorsTotal)
	m.datadogClient = ddog
	return m
}

// Metrics is a struct containing pointers to all metrics that should be
// exposed to Prometheus
type Metrics struct {
	countTotal          *prometheus.CounterVec
	bytesTotal          *prometheus.CounterVec
	upstreamSeconds     *prometheus.SummaryVec
	upstreamSecondsHist *prometheus.HistogramVec
	responseSeconds     *prometheus.SummaryVec
	responseSecondsHist *prometheus.HistogramVec
	parseErrorsTotal    prometheus.Counter
	datadogClient       *statsd.Client
}

func inLabels(label string, labels []string) bool {
	for _, l := range labels {
		if label == l {
			return true
		}
	}
	return false
}

// Init initializes a metrics struct
func (m *Metrics) Init(cfg *config.NamespaceConfig) {
	cfg.MustCompile()

	labels := cfg.OrderedLabelNames

	for i := range cfg.RelabelConfigs {
		labels = append(labels, cfg.RelabelConfigs[i].TargetLabel)
	}

	for _, r := range relabeling.DefaultRelabelings {
		if !inLabels(r.TargetLabel, labels) {
			labels = append(labels, r.TargetLabel)
		}
	}

	m.countTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   cfg.NamespacePrefix,
		ConstLabels: cfg.NamespaceLabels,
		Name:        "http_response_count_total",
		Help:        "Amount of processed HTTP requests",
	}, labels)

	m.bytesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   cfg.NamespacePrefix,
		ConstLabels: cfg.NamespaceLabels,
		Name:        "http_response_size_bytes",
		Help:        "Total amount of transferred bytes",
	}, labels)

	m.upstreamSeconds = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Namespace:   cfg.NamespacePrefix,
		ConstLabels: cfg.NamespaceLabels,
		Name:        "http_upstream_time_seconds",
		Help:        "Time needed by upstream servers to handle requests",
		Objectives:  map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
	}, labels)

	m.upstreamSecondsHist = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   cfg.NamespacePrefix,
		ConstLabels: cfg.NamespaceLabels,
		Name:        "http_upstream_time_seconds_hist",
		Help:        "Time needed by upstream servers to handle requests",
		Buckets:     cfg.HistogramBuckets,
	}, labels)

	m.responseSeconds = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Namespace:   cfg.NamespacePrefix,
		ConstLabels: cfg.NamespaceLabels,
		Name:        "http_response_time_seconds",
		Help:        "Time needed by NGINX to handle requests",
		Objectives:  map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
	}, labels)

	m.responseSecondsHist = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   cfg.NamespacePrefix,
		ConstLabels: cfg.NamespaceLabels,
		Name:        "http_response_time_seconds_hist",
		Help:        "Time needed by NGINX to handle requests",
		Buckets:     cfg.HistogramBuckets,
	}, labels)

	m.parseErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   cfg.NamespacePrefix,
		ConstLabels: cfg.NamespaceLabels,
		Name:        "parse_errors_total",
		Help:        "Total number of log file lines that could not be parsed",
	})
}

//For Datadog START
var datadogTags map[string]bool

func (m *Metrics) IncrDD(name string, tags []string) {
	if m.datadogClient == nil {
		return
	}
	m.datadogClient.Incr(name, tags, 1)
}
func (m *Metrics) CountDD(name string, value int64, tags []string) {
	if m.datadogClient == nil {
		return
	}
	m.datadogClient.Count(name, value, tags, 1)
}
func (m *Metrics) HistogramDD(name string, value float64, tags []string) {
	if m.datadogClient == nil {
		return
	}
	m.datadogClient.Histogram(name, value, tags, 1)
}
func (m *Metrics) GaugeDD(name string, value float64, tags []string) {
	if m.datadogClient == nil {
		return
	}
	m.datadogClient.Gauge(name, value, tags, 1)
}

//For Datadog END

func main() {
	var opts config.StartupFlags
	var cfg = config.Config{
		Listen: config.ListenConfig{
			Port:            4040,
			Address:         "0.0.0.0",
			MetricsEndpoint: "/metrics",
		},
	}
	nsGatherers := make(prometheus.Gatherers, 0)

	flag.IntVar(&opts.ListenPort, "listen-port", 4040, "HTTP port to listen on")
	flag.StringVar(&opts.Format, "format", `$remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent" "$http_x_forwarded_for"`, "NGINX access log format")
	flag.StringVar(&opts.Namespace, "namespace", "nginx", "namespace to use for metric names")
	flag.StringVar(&opts.ConfigFile, "config-file", "", "Configuration file to read from")
	flag.BoolVar(&opts.EnableExperimentalFeatures, "enable-experimental", false, "Set this flag to enable experimental features")
	flag.StringVar(&opts.CPUProfile, "cpuprofile", "", "write cpu profile to `file`")
	flag.StringVar(&opts.MemProfile, "memprofile", "", "write memory profile to `file`")
	flag.StringVar(&opts.DatadogUrl, "datadog-url", "datadog.tokopedia.local:8125", "Datadog URL")
	flag.StringVar(&opts.MetricsEndpoint, "metrics-endpoint", cfg.Listen.MetricsEndpoint, "URL path at which to serve metrics")
	flag.Parse()

	opts.Filenames = flag.Args()

	sigChan := make(chan os.Signal, 1)
	stopChan := make(chan bool)
	stopHandlers := sync.WaitGroup{}

	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGINT)

	go func() {
		sig := <-sigChan

		fmt.Printf("caught term %s. exiting\n", sig)

		close(stopChan)
		stopHandlers.Wait()

		os.Exit(0)
	}()

	defer func() {
		close(stopChan)
		stopHandlers.Wait()
	}()

	dd, err := statsd.New(opts.DatadogUrl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to datadog.")
		os.Exit(1)
	}
	datadogTags = make(map[string]bool)

	prof.SetupCPUProfiling(opts.CPUProfile, stopChan, &stopHandlers)
	prof.SetupMemoryProfiling(opts.MemProfile, stopChan, &stopHandlers)

	loadConfig(&opts, &cfg)

	fmt.Printf("using configuration %+v\n", cfg)

	if stabilityError := cfg.StabilityWarnings(); stabilityError != nil && !opts.EnableExperimentalFeatures {
		fmt.Fprintf(os.Stderr, "Your configuration file contains an option that is explicitly labeled as experimental feature:\n\n  %s\n\n", stabilityError.Error())
		fmt.Fprintln(os.Stderr, "Use the -enable-experimental flag or the enable_experimental option to enable these features. Use them at your own peril.")

		os.Exit(1)
	}

	if cfg.Consul.Enable {
		setupConsul(&cfg, stopChan, &stopHandlers)
	}

	for _, ns := range cfg.Namespaces {
		nsMetrics := NewNSMetrics(&ns, dd)
		nsGatherers = append(nsGatherers, nsMetrics.registry)

		fmt.Printf("starting listener for namespace %s\n", ns.Name)
		go processNamespace(ns, &(nsMetrics.Metrics))
	}

	listenAddr := fmt.Sprintf("%s:%d", cfg.Listen.Address, cfg.Listen.Port)
	endpoint := cfg.Listen.MetricsEndpointOrDefault()

	fmt.Printf("running HTTP server on address %s, serving metrics at %s\n", listenAddr, endpoint)

	nsHandler := promhttp.InstrumentMetricHandler(
		prometheus.DefaultRegisterer, promhttp.HandlerFor(nsGatherers, promhttp.HandlerOpts{}),
	)

	http.Handle(endpoint, nsHandler)

	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		fmt.Printf("error while starting HTTP server: %s", err.Error())
	}
}

func loadConfig(opts *config.StartupFlags, cfg *config.Config) {
	if opts.ConfigFile != "" {
		fmt.Printf("loading configuration file %s\n", opts.ConfigFile)
		if err := config.LoadConfigFromFile(cfg, opts.ConfigFile); err != nil {
			panic(err)
		}
	} else if err := config.LoadConfigFromFlags(cfg, opts); err != nil {
		panic(err)
	}
}

func setupConsul(cfg *config.Config, stopChan <-chan bool, stopHandlers *sync.WaitGroup) {
	registrator, err := discovery.NewConsulRegistrator(cfg)
	if err != nil {
		panic(err)
	}

	fmt.Printf("registering service in Consul\n")
	if err := registrator.RegisterConsul(); err != nil {
		panic(err)
	}

	go func() {
		<-stopChan
		fmt.Printf("unregistering service in Consul\n")

		if err := registrator.UnregisterConsul(); err != nil {
			fmt.Printf("error while unregistering from consul: %s\n", err.Error())
		}

		stopHandlers.Done()
	}()

	stopHandlers.Add(1)
}

func processNamespace(nsCfg config.NamespaceConfig, metrics *Metrics) {
	var followers []tail.Follower

	parser := gonx.NewParser(nsCfg.Format)

	for _, f := range nsCfg.SourceData.Files {
		t, err := tail.NewFileFollower(f)
		if err != nil {
			panic(err)
		}

		t.OnError(func(err error) {
			panic(err)
		})

		followers = append(followers, t)
	}

	if nsCfg.SourceData.Syslog != nil {
		slCfg := nsCfg.SourceData.Syslog

		fmt.Printf("running Syslog server on address %s\n", slCfg.ListenAddress)
		channel, server, err := syslog.Listen(slCfg.ListenAddress, slCfg.Format)
		if err != nil {
			panic(err)
		}

		for _, f := range slCfg.Tags {
			t, err := tail.NewSyslogFollower(f, server, channel)
			if err != nil {
				panic(err)
			}

			t.OnError(func(err error) {
				panic(err)
			})

			followers = append(followers, t)
		}
	}

	for _, f := range followers {
		go processSource(nsCfg, f, parser, metrics)
	}

}

func getServerIP() (string, error) {
	output, err := exec.Command("ip", "r").Output()
	if err != nil {
		return "0.0.0.0", nil
	}

	result := ""
	arr := strings.Split(string(output), "\n")
	for _, v := range arr {
		if strings.Contains(v, "proto kernel") && strings.Contains(v, "scope link") {
			splited := strings.Split(v, " ")
			result = splited[len(splited)-1]
		}
	}

	return result, nil
}

func processSource(nsCfg config.NamespaceConfig, t tail.Follower, parser *gonx.Parser, metrics *Metrics) {
	relabelings := relabeling.NewRelabelings(nsCfg.RelabelConfigs)
	relabelings = append(relabelings, relabeling.DefaultRelabelings...)
	relabelings = relabeling.UniqueRelabelings(relabelings)

	staticLabelValues := nsCfg.OrderedLabelValues
	staticLabels := nsCfg.Labels //For Datadog
	staticName := nsCfg.Name     //For Datadog

	totalLabelCount := len(staticLabelValues) + len(relabelings)
	relabelLabelOffset := len(staticLabelValues)
	labelValues := make([]string, totalLabelCount)
	datadogLabels := []string{} //For Datadog

	for i := range staticLabelValues {
		labelValues[i] = staticLabelValues[i]
	}
	//For Datadog START
	for k, v := range staticLabels {
		datadogLabels = append(datadogLabels, fmt.Sprintf("%s:%s", k, v))
	}

	hostname, _ := os.Hostname()
	serverIP, _ := getServerIP()
	datadogLabels = append(datadogLabels, fmt.Sprintf("%s_hostname:%s", staticName, hostname))
	datadogLabels = append(datadogLabels, fmt.Sprintf("%s_ip:%s", staticName, serverIP))
	//For Datadog END

	for line := range t.Lines() {
		if nsCfg.PrintLog {
			fmt.Println(line)
		}

		entry, err := parser.ParseString(line)
		if err != nil {
			fmt.Printf("error while parsing line '%s': %s\n", line, err)
			metrics.parseErrorsTotal.Inc()
			continue
		}

		fields := entry.Fields()
		tags := []string{}
		copy(tags, datadogLabels)

		for i := range relabelings {
			if str, ok := fields[relabelings[i].SourceValue]; ok {
				mapped, err := relabelings[i].Map(str)
				if err == nil {
					labelValues[i+relabelLabelOffset] = mapped
					tags = append(tags, fmt.Sprintf("%s:%s", relabelings[i].TargetLabel, mapped))

					if relabelings[i].TargetLabel == "status" {
						tags = append(tags, fmt.Sprintf("status_group:%sxx", mapped[0:1]))
					}
				}
			}
		}

		metrics.countTotal.WithLabelValues(labelValues...).Inc()
		metrics.IncrDD(staticName+".nginx.response.count_total", tags) //For Datadog

		// check datadog tags length
		for _, t := range tags {
			datadogTags[t] = true
		}
		if len(datadogTags) >= 400 {
			log.Printf("too many datadog tags beign created, please check, datadogTags: %v", datadogTags)
			os.Exit(0)
		}

		if bytes, ok := floatFromFields(fields, "body_bytes_sent"); ok {
			metrics.bytesTotal.WithLabelValues(labelValues...).Add(bytes)
			metrics.CountDD(staticName+".nginx.response.size_bytes", int64(bytes), tags) //For Datadog
		}

		if upstreamTime, ok := floatFromFields(fields, "upstream_response_time"); ok {
			metrics.upstreamSeconds.WithLabelValues(labelValues...).Observe(upstreamTime)
			metrics.upstreamSecondsHist.WithLabelValues(labelValues...).Observe(upstreamTime)
			metrics.HistogramDD(staticName+".nginx.upstream.time_seconds", upstreamTime, tags) //For Datadog
		}

		if responseTime, ok := floatFromFields(fields, "request_time"); ok {
			metrics.responseSeconds.WithLabelValues(labelValues...).Observe(responseTime)
			metrics.responseSecondsHist.WithLabelValues(labelValues...).Observe(responseTime)
			metrics.HistogramDD(staticName+".nginx.response.time_seconds", responseTime, tags) //For Datadog
		}
	}
}

func floatFromFields(fields gonx.Fields, name string) (float64, bool) {
	val, ok := fields[name]
	if !ok {
		return 0, false
	}

	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0, false
	}

	return f, true
}
