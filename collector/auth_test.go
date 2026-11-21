package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuthenticationBoundaries(t *testing.T) {
	for _, mode := range []string{"workload", "instance", "resource"} {
		o := authOptions{mode: mode, profile: "DEFAULT"}
		if err := o.validate(); err != nil {
			t.Fatal(err)
		}
		o.configFile = "/local/oci.properties"
		if o.validate() == nil {
			t.Fatalf("%s accepted local credentials", mode)
		}
	}
	for _, o := range []authOptions{
		{mode: "environment", profile: "DEFAULT"},
		{mode: "config", profile: "DEFAULT", configFile: "/local/oci.properties"},
		{mode: "config", profile: "DEFAULT", once: true},
	} {
		if _, err := o.provider(); err == nil {
			t.Fatalf("accepted invalid authentication: %+v", o)
		}
	}
}

func TestExplicitLocalConfigProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oci.properties")
	if err := os.WriteFile(path, []byte("[DEFAULT]\ntenancy=default-tenancy\nregion=us-ashburn-1\n[LOCAL]\ntenancy=local-tenancy\nregion=eu-frankfurt-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF_VAR_tenancy_ocid", "unwanted-tenancy")
	o := authOptions{mode: "config", configFile: path, profile: "LOCAL", once: true}
	p, err := o.provider()
	if err != nil {
		t.Fatal(err)
	}
	tenancy, err := p.TenancyOCID()
	if err != nil || tenancy != "local-tenancy" {
		t.Fatalf("tenancy=%q error=%v", tenancy, err)
	}
	region, err := p.Region()
	if err != nil || region != "eu-frankfurt-1" {
		t.Fatalf("region=%q error=%v", region, err)
	}
	o.configFile = filepath.Join(t.TempDir(), "missing.properties")
	p, err = o.provider()
	if err == nil {
		_, err = p.TenancyOCID()
	}
	if err == nil {
		t.Fatal("missing explicit file accepted")
	}
}
