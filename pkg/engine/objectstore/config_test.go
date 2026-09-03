package objectstore

import (
	"strings"
	"testing"
)

const testKey = "core.sessions.object_store"

func TestConfigEnabled(t *testing.T) {
	if (Config{}).Enabled() {
		t.Error("zero Config reports Enabled; the default path must be disabled")
	}
	if !(Config{BackendName: "x"}).Enabled() {
		t.Error("Config with a backend name reports disabled")
	}
}

func TestValidateZeroConfigIsAccepted(t *testing.T) {
	c := Config{}
	if err := c.Validate(testKey); err != nil {
		t.Fatalf("Validate(zero) = %v, want nil", err)
	}
	if c.FailurePolicy != "" {
		t.Errorf("FailurePolicy normalised to %q on a disabled block; want it left empty", c.FailurePolicy)
	}
}

func TestValidateNormalisesFailurePolicy(t *testing.T) {
	registerTemp(t, "vstub", stubFactory)
	c := Config{BackendName: "vstub", Bucket: "b"}
	if err := c.Validate(testKey); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	if c.FailurePolicy != FailurePolicyDegrade {
		t.Errorf("FailurePolicy = %q, want %q (the documented default)", c.FailurePolicy, FailurePolicyDegrade)
	}
}

func TestValidateAcceptsExplicitPolicies(t *testing.T) {
	registerTemp(t, "vstub2", stubFactory)
	for _, p := range []FailurePolicy{FailurePolicyDegrade, FailurePolicyStrict} {
		c := Config{BackendName: "vstub2", Bucket: "b", FailurePolicy: p}
		if err := c.Validate(testKey); err != nil {
			t.Errorf("Validate(policy=%q) = %v, want nil", p, err)
		}
	}
}

func TestValidateErrorsNameTheKey(t *testing.T) {
	registerTemp(t, "vstub3", stubFactory)

	cases := []struct {
		name    string
		cfg     Config
		wantKey string
	}{
		{
			name:    "unregistered backend",
			cfg:     Config{BackendName: "not-a-backend", Bucket: "b"},
			wantKey: testKey + ".backend",
		},
		{
			name:    "missing bucket",
			cfg:     Config{BackendName: "vstub3"},
			wantKey: testKey + ".bucket",
		},
		{
			name:    "leading slash in prefix",
			cfg:     Config{BackendName: "vstub3", Bucket: "b", Prefix: "/nexus"},
			wantKey: testKey + ".prefix",
		},
		{
			name:    "trailing slash in prefix",
			cfg:     Config{BackendName: "vstub3", Bucket: "b", Prefix: "nexus/"},
			wantKey: testKey + ".prefix",
		},
		{
			name:    "bad failure policy",
			cfg:     Config{BackendName: "vstub3", Bucket: "b", FailurePolicy: "explode"},
			wantKey: testKey + ".failure_policy",
		},
		{
			name:    "block filled in but no backend named",
			cfg:     Config{Bucket: "b", Region: "us-east-1"},
			wantKey: testKey + ".bucket",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			err := cfg.Validate(testKey)
			if err == nil {
				t.Fatalf("Validate(%+v) = nil, want an error", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("error %q does not name %q", err, tc.wantKey)
			}
		})
	}
}

func TestValidateUnregisteredBackendExplainsMissingImport(t *testing.T) {
	// The overwhelmingly likely cause of an unregistered name is a forgotten
	// blank import, so the message has to say so rather than just "unknown".
	c := Config{BackendName: "gcs", Bucket: "b"}
	err := c.Validate(testKey)
	if err == nil {
		t.Fatal("Validate = nil for an unregistered backend")
	}
	if !strings.Contains(err.Error(), "import") {
		t.Errorf("error %q does not point at the missing module import", err)
	}
}
