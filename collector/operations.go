package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

const agentLookback = 15 * time.Minute

type metricSignal struct {
	Name, Namespace, Metric, ShapePrefix string
}

var healthSignals = []metricSignal{
	{"vm_infrastructure", "oci_compute_infrastructure_health", "instance_status", "VM."},
	{"vm_accessibility", "oci_compute_instance_health", "instance_accessibility_status", "VM."},
	{"vm_filesystem", "oci_compute_instance_health", "InstanceFileSystemStatus", "VM."},
	{"bare_metal_hardware", "oci_compute_infrastructure_health", "health_status", "BM."},
}

var cpuSignal = metricSignal{"agent_cpu", "oci_computeagent", "CpuUtilization", ""}

type metricSample struct {
	At    time.Time
	Value float64
}

type maintenanceEvent struct {
	ID, InstanceID, State, Action, Category string
	WindowStart, Deadline                   time.Time
}

type operationsBackend interface {
	MetricSamples(context.Context, location, metricSignal, time.Time, time.Time) (map[string]metricSample, error)
	MaintenanceEvents(context.Context, location, string) ([]maintenanceEvent, string, error)
}

type signalResult struct {
	Signal  metricSignal
	Samples map[string]metricSample
	Success bool
}

type operationsLocation struct {
	Location           location
	Instances          []string
	Signals            []signalResult
	Maintenance        map[string]maintenanceEvent
	MaintenanceSuccess bool
}

type operationsSnapshot struct {
	Locations []operationsLocation
	Success   bool
}

func collectOperations(ctx context.Context, api operationsBackend, s snapshot, workers int) *operationsSnapshot {
	result := &operationsSnapshot{}
	if !s.Success {
		return result
	}

	idsByLocation := map[location][]string{}
	for _, id := range slices.Sorted(maps.Keys(s.Instances)) {
		loc := s.Instances[id].Location
		idsByLocation[loc] = append(idsByLocation[loc], id)
	}
	compartments := slices.Sorted(maps.Keys(s.Compartments))
	for _, region := range s.Regions {
		for _, comp := range compartments {
			loc := location{region, comp}
			ids := idsByLocation[loc]
			if len(ids) == 0 {
				continue
			}
			cell := operationsLocation{Location: loc, Instances: ids}
			for _, signal := range healthSignals {
				if slices.ContainsFunc(ids, func(id string) bool {
					return strings.HasPrefix(s.Instances[id].Shape, signal.ShapePrefix)
				}) {
					cell.Signals = append(cell.Signals, signalResult{Signal: signal})
				}
			}
			cell.Signals = append(cell.Signals, signalResult{Signal: cpuSignal})
			result.Locations = append(result.Locations, cell)
		}
	}

	// Share a single rate limit across workers to leave headroom below OCI's query quota.
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	jobs := make(chan *operationsLocation)
	var wg sync.WaitGroup
	for range min(workers, len(result.Locations)) {
		wg.Go(func() {
			for cell := range jobs {
				collectOperationsLocation(ctx, api, cell, ticker.C)
			}
		})
	}
schedule:
	for i := range result.Locations {
		select {
		case jobs <- &result.Locations[i]:
		case <-ctx.Done():
			break schedule
		}
	}
	close(jobs)
	wg.Wait()

	result.Success = ctx.Err() == nil
	for _, cell := range result.Locations {
		result.Success = result.Success && cell.MaintenanceSuccess
		for _, signal := range cell.Signals {
			result.Success = result.Success && signal.Success
		}
	}
	return result
}

func collectOperationsLocation(ctx context.Context, api operationsBackend, cell *operationsLocation, ticks <-chan time.Time) {
	loc := cell.Location
	events := map[string]maintenanceEvent{}
	err := pageLoop(func(page string) (string, error) {
		items, next, err := api.MaintenanceEvents(ctx, loc, page)
		if err != nil {
			return "", err
		}
		for _, event := range items {
			if _, found := slices.BinarySearch(cell.Instances, event.InstanceID); found {
				events[event.ID] = event
			}
		}
		return next, nil
	})
	if err != nil {
		log.Printf("Instance maintenance scan failed: region=%s compartment=%s error=%v", loc.Region, loc.Compartment, err)
	} else {
		cell.Maintenance = events
		cell.MaintenanceSuccess = true
	}

	for i := range cell.Signals {
		select {
		case <-ticks:
		case <-ctx.Done():
			return
		}
		signal := &cell.Signals[i]
		now := time.Now().UTC()
		samples, err := api.MetricSamples(ctx, loc, signal.Signal, now.Add(-agentLookback), now)
		if err != nil {
			log.Printf("Instance metric query failed: region=%s compartment=%s signal=%s error=%v", loc.Region, loc.Compartment, signal.Signal.Name, err)
			continue
		}
		signal.Samples = samples
		signal.Success = true
	}
}

func renderOperations(b *strings.Builder, s snapshot) {
	gauge := func(name, help string) { fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name) }
	boolean := func(value bool) int {
		if value {
			return 1
		}
		return 0
	}
	const enabled = "oci_compute_inventory_instance_monitoring_enabled"
	const success = "oci_compute_inventory_instance_monitoring_collection_success"
	gauge(enabled, "Whether instance monitoring is enabled.")
	fmt.Fprintf(b, "%s %d\n", enabled, boolean(s.Operations != nil))
	if s.Operations == nil {
		return
	}
	gauge(success, "Whether the latest instance monitoring scan succeeded for every source.")
	fmt.Fprintf(b, "%s %d\n", success, boolean(s.Operations.Success))
	if !s.Success {
		return
	}

	const querySuccess = "oci_compute_inventory_instance_signal_query_success"
	const health = "oci_compute_inventory_instance_health"
	const coverage = "oci_compute_inventory_instance_agent_cpu_samples_present"
	const cpu = "oci_compute_inventory_instance_cpu_utilization_percent"
	const cpuTime = "oci_compute_inventory_instance_cpu_sample_timestamp_seconds"
	const eventInfo = "oci_compute_inventory_instance_maintenance_event"
	const deadline = "oci_compute_inventory_instance_maintenance_deadline_timestamp_seconds"
	gauge(querySuccess, "Whether the latest source query succeeded for a region and compartment with running instances.")
	gauge(health, "Latest OCI health value in the last 15 minutes. Positive values indicate an issue.")
	gauge(coverage, "Whether the instance reported an OCI Compute Agent CPU sample within the last 15 minutes.")
	gauge(cpu, "Latest observed one-minute maximum OCI Compute Agent CPU utilization percent within the last 15 minutes.")
	gauge(cpuTime, "OCI timestamp of the CPU utilization sample as Unix seconds.")
	gauge(eventInfo, "Active OCI Compute maintenance event for a running instance.")
	gauge(deadline, "OCI hard due date for the maintenance event, when supplied, as Unix seconds.")
	for _, cell := range s.Operations.Locations {
		loc := cell.Location
		comp := s.Compartments[loc.Compartment]
		locationLabels := fmt.Sprintf(`region="%s",compartment_id="%s",compartment_path="%s"`,
			escape(loc.Region), escape(loc.Compartment), escape(comp.Path))
		writeSample := func(metric, labels string, value float64) {
			fmt.Fprintf(b, "%s{%s%s} %g\n", metric, locationLabels, labels, value)
		}
		labelsByID := make(map[string]string, len(cell.Instances))
		for _, id := range cell.Instances {
			in := s.Instances[id]
			labelsByID[id] = fmt.Sprintf(`,instance_id="%s",instance_name="%s",shape="%s",availability_domain="%s",fault_domain="%s",owner="%s",environment="%s"`,
				escape(id), escape(in.Name), escape(in.Shape), escape(in.AvailabilityDomain),
				escape(in.FaultDomain), escape(in.Owner), escape(in.Environment))
		}
		writeSample(querySuccess, `,signal="maintenance"`, float64(boolean(cell.MaintenanceSuccess)))
		for _, signal := range cell.Signals {
			signalLabel := fmt.Sprintf(`,signal="%s"`, signal.Signal.Name)
			writeSample(querySuccess, signalLabel, float64(boolean(signal.Success)))
			if !signal.Success {
				continue
			}
			for _, id := range cell.Instances {
				sample, found := signal.Samples[id]
				if signal.Signal == cpuSignal {
					writeSample(coverage, labelsByID[id], float64(boolean(found)))
					if found {
						writeSample(cpu, labelsByID[id], sample.Value)
						writeSample(cpuTime, labelsByID[id], float64(sample.At.Unix()))
					}
				} else if found && strings.HasPrefix(s.Instances[id].Shape, signal.Signal.ShapePrefix) {
					writeSample(health, labelsByID[id]+signalLabel, sample.Value)
				}
			}
		}
		for _, id := range slices.Sorted(maps.Keys(cell.Maintenance)) {
			event := cell.Maintenance[id]
			labels := fmt.Sprintf(`%s,event_id="%s",maintenance_state="%s",maintenance_category="%s",maintenance_action="%s"`,
				labelsByID[event.InstanceID], escape(event.ID), escape(event.State), escape(event.Category), escape(event.Action))
			date := func(value time.Time) string {
				if value.IsZero() {
					return ""
				}
				return value.UTC().Format(time.RFC3339)
			}
			writeSample(eventInfo, labels+fmt.Sprintf(`,window_start="%s",hard_deadline="%s"`, date(event.WindowStart), date(event.Deadline)), 1)
			if !event.Deadline.IsZero() {
				writeSample(deadline, labels, float64(event.Deadline.Unix()))
			}
		}
	}
}
