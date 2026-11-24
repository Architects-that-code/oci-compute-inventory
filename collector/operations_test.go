package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeOperations struct {
	sample func(location, metricSignal) (map[string]metricSample, error)
	events func(location, string) ([]maintenanceEvent, string, error)
}

func (f fakeOperations) MetricSamples(_ context.Context, loc location, signal metricSignal, start, end time.Time) (map[string]metricSample, error) {
	if end.Sub(start) != 15*time.Minute {
		return nil, errors.New("incorrect metric lookback")
	}
	return f.sample(loc, signal)
}

func (f fakeOperations) MaintenanceEvents(_ context.Context, loc location, page string) ([]maintenanceEvent, string, error) {
	return f.events(loc, page)
}

func TestOperationsKeepInventoryAndDistinguishMissingTelemetryFromQueryFailure(t *testing.T) {
	inventoryAPI := fixture()
	inventoryAPI.instances = func(_ context.Context, r, c, p string) ([]instance, string, error) {
		switch (location{r, c}) {
		case location{"iad", "root"}:
			return []instance{
				{ID: "app-vm", State: "RUNNING", Name: "App VM", Shape: "VM.Standard3.Flex", Owner: "app-team"},
				{ID: "agent-vm", State: "RUNNING", Shape: "VM.Standard3.Flex"},
				{ID: "inactive", State: "STOPPED", Shape: "VM.Standard3.Flex"},
			}, "", nil
		case location{"lhr", "child"}:
			return []instance{{ID: "database", State: "RUNNING", Name: "DB server", Shape: "BM.Standard3.64"}}, "", nil
		}
		return nil, "", nil
	}
	s := collect(context.Background(), inventoryAPI, "root", 4)
	if !s.Success || total(s) != 3 {
		t.Fatal("fixture inventory is incomplete")
	}
	window := time.Date(2026, 11, 30, 15, 0, 0, 0, time.UTC)
	api := fakeOperations{
		sample: func(loc location, signal metricSignal) (map[string]metricSample, error) {
			switch signal.Name {
			case "agent_cpu":
				if loc.Region == "lhr" {
					return nil, errors.New("permission denied")
				}
				return map[string]metricSample{"agent-vm": {At: time.Unix(1795550000, 0), Value: 42.5}}, nil
			case "vm_accessibility":
				return map[string]metricSample{
					"app-vm": {Value: 1}, "inactive": {Value: 1}, "database": {Value: 1},
				}, nil
			case "bare_metal_hardware":
				return map[string]metricSample{"database": {Value: 2}}, nil
			}
			return nil, nil
		},
		events: func(loc location, page string) ([]maintenanceEvent, string, error) {
			if loc.Region == "lhr" {
				return []maintenanceEvent{{ID: "partial-page", InstanceID: "database"}}, "next", nil
			}
			if page == "" {
				return []maintenanceEvent{{ID: "irrelevant", InstanceID: "inactive"}}, "next", nil
			}
			return []maintenanceEvent{
				{ID: "reboot-event", InstanceID: "app-vm", State: "SCHEDULED", Action: "REBOOT_MIGRATION", WindowStart: window, Deadline: window.Add(time.Hour)},
				{ID: "no-deadline", InstanceID: "app-vm", State: "STARTED"},
			}, "", nil
		},
	}
	s.Operations = collectOperations(context.Background(), api, s, 4)
	if s.Operations.Success || !s.Success {
		t.Fatal("instance monitoring and inventory status should be independent")
	}

	output := render(s, 5*time.Minute)
	for _, want := range []string{
		"oci_compute_inventory_collection_success 1\n",
		"oci_compute_inventory_instance_monitoring_enabled 1\n",
		"oci_compute_inventory_instance_monitoring_collection_success 0\n",
		"oci_compute_inventory_instance_signal_query_success{region=\"lhr\",compartment_id=\"child\",compartment_path=\"/Project/Nested\",signal=\"agent_cpu\"} 0\n",
		"oci_compute_inventory_instance_signal_query_success{region=\"lhr\",compartment_id=\"child\",compartment_path=\"/Project/Nested\",signal=\"maintenance\"} 0\n",
		`owner="app-team",environment="",signal="vm_accessibility"} 1`,
		`environment="",signal="bare_metal_hardware"} 2`,
		`window_start="2026-11-30T15:00:00Z",hard_deadline="2026-11-30T16:00:00Z"} 1`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Count(output, "oci_compute_inventory_instance_info{") != 3 {
		t.Fatal("roster must include every running instance even when a telemetry query fails")
	}
	if strings.Count(output, "oci_compute_inventory_instance_cpu_utilization_percent{") != 1 ||
		!strings.Contains(output, `environment=""} 42.5`) ||
		!strings.Contains(output, `environment=""} 1.79555e+09`) {
		t.Fatal("CPU values and source timestamps must be emitted only for observed samples")
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, `instance_id="inactive"`) || strings.Contains(line, "partial-page") || strings.Contains(line, "irrelevant") {
			t.Errorf("unexpected instance or partial maintenance result: %s", line)
		}
		if strings.HasPrefix(line, "oci_compute_inventory_instance_agent_cpu_samples_present{") {
			missing := strings.Contains(line, `instance_id="app-vm"`) && strings.HasSuffix(line, "} 0")
			present := strings.Contains(line, `instance_id="agent-vm"`) && strings.HasSuffix(line, "} 1")
			if !missing && !present {
				t.Errorf("invalid coverage row: %s", line)
			}
		}
		if strings.HasPrefix(line, "oci_compute_inventory_instance_maintenance_deadline_timestamp_seconds{") && !strings.Contains(line, `event_id="reboot-event"`) {
			t.Errorf("deadline for the wrong event: %s", line)
		}
	}
	if strings.Count(output, "oci_compute_inventory_instance_agent_cpu_samples_present{") != 2 ||
		strings.Count(output, "oci_compute_inventory_instance_maintenance_event{") != 2 ||
		strings.Count(output, "oci_compute_inventory_instance_maintenance_deadline_timestamp_seconds{") != 1 {
		t.Fatal(output)
	}
}
