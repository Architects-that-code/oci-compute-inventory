// OCI Compute inventory exporter. Only complete tenancy snapshots publish counts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type region struct{ Name, Status string }
type compartment struct{ ID, Name, Parent, State string }
type instance struct{ ID, Compartment, State string }
type backend interface {
	Regions(context.Context) ([]region, error)
	Compartments(context.Context, string) ([]compartment, string, error)
	Instances(context.Context, string, string, string) ([]instance, string, error)
}
type location struct{ Region, Compartment string }
type compartmentInfo struct{ Name, Path string }
type snapshot struct {
	Counts                   map[location]int
	Regions                  []string
	Compartments             map[string]compartmentInfo
	RegionSuccess            map[string]int
	Success                  bool
	LastAttempt, LastSuccess time.Time
	Duration                 float64
	Errors                   int
}

func pageLoop(visit func(string) (string, error)) error {
	seen := map[string]bool{}
	page := ""
	for {
		next, err := visit(page)
		if err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		if seen[next] {
			return errors.New("OCI pagination repeated a continuation token")
		}
		seen[next] = true
		page = next
	}
}
func collect(ctx context.Context, api backend, tenancy string, workers int) (s snapshot) {
	started := time.Now()
	s.LastAttempt = started
	defer func() { s.Duration = time.Since(started).Seconds() }()
	fail := func(err error) snapshot { log.Printf("Inventory discovery failed: %v", err); s.Errors++; return s }
	rs, err := api.Regions(ctx)
	if err != nil {
		return fail(err)
	}
	unique := map[string]bool{}
	for _, r := range rs {
		if r.Status == "READY" && r.Name != "" {
			unique[r.Name] = true
		}
	}
	for name := range unique {
		s.Regions = append(s.Regions, name)
	}
	sort.Strings(s.Regions)
	if len(s.Regions) == 0 {
		return fail(errors.New("no READY region subscriptions"))
	}
	compartments := map[string]compartment{}
	err = pageLoop(func(page string) (string, error) {
		items, next, e := api.Compartments(ctx, page)
		if e != nil {
			return "", e
		}
		for _, c := range items {
			if c.State == "ACTIVE" {
				compartments[c.ID] = c
			}
		}
		return next, nil
	})
	if err != nil {
		return fail(err)
	}
	s.Compartments = map[string]compartmentInfo{tenancy: {Name: "Tenancy root", Path: "/"}}
	var path func(string, map[string]bool) (string, error)
	path = func(id string, seen map[string]bool) (string, error) {
		if id == tenancy {
			return "/", nil
		}
		c, ok := compartments[id]
		if !ok || seen[id] {
			return "", errors.New("compartment ancestry is incomplete or cyclic")
		}
		seen[id] = true
		parent, e := path(c.Parent, seen)
		if e != nil {
			return "", e
		}
		return strings.TrimRight(parent, "/") + "/" + c.Name, nil
	}
	for id, c := range compartments {
		p, e := path(id, map[string]bool{})
		if e != nil {
			return fail(e)
		}
		s.Compartments[id] = compartmentInfo{Name: c.Name, Path: p}
	}
	type job struct{ region, compartment string }
	type result struct {
		job
		instances map[string]string
		err       error
	}
	jobs := make(chan job)
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				found := map[string]string{}
				e := pageLoop(func(page string) (string, error) {
					items, next, err := api.Instances(ctx, j.region, j.compartment, page)
					if err != nil {
						return "", err
					}
					for _, in := range items {
						if in.State == "RUNNING" {
							c := in.Compartment
							if c == "" {
								c = j.compartment
							}
							found[in.ID] = c
						}
					}
					return next, nil
				})
				results <- result{job: j, instances: found, err: e}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, r := range s.Regions {
			for c := range s.Compartments {
				select {
				case jobs <- job{r, c}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	inventory := map[string]location{}
	completed := map[string]int{}
	failed := map[string]bool{}
	for r := range results {
		completed[r.region]++
		if r.err != nil {
			s.Errors++
			failed[r.region] = true
			log.Printf("Inventory scan failed: region=%s compartment=%s error=%v", r.region, r.compartment, r.err)
			continue
		}
		for id, c := range r.instances {
			if _, ok := s.Compartments[c]; !ok {
				s.Errors++
				failed[r.region] = true
				continue
			}
			inventory[id] = location{r.region, c}
		}
	}
	if ctx.Err() != nil {
		s.Errors++
	}
	s.RegionSuccess = map[string]int{}
	for _, r := range s.Regions {
		if !failed[r] && completed[r] == len(s.Compartments) {
			s.RegionSuccess[r] = 1
		} else {
			s.RegionSuccess[r] = 0
		}
	}
	s.Success = s.Errors == 0
	if s.Success {
		s.Counts = map[location]int{}
		for _, r := range s.Regions {
			for c := range s.Compartments {
				s.Counts[location{r, c}] = 0
			}
		}
		for _, loc := range inventory {
			s.Counts[loc]++
		}
		s.LastSuccess = time.Now()
	}
	return s
}
func escape(v string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"").Replace(v)
}
func render(s snapshot, interval time.Duration) string {
	var b strings.Builder
	gauge := func(name, help string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name) }
	scalar := func(name, help string, v float64) {
		gauge(name, help)
		fmt.Fprintf(&b, "%s %s\n", name, strconv.FormatFloat(v, 'f', -1, 64))
	}
	timestamp := func(t time.Time) float64 {
		if t.IsZero() {
			return 0
		}
		return float64(t.UnixNano()) / 1e9
	}
	success := 0.
	if s.Success {
		success = 1
	}
	scalar("oci_compute_inventory_collection_success", "One only when the latest complete tenancy scan succeeded.", success)
	scalar("oci_compute_inventory_last_attempt_timestamp_seconds", "Last scan attempt start time.", timestamp(s.LastAttempt))
	scalar("oci_compute_inventory_last_success_timestamp_seconds", "Last fully successful tenancy scan completion time.", timestamp(s.LastSuccess))
	scalar("oci_compute_inventory_collection_duration_seconds", "Most recent scan duration.", s.Duration)
	scalar("oci_compute_inventory_collection_errors", "Failed region-compartment scans in the latest attempt.", float64(s.Errors))
	scalar("oci_compute_inventory_poll_interval_seconds", "Configured refresh interval after each scan.", interval.Seconds())
	scalar("oci_compute_inventory_regions_expected", "Number of READY subscribed regions discovered.", float64(len(s.Regions)))
	scalar("oci_compute_inventory_compartments_expected", "Number of active compartments including the tenancy root.", float64(len(s.Compartments)))
	gauge("oci_compute_inventory_region_success", "One when all compartment scans succeeded in a subscribed region.")
	for _, r := range s.Regions {
		fmt.Fprintf(&b, "oci_compute_inventory_region_success{region=\"%s\"} %d\n", escape(r), s.RegionSuccess[r])
	}
	if s.Success {
		ids := make([]string, 0, len(s.Compartments))
		for id := range s.Compartments {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		gauge("oci_compute_compartment_info", "Compartment selector metadata for the complete snapshot.")
		for _, id := range ids {
			c := s.Compartments[id]
			fmt.Fprintf(&b, "oci_compute_compartment_info{compartment_id=\"%s\",compartment_name=\"%s\",compartment_path=\"%s\"} 1\n", escape(id), escape(c.Name), escape(c.Path))
		}
		gauge("oci_compute_running_instances", "Compute API RUNNING instances, including VM and bare metal.")
		for _, r := range s.Regions {
			for _, id := range ids {
				c := s.Compartments[id]
				fmt.Fprintf(&b, "oci_compute_running_instances{compartment_id=\"%s\",compartment_name=\"%s\",compartment_path=\"%s\",region=\"%s\"} %d\n", escape(id), escape(c.Name), escape(c.Path), escape(r), s.Counts[location{r, id}])
			}
		}
	}
	return b.String()
}

type store struct {
	sync.RWMutex
	s snapshot
}

func (st *store) publish(s snapshot) {
	st.Lock()
	defer st.Unlock()
	if !s.Success {
		s.LastSuccess = st.s.LastSuccess
	}
	st.s = s
}
func (st *store) handler(interval time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		st.RLock()
		defer st.RUnlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, render(st.s, interval))
	})
	return mux
}
func envInt(name string, fallback int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	i, e := strconv.Atoi(v)
	if e != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, e)
	}
	return i, nil
}
func run() error {
	once := flag.Bool("once", false, "Run a read-only scan, print metrics, and exit")
	mode := flag.String("auth", "workload", "Authentication: workload (OKE), instance (OCI Compute), resource (Container Instances), config (local --once only)")
	configFile := flag.String("config-file", "", "Explicit local OCI configuration file; requires --auth config --once")
	profile := flag.String("profile", "DEFAULT", "OCI configuration file profile (local validation only)")
	flag.Parse()
	options := authOptions{mode: *mode, configFile: *configFile, profile: *profile, once: *once}
	if err := options.validate(); err != nil {
		return err
	}
	tenancy := os.Getenv("OCI_TENANCY_OCID")
	if *mode == "config" {
		provider, err := options.provider()
		if err != nil {
			return err
		}
		tenancy, err = provider.TenancyOCID()
		if err != nil {
			return err
		}
	}
	if tenancy == "" {
		return errors.New("OCI_TENANCY_OCID is required")
	}

	interval, e := envInt("POLL_INTERVAL_SECONDS", 300)
	if e != nil {
		return e
	}
	workers, e := envInt("COLLECTOR_WORKERS", 4)
	if e != nil {
		return e
	}
	timeout, e := envInt("COLLECTION_TIMEOUT_SECONDS", 600)
	if e != nil {
		return e
	}
	if interval < 30 || workers < 1 || workers > 16 || timeout < 1 {
		return errors.New("poll interval must be >=30, workers 1..16, and collection timeout positive")
	}
	region := os.Getenv("OCI_RESOURCE_PRINCIPAL_REGION")
	if *mode == "config" {
		provider, err := options.provider()
		if err != nil {
			return err
		}
		region, err = provider.Region()
		if err != nil {
			return err
		}
	}
	if region == "" {
		region = "us-ashburn-1"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	period := time.Duration(interval) * time.Second
	scan := func(api backend) snapshot {
		c, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
		return collect(c, api, tenancy, workers)
	}
	if *once {
		api, err := newOCIBackend(options, region, tenancy)
		if err != nil {
			return err
		}
		s := scan(api)
		fmt.Print(render(s, period))
		if !s.Success {
			return errors.New("inventory incomplete")
		}
		return nil
	}
	st := &store{}
	go func() {
		var api backend
		for {
			started := time.Now()
			var err error
			if api == nil {
				api, err = newOCIBackend(options, region, tenancy)
			}
			var s snapshot
			if err != nil {
				log.Printf("Inventory authentication failed: %v", err)
				s = snapshot{Errors: 1, LastAttempt: started, Duration: time.Since(started).Seconds()}
			} else {
				s = scan(api)
			}
			st.publish(s)
			total := 0
			for _, n := range s.Counts {
				total += n
			}
			log.Printf("Inventory refresh complete: success=%t regions=%d compartments=%d running=%d errors=%d duration=%.1fs", s.Success, len(s.Regions), len(s.Compartments), total, s.Errors, s.Duration)
			timer := time.NewTimer(period)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	server := &http.Server{Addr: ":9108", Handler: st.handler(period), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(c)
	}()
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
