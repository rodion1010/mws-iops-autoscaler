package compute

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	"go.mws.cloud/go-sdk/pkg/imds"
	"go.mws.cloud/util-toolset/pkg/os/env"
)

// instanceNameKey, instanceIDKey and instanceZoneKey are the instance
// metadata keys ResolveSelfIdentity reads (docs/platform-reference.md "VM
// identity metadata keys"). All three are confirmed against a live VM.
const (
	instanceNameKey = "instance/name"
	instanceIDKey   = "instance/id"
	instanceZoneKey = "instance/zone"
)

// selfResourceIDPattern matches the documented instance/id shape,
// "projects/{project}/virtualMachines/{vm}" (docs/platform-reference.md).
var selfResourceIDPattern = regexp.MustCompile(`^projects/([^/]+)/virtualMachines/([^/]+)$`)

// selfZonePattern matches the documented instance/zone shape: a region
// prefix followed by a single-letter availability-zone suffix, e.g.
// "ru-central1-a" (docs/platform-reference.md).
var selfZonePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*-[a-z]$`)

// SelfIdentity is the running VM's own project, zone and name, resolved
// from the instance metadata service in one coherent read.
type SelfIdentity struct {
	Project string
	Zone    string
	VMName  string
}

// ResolveSelfIdentity resolves the running VM's project, zone and own name
// from the instance metadata service — local mode only, where the agent
// runs inside the VM it manages; there is no metadata service to ask in
// fleet mode, where VMs are named in config instead (US-003).
//
// instance/id's "projects/{project}/virtualMachines/{vm}" is parsed and
// its VM name must agree with instance/name — an inconsistency between the
// two is rejected rather than trusting one over the other.
func ResolveSelfIdentity(ctx context.Context) (SelfIdentity, error) {
	client := imds.NewClient(http.DefaultClient, env.RealEnv{})

	name, err := client.GetWithContext(ctx, instanceNameKey)
	if err != nil {
		return SelfIdentity{}, fmt.Errorf("resolve own VM name from the metadata service: %w", err)
	}
	if name == "" {
		return SelfIdentity{}, fmt.Errorf("metadata service returned an empty VM name for %q", instanceNameKey)
	}

	id, err := client.GetWithContext(ctx, instanceIDKey)
	if err != nil {
		return SelfIdentity{}, fmt.Errorf("resolve own VM resource ID from the metadata service: %w", err)
	}
	project, idVMName, err := parseSelfResourceID(id)
	if err != nil {
		return SelfIdentity{}, fmt.Errorf("parse %q from the metadata service: %w", instanceIDKey, err)
	}
	if idVMName != name {
		return SelfIdentity{}, fmt.Errorf(
			"metadata service is internally inconsistent: %q names VM %q but %q names VM %q",
			instanceIDKey, idVMName, instanceNameKey, name)
	}

	zoneRaw, err := client.GetWithContext(ctx, instanceZoneKey)
	if err != nil {
		return SelfIdentity{}, fmt.Errorf("resolve own zone from the metadata service: %w", err)
	}
	zone, err := parseSelfZone(zoneRaw)
	if err != nil {
		return SelfIdentity{}, fmt.Errorf("parse %q from the metadata service: %w", instanceZoneKey, err)
	}

	return SelfIdentity{Project: project, Zone: zone, VMName: name}, nil
}

// parseSelfResourceID extracts project and VM name from the documented
// instance/id shape. Both parts are required non-empty: the regexp's
// [^/]+ groups already reject an empty capture.
func parseSelfResourceID(raw string) (project, vmName string, err error) {
	m := selfResourceIDPattern.FindStringSubmatch(raw)
	if m == nil {
		return "", "", fmt.Errorf(
			"%q does not match the documented \"projects/{project}/virtualMachines/{vm}\" shape", raw)
	}
	return m[1], m[2], nil
}

// parseSelfZone accepts only the documented bare zone shape (e.g.
// "ru-central1-a"), rejecting a malformed or qualified value rather than
// trying to salvage one from it.
func parseSelfZone(raw string) (string, error) {
	if !selfZonePattern.MatchString(raw) {
		return "", fmt.Errorf("%q is not a zone in the documented form (e.g. \"ru-central1-a\")", raw)
	}
	return raw, nil
}
