package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/monitoring"
)

type ociBackend struct {
	provider          common.ConfigurationProvider
	identity          identity.IdentityClient
	clients           map[string]core.ComputeClient
	monitoringClients map[string]monitoring.MonitoringClient
	mu                sync.Mutex
	retry             common.RetryPolicy
	tenancy           string
}

type authOptions struct {
	mode, configFile, profile string
	once                      bool
}

func (o authOptions) validate() error {
	switch o.mode {
	case "workload", "instance", "resource":
		if o.configFile != "" || o.profile != "DEFAULT" {
			return fmt.Errorf("config-file and profile require auth=config")
		}
	case "config":
		if !o.once || o.configFile == "" {
			return fmt.Errorf("auth=config requires --once and an explicit --config-file for local validation")
		}
	default:
		return fmt.Errorf("auth must be workload, instance, resource, or config")
	}
	return nil
}

func (o authOptions) provider() (common.ConfigurationProvider, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	switch o.mode {
	case "workload":
		return auth.OkeWorkloadIdentityConfigurationProvider()
	case "instance":
		return auth.InstancePrincipalConfigurationProvider()
	case "resource":
		return auth.ResourcePrincipalConfigurationProvider()
	case "config":
		return common.ConfigurationProviderFromFileWithProfile(o.configFile, o.profile, "")
	}
	return nil, fmt.Errorf("unsupported auth mode")
}

func newOCIBackend(options authOptions, region, tenancy string) (*ociBackend, error) {
	provider, err := options.provider()
	if err != nil {
		return nil, err
	}

	client, err := identity.NewIdentityClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	client.SetRegion(region)
	client.HTTPClient = &http.Client{Timeout: 45 * time.Second}
	retry := common.DefaultRetryPolicy()
	retry.MaximumNumberAttempts = 4
	return &ociBackend{
		provider: provider, identity: client, retry: retry, tenancy: tenancy,
		clients: map[string]core.ComputeClient{}, monitoringClients: map[string]monitoring.MonitoringClient{},
	}, nil
}
func (b *ociBackend) Regions(ctx context.Context) ([]region, error) {
	r, e := b.identity.ListRegionSubscriptions(ctx, identity.ListRegionSubscriptionsRequest{TenancyId: &b.tenancy, RequestMetadata: common.RequestMetadata{RetryPolicy: &b.retry}})
	if e != nil {
		return nil, e
	}
	// This OCI operation is not paginated. Fail closed if that contract changes.
	if r.RawResponse != nil && r.RawResponse.Header.Get("opc-next-page") != "" {
		return nil, fmt.Errorf("unexpected paginated region-subscription response")
	}
	out := make([]region, 0, len(r.Items))
	for _, x := range r.Items {
		if x.RegionName != nil {
			out = append(out, region{*x.RegionName, string(x.Status)})
		}
	}
	return out, nil
}
func (b *ociBackend) Compartments(ctx context.Context, page string) ([]compartment, string, error) {
	request := identity.ListCompartmentsRequest{CompartmentId: &b.tenancy, CompartmentIdInSubtree: common.Bool(true), AccessLevel: identity.ListCompartmentsAccessLevelAny, LifecycleState: identity.CompartmentLifecycleStateActive, RequestMetadata: common.RequestMetadata{RetryPolicy: &b.retry}}
	if page != "" {
		request.Page = &page
	}
	r, e := b.identity.ListCompartments(ctx, request)
	if e != nil {
		return nil, "", e
	}
	out := make([]compartment, 0, len(r.Items))
	for _, x := range r.Items {
		if x.Id != nil && x.Name != nil && x.CompartmentId != nil {
			out = append(out, compartment{*x.Id, *x.Name, *x.CompartmentId, string(x.LifecycleState)})
		} else {
			return nil, "", fmt.Errorf("incomplete compartment identity")
		}
	}
	next := ""
	if r.OpcNextPage != nil {
		next = *r.OpcNextPage
	}
	return out, next, nil
}
func (b *ociBackend) computeClient(region string) (core.ComputeClient, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	client, ok := b.clients[region]
	if !ok {
		var e error
		client, e = core.NewComputeClientWithConfigurationProvider(b.provider)
		if e != nil {
			return client, e
		}
		client.SetRegion(region)
		client.HTTPClient = &http.Client{Timeout: 45 * time.Second}
		b.clients[region] = client
	}
	return client, nil
}

func (b *ociBackend) Instances(ctx context.Context, region, comp, page string) ([]instance, string, error) {
	client, err := b.computeClient(region)
	if err != nil {
		return nil, "", err
	}
	request := core.ListInstancesRequest{CompartmentId: &comp, LifecycleState: core.InstanceLifecycleStateRunning, RequestMetadata: common.RequestMetadata{RetryPolicy: &b.retry}}
	if page != "" {
		request.Page = &page
	}
	r, e := client.ListInstances(ctx, request)
	if e != nil {
		return nil, "", e
	}
	out := make([]instance, 0, len(r.Items))
	for _, x := range r.Items {
		if x.Id == nil {
			return nil, "", fmt.Errorf("instance response missing OCID")
		}
		c := ""
		if x.CompartmentId != nil {
			c = *x.CompartmentId
		}
		out = append(out, instance{
			ID: *x.Id, Compartment: c, State: string(x.LifecycleState),
			Name: stringValue(x.DisplayName), Shape: stringValue(x.Shape),
			AvailabilityDomain: stringValue(x.AvailabilityDomain), FaultDomain: stringValue(x.FaultDomain),
			Owner: x.FreeformTags["owner"], Environment: x.FreeformTags["environment"],
		})
	}
	next := ""
	if r.OpcNextPage != nil {
		next = *r.OpcNextPage
	}
	return out, next, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (b *ociBackend) monitoringClient(region string) (monitoring.MonitoringClient, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	client, ok := b.monitoringClients[region]
	if !ok {
		var err error
		client, err = monitoring.NewMonitoringClientWithConfigurationProvider(b.provider)
		if err != nil {
			return client, err
		}
		client.SetRegion(region)
		client.HTTPClient = &http.Client{Timeout: 45 * time.Second}
		b.monitoringClients[region] = client
	}
	return client, nil
}

func (b *ociBackend) MetricSamples(ctx context.Context, loc location, signal metricSignal, start, end time.Time) (map[string]metricSample, error) {
	client, err := b.monitoringClient(loc.Region)
	if err != nil {
		return nil, err
	}
	response, err := client.SummarizeMetricsData(ctx, monitoring.SummarizeMetricsDataRequest{
		CompartmentId: &loc.Compartment,
		SummarizeMetricsDataDetails: monitoring.SummarizeMetricsDataDetails{
			Namespace:  &signal.Namespace,
			Query:      common.String(signal.Metric + "[1m].groupBy(resourceId).max()"),
			StartTime:  &common.SDKTime{Time: start},
			EndTime:    &common.SDKTime{Time: end},
			Resolution: common.String("1m"),
		},
		RequestMetadata: common.RequestMetadata{RetryPolicy: &b.retry},
	})
	if err != nil {
		return nil, err
	}
	// OCI limits the returned streams. A response at the limit cannot prove coverage.
	if len(response.Items) >= 2000 {
		return nil, fmt.Errorf("OCI Monitoring response reached the 2,000 stream limit")
	}
	samples := map[string]metricSample{}
	for _, stream := range response.Items {
		id := stream.Dimensions["resourceId"]
		if id == "" {
			return nil, fmt.Errorf("OCI Monitoring response missing the resourceId dimension")
		}
		for _, point := range stream.AggregatedDatapoints {
			if point.Timestamp == nil || point.Value == nil || math.IsNaN(*point.Value) || math.IsInf(*point.Value, 0) {
				return nil, fmt.Errorf("OCI Monitoring response contained an invalid datapoint")
			}
			at := point.Timestamp.Time
			if at.Before(start) || at.After(end) {
				continue
			}
			previous := samples[id]
			if at.After(previous.At) || (at.Equal(previous.At) && *point.Value > previous.Value) {
				samples[id] = metricSample{at, *point.Value}
			}
		}
	}
	return samples, nil
}

func (b *ociBackend) MaintenanceEvents(ctx context.Context, loc location, page string) ([]maintenanceEvent, string, error) {
	client, err := b.computeClient(loc.Region)
	if err != nil {
		return nil, "", err
	}
	request := core.ListInstanceMaintenanceEventsRequest{
		CompartmentId:   &loc.Compartment,
		RequestMetadata: common.RequestMetadata{RetryPolicy: &b.retry},
	}
	if page != "" {
		request.Page = &page
	}
	response, err := client.ListInstanceMaintenanceEvents(ctx, request)
	if err != nil {
		return nil, "", err
	}
	events := make([]maintenanceEvent, 0, len(response.Items))
	for _, event := range response.Items {
		switch event.LifecycleState {
		case core.InstanceMaintenanceEventLifecycleStateSucceeded, core.InstanceMaintenanceEventLifecycleStateFailed, core.InstanceMaintenanceEventLifecycleStateCanceled:
			continue
		}
		if stringValue(event.Id) == "" || stringValue(event.InstanceId) == "" {
			return nil, "", fmt.Errorf("OCI maintenance event missing an OCID")
		}
		item := maintenanceEvent{
			ID: *event.Id, InstanceID: *event.InstanceId, State: string(event.LifecycleState),
			Action: string(event.InstanceAction), Category: string(event.MaintenanceCategory),
		}
		if event.TimeWindowStart != nil {
			item.WindowStart = event.TimeWindowStart.Time
		}
		if event.TimeHardDueDate != nil {
			item.Deadline = event.TimeHardDueDate.Time
		}
		events = append(events, item)
	}
	return events, stringValue(response.OpcNextPage), nil
}
