package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBackend struct {
	regions      func(context.Context) ([]region, error)
	compartments func(context.Context, string) ([]compartment, string, error)
	instances    func(context.Context, string, string, string) ([]instance, string, error)
}

func (f fakeBackend) Regions(c context.Context) ([]region, error) { return f.regions(c) }
func (f fakeBackend) Compartments(c context.Context, p string) ([]compartment, string, error) {
	return f.compartments(c, p)
}
func (f fakeBackend) Instances(c context.Context, r, d, p string) ([]instance, string, error) {
	return f.instances(c, r, d, p)
}
func fixture() fakeBackend {
	return fakeBackend{
		regions: func(context.Context) ([]region, error) {
			return []region{{"iad", "READY"}, {"lhr", "READY"}, {"pending", "IN_PROGRESS"}}, nil
		},
		compartments: func(_ context.Context, p string) ([]compartment, string, error) {
			if p == "next" {
				return []compartment{{"child", "Nested", "project", "ACTIVE"}}, "", nil
			}
			return []compartment{{"project", "Project", "root", "ACTIVE"}, {"deleted", "Gone", "root", "DELETED"}}, "next", nil
		},
		instances: func(context.Context, string, string, string) ([]instance, string, error) { return nil, "", nil },
	}
}
func total(s snapshot) int {
	n := 0
	for _, v := range s.Counts {
		n += v
	}
	return n
}
func TestCompleteScanPaginationAndStates(t *testing.T) {
	api := fixture()
	var mu sync.Mutex
	calls := 0
	api.instances = func(_ context.Context, r, c, p string) ([]instance, string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		if r == "iad" && c == "root" {
			if p == "next" {
				return []instance{{ID: "vm", Compartment: "root", State: "RUNNING"}, {ID: "baremetal", Compartment: "root", State: "RUNNING"}}, "", nil
			}
			return []instance{{ID: "vm", Compartment: "root", State: "RUNNING"}, {ID: "stopped", Compartment: "root", State: "STOPPED"}}, "next", nil
		}
		if r == "lhr" && c == "child" {
			return []instance{{ID: "nested-vm", Compartment: "child", State: "RUNNING"}}, "", nil
		}
		return nil, "", nil
	}
	s := collect(context.Background(), api, "root", 4)
	if !s.Success || total(s) != 3 || len(s.Counts) != 6 || calls != 7 {
		t.Fatalf("incomplete/incorrect scan: %+v calls=%d", s, calls)
	}
	if s.Compartments["child"].Path != "/Project/Nested" || s.Counts[location{"iad", "root"}] != 2 || s.RegionSuccess["iad"] != 1 || s.RegionSuccess["lhr"] != 1 {
		t.Fatal(s)
	}
	if s.LastSuccess.IsZero() || s.Duration <= 0 {
		t.Fatal("missing scan timing")
	}
}
func TestCompartmentMovesDeduplicated(t *testing.T) {
	api := fixture()
	api.instances = func(_ context.Context, r, c, p string) ([]instance, string, error) {
		if r == "iad" && c != "child" {
			return []instance{{ID: "moving", Compartment: "project", State: "RUNNING"}}, "", nil
		}
		return nil, "", nil
	}
	s := collect(context.Background(), api, "root", 2)
	if !s.Success || total(s) != 1 || s.Counts[location{"iad", "project"}] != 1 {
		t.Fatal(s)
	}
}
func TestFailureHidesPartialCounts(t *testing.T) {
	api := fixture()
	api.instances = func(_ context.Context, r, c, p string) ([]instance, string, error) {
		if r == "lhr" {
			return nil, "", errors.New("not authorized")
		}
		return []instance{{ID: "vm", Compartment: "root", State: "RUNNING"}}, "", nil
	}
	s := collect(context.Background(), api, "root", 2)
	text := render(s, 300*time.Second)
	if s.Success || s.Counts != nil || s.RegionSuccess["lhr"] != 0 || s.RegionSuccess["iad"] != 1 || !strings.Contains(text, "collection_success 0\n") || strings.Contains(text, "oci_compute_running_instances{") {
		t.Fatal(text)
	}
}
func TestDiscoveryFailures(t *testing.T) {
	tests := []struct {
		name string
		edit func(*fakeBackend)
	}{
		{"no regions", func(a *fakeBackend) { a.regions = func(context.Context) ([]region, error) { return nil, nil } }},
		{"region authorization", func(a *fakeBackend) {
			a.regions = func(context.Context) ([]region, error) { return nil, errors.New("denied") }
		}},
		{"compartment authorization", func(a *fakeBackend) {
			a.compartments = func(context.Context, string) ([]compartment, string, error) { return nil, "", errors.New("denied") }
		}},
		{"missing ancestor", func(a *fakeBackend) {
			a.compartments = func(context.Context, string) ([]compartment, string, error) {
				return []compartment{{"c", "Child", "missing", "ACTIVE"}}, "", nil
			}
		}},
		{"ancestry cycle", func(a *fakeBackend) {
			a.compartments = func(context.Context, string) ([]compartment, string, error) {
				return []compartment{{"c", "Child", "c", "ACTIVE"}}, "", nil
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := fixture()
			test.edit(&api)
			s := collect(context.Background(), api, "root", 2)
			if s.Success || s.Errors == 0 || strings.Contains(render(s, time.Minute), "oci_compute_running_instances{") {
				t.Fatal(s)
			}
		})
	}
}
func TestPaginationLoopsFailClosed(t *testing.T) {
	api := fixture()
	api.instances = func(context.Context, string, string, string) ([]instance, string, error) { return nil, "repeated", nil }
	s := collect(context.Background(), api, "root", 2)
	if s.Success || s.Errors != 6 || s.Counts != nil {
		t.Fatal(s)
	}
}
func TestCanceledScanFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := collect(ctx, fixture(), "root", 2)
	if s.Success || s.Counts != nil {
		t.Fatal(s)
	}
}
func TestNewCompartmentDuringScanFailsClosed(t *testing.T) {
	api := fixture()
	api.instances = func(context.Context, string, string, string) ([]instance, string, error) {
		return []instance{{ID: "new", Compartment: "unknown", State: "RUNNING"}}, "", nil
	}
	s := collect(context.Background(), api, "root", 2)
	if s.Success || s.Errors != 6 || s.Counts != nil {
		t.Fatal(s)
	}
}
func TestMetricsEscapingAndHTTP(t *testing.T) {
	if got := escape("A\"B\\C\nD"); got != "A\\\"B\\\\C\\nD" {
		t.Fatal(got)
	}
	st := &store{}
	h := st.handler(5 * time.Minute)
	for _, path := range []string{"/healthz", "/metrics"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 200 {
			t.Fatal(r.Code)
		}
	}
	good := collect(context.Background(), fixture(), "root", 2)
	st.publish(good)
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(r.Body.String(), "oci_compute_running_instances{") || !strings.Contains(r.Header().Get("Content-Type"), "version=0.0.4") {
		t.Fatal(r.Body.String())
	}
	st.publish(snapshot{Errors: 1})
	r = httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(r.Body.String(), "oci_compute_running_instances{") || !st.s.LastSuccess.Equal(good.LastSuccess) {
		t.Fatal(r.Body.String())
	}
	r = httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/missing", nil))
	if r.Code != 404 {
		t.Fatal(r.Code)
	}
}
func TestConcurrentScrapeAndRefresh(t *testing.T) {
	st := &store{}
	h := st.handler(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				st.publish(snapshot{Errors: 1})
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil))
			}
		}()
	}
	wg.Wait()
}
