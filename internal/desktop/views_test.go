package desktop

import "testing"

func TestViewPoliciesAreOriginScoped(t *testing.T) {
	trusted, err := NewTrustedViewPolicy("http://127.0.0.1:43100/")
	if err != nil {
		t.Fatal(err)
	}
	serviceA, err := NewServiceViewPolicy("http://127.0.0.1:43101/ui")
	if err != nil {
		t.Fatal(err)
	}
	serviceB, err := NewServiceViewPolicy("http://127.0.0.1:43102/")
	if err != nil {
		t.Fatal(err)
	}

	for _, uri := range []string{"http://127.0.0.1:43100/", "http://127.0.0.1:43100/settings"} {
		if !trusted.AllowNavigation(uri) {
			t.Errorf("trusted policy denied %q", uri)
		}
	}
	for _, uri := range []string{"http://127.0.0.1:43101/", "http://127.0.0.1:43101/other"} {
		if !serviceA.AllowNavigation(uri) {
			t.Errorf("service policy denied its ensured origin %q", uri)
		}
	}
	for _, uri := range []string{
		"http://127.0.0.1:43102/",
		"http://127.0.0.1:43100/",
		"https://127.0.0.1:43101/",
		"http://localhost:43101/",
		"https://example.com/",
	} {
		if serviceA.AllowNavigation(uri) {
			t.Errorf("service policy admitted unrelated origin %q", uri)
		}
	}
	if trusted.AllowNavigation("http://127.0.0.1:43101/") {
		t.Fatal("trusted UI policy admitted a service origin")
	}
	if serviceB.AllowNavigation("http://127.0.0.1:43101/") {
		t.Fatal("service policy admitted another service origin")
	}
	if trusted.AllowNewWindow("http://127.0.0.1:43100/") || serviceA.AllowNewWindow("http://127.0.0.1:43101/") {
		t.Fatal("view policy allowed a popup")
	}
}

func TestViewPolicyRejectsInvalidOrigins(t *testing.T) {
	for _, url := range []string{
		"",
		"https://127.0.0.1:43100/",
		"http://localhost:43100/",
		"http://192.0.2.10:43100/",
		"http://127.0.0.1/",
	} {
		if _, err := NewTrustedViewPolicy(url); err == nil {
			t.Errorf("NewTrustedViewPolicy(%q) succeeded", url)
		}
		if _, err := NewServiceViewPolicy(url); err == nil {
			t.Errorf("NewServiceViewPolicy(%q) succeeded", url)
		}
	}
}

func TestViewConfigRequiresRoleExactOriginAndProfile(t *testing.T) {
	trusted, err := NewTrustedViewPolicy("http://127.0.0.1:43100/")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceViewPolicy("http://127.0.0.1:43101/")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		config ViewConfig
		role   viewPolicyRole
		valid  bool
	}{
		{"trusted config", ViewConfig{URL: "http://127.0.0.1:43100/", ProfileFolder: "trusted", Policy: trusted}, trustedViewPolicy, true},
		{"service config", ViewConfig{URL: "http://127.0.0.1:43101/page", ProfileFolder: "service", Policy: service}, serviceViewPolicy, true},
		{"cross-role service", ViewConfig{URL: "http://127.0.0.1:43101/", ProfileFolder: "service", Policy: trusted}, serviceViewPolicy, false},
		{"cross-origin URL", ViewConfig{URL: "http://127.0.0.1:43102/", ProfileFolder: "service", Policy: service}, serviceViewPolicy, false},
		{"missing profile", ViewConfig{URL: "http://127.0.0.1:43101/", Policy: service}, serviceViewPolicy, false},
		{"legacy union policy", ViewConfig{URL: "http://127.0.0.1:43100/", ProfileFolder: "trusted", Policy: mustLegacyPolicy(t)}, trustedViewPolicy, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateViewConfig(tc.config, tc.role)
			if tc.valid && err != nil {
				t.Fatalf("validateViewConfig() error = %v", err)
			}
			if !tc.valid && err == nil {
				t.Fatal("validateViewConfig() accepted an invalid config")
			}
		})
	}
}

func mustLegacyPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := NewPolicy("http://127.0.0.1:43100/")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
