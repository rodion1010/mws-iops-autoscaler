package compute

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The dependent update body must contain every alias and only the target IOPS.
func TestBuildDependentUpdateBody_wireShape(t *testing.T) {
	req := DependentUpdateRequest{
		VMName:      "iops-test-vm",
		Etag:        "etag-abc",
		Aliases:     []string{"bootdisk", "datadisk"},
		TargetAlias: "bootdisk",
		TargetIOPS:  5000,
	}

	body, err := buildDependentUpdateBody(req)
	if err != nil {
		t.Fatalf("buildDependentUpdateBody: %v", err)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal for inspection: %v", err)
	}

	if got := decoded["metadata"].(map[string]any)["etag"]; got != "etag-abc" {
		t.Errorf("metadata.etag = %v, want etag-abc", got)
	}

	disks := decoded["spec"].(map[string]any)["storage"].(map[string]any)["disks"].([]any)
	if len(disks) != 2 {
		t.Fatalf("disks entries = %d, want 2", len(disks))
	}

	for _, raw := range disks {
		entry := raw.(map[string]any)
		name := entry["name"]
		switch name {
		case "bootdisk":
			if len(entry) != 2 {
				t.Errorf("target entry has extra fields beyond name+disk: %+v", entry)
			}
			disk, ok := entry["disk"].(map[string]any)
			if !ok {
				t.Fatalf("target entry missing disk: %+v", entry)
			}
			if len(disk) != 1 {
				t.Errorf("target disk has fields beyond spec: %+v", disk)
			}
			spec, ok := disk["spec"].(map[string]any)
			if !ok {
				t.Fatalf("target disk missing spec: %+v", disk)
			}
			if len(spec) != 1 {
				t.Errorf("target spec has fields beyond iops: %+v", spec)
			}
			if iops, _ := spec["iops"].(float64); int64(iops) != 5000 {
				t.Errorf("target iops = %v, want 5000", spec["iops"])
			}
		case "datadisk":
			if len(entry) != 1 {
				t.Errorf("non-target entry carries more than name — must be name-only, got: %+v", entry)
			}
		default:
			t.Errorf("unexpected alias in output: %v", name)
		}
	}

	// Check raw JSON keys so legitimate alias text cannot produce a false positive.
	for _, forbidden := range []string{"\"boot\":", "\"deviceName\":", "\"diskType\":", "\"size\":", "\"ref\":"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("wire body contains forbidden field %q — immutable fields must never be resent: %s", forbidden, raw)
		}
	}
}

func TestBuildDependentUpdateBody_targetAliasNotFound(t *testing.T) {
	req := DependentUpdateRequest{
		VMName:      "iops-test-vm",
		Etag:        "etag-abc",
		Aliases:     []string{"bootdisk", "datadisk"},
		TargetAlias: "no-such-alias",
		TargetIOPS:  5000,
	}

	if _, err := buildDependentUpdateBody(req); err == nil {
		t.Fatal("buildDependentUpdateBody succeeded with a TargetAlias absent from Aliases")
	}
}

// A zero-value SDK client proves the empty-etag guard runs before any network call.
func TestSDKClient_UpdateDependentDisk_refusesEmptyEtag(t *testing.T) {
	c := &sdkClient{}

	_, err := c.UpdateDependentDisk(context.Background(), DependentUpdateRequest{
		VMName:      "iops-test-vm",
		Etag:        "",
		Aliases:     []string{"bootdisk"},
		TargetAlias: "bootdisk",
		TargetIOPS:  5000,
	})
	if err == nil {
		t.Fatal("UpdateDependentDisk succeeded with an empty etag")
	}
	if !strings.Contains(err.Error(), "empty etag") {
		t.Errorf("error = %v, want it to name the empty etag", err)
	}
}
