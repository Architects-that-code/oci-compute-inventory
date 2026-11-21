package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
)

type ociBackend struct {
	provider common.ConfigurationProvider
	identity identity.IdentityClient
	clients  map[string]core.ComputeClient
	mu       sync.Mutex
	retry    common.RetryPolicy
	tenancy  string
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

func newOCIBackend(options authOptions, region, tenancy string) (backend, error) {
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
	return &ociBackend{provider: provider, identity: client, clients: map[string]core.ComputeClient{}, retry: retry, tenancy: tenancy}, nil
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
func (b *ociBackend) Instances(ctx context.Context, region, comp, page string) ([]instance, string, error) {
	b.mu.Lock()
	client, ok := b.clients[region]
	if !ok {
		var e error
		client, e = core.NewComputeClientWithConfigurationProvider(b.provider)
		if e != nil {
			b.mu.Unlock()
			return nil, "", e
		}
		client.SetRegion(region)
		client.HTTPClient = &http.Client{Timeout: 45 * time.Second}
		b.clients[region] = client
	}
	b.mu.Unlock()
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
		out = append(out, instance{*x.Id, c, string(x.LifecycleState)})
	}
	next := ""
	if r.OpcNextPage != nil {
		next = *r.OpcNextPage
	}
	return out, next, nil
}
