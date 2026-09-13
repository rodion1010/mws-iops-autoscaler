package compute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newMetadataFake starts an in-process instance metadata service serving
// exactly the given plain-text key/value pairs, and points
// MWS_COMPUTE_METADATA_HOST at it for the duration of the test — the same
// override imds.Client itself already supports (docs/platform-reference.md).
func newMetadataFake(t *testing.T, values map[string]string) {
	t.Helper()
	mux := http.NewServeMux()
	for key, val := range values {
		v := val
		mux.HandleFunc("GET /computeMetadata/v1/"+key, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(v))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("MWS_COMPUTE_METADATA_HOST", srv.URL)
}

func TestSelfVMIdentityFromMetadata(t *testing.T) {
	newMetadataFake(t, map[string]string{
		"instance/name": "my-vm",
		"instance/id":   "projects/my-project/virtualMachines/my-vm",
		"instance/zone": "ru-central1-a",
	})

	got, err := ResolveSelfIdentity(context.Background())
	if err != nil {
		t.Fatalf("ResolveSelfIdentity: %v", err)
	}
	want := SelfIdentity{Project: "my-project", Zone: "ru-central1-a", VMName: "my-vm"}
	if got != want {
		t.Errorf("ResolveSelfIdentity = %+v, want %+v", got, want)
	}
}

// TestSelfVMIdentityRejectsMalformedZone confirms a zone value that is not
// the documented bare shape is rejected rather than salvaged — neither an
// unstructured string nor a qualified path is a zone identity can trust.
func TestSelfVMIdentityRejectsMalformedZone(t *testing.T) {
	for _, zone := range []string{"garbage", "projects/123456789/zones/ru-central1-a"} {
		t.Run(zone, func(t *testing.T) {
			newMetadataFake(t, map[string]string{
				"instance/name": "my-vm",
				"instance/id":   "projects/my-project/virtualMachines/my-vm",
				"instance/zone": zone,
			})

			if _, err := ResolveSelfIdentity(context.Background()); err == nil {
				t.Fatalf("ResolveSelfIdentity succeeded on malformed zone %q, want an error", zone)
			}
		})
	}
}

func TestSelfVMIdentityRejectsMalformedResourceID(t *testing.T) {
	newMetadataFake(t, map[string]string{
		"instance/name": "my-vm",
		"instance/id":   "not-a-resource-id",
		"instance/zone": "ru-central1-a",
	})

	if _, err := ResolveSelfIdentity(context.Background()); err == nil {
		t.Fatal("ResolveSelfIdentity succeeded on a malformed instance/id, want an error")
	}
}

func TestSelfVMIdentityRejectsInconsistentName(t *testing.T) {
	newMetadataFake(t, map[string]string{
		"instance/name": "my-vm",
		"instance/id":   "projects/my-project/virtualMachines/other-vm",
		"instance/zone": "ru-central1-a",
	})

	if _, err := ResolveSelfIdentity(context.Background()); err == nil {
		t.Fatal("ResolveSelfIdentity succeeded despite instance/id and instance/name naming different VMs, want an error")
	}
}

func TestSelfVMIdentityRejectsEmptyZone(t *testing.T) {
	newMetadataFake(t, map[string]string{
		"instance/name": "my-vm",
		"instance/id":   "projects/my-project/virtualMachines/my-vm",
		"instance/zone": "",
	})

	if _, err := ResolveSelfIdentity(context.Background()); err == nil {
		t.Fatal("ResolveSelfIdentity succeeded on an empty zone, want an error")
	}
}

func TestSelfVMIdentityRejectsEmptyName(t *testing.T) {
	newMetadataFake(t, map[string]string{
		"instance/name": "",
		"instance/id":   "projects/my-project/virtualMachines/my-vm",
		"instance/zone": "ru-central1-a",
	})

	if _, err := ResolveSelfIdentity(context.Background()); err == nil {
		t.Fatal("ResolveSelfIdentity succeeded on an empty VM name, want an error")
	}
}
