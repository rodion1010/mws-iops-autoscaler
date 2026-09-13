// Package platformfake implements the Compute and metadata wire protocols
// needed by integration tests. Hand-written fixtures are decoded by the real SDK.
package platformfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Disk constants are resource names, not VM attachment aliases.
const (
	Project         = "iops-test-project"
	Zone            = "ru-central1-a"
	VMName          = "iops-test-vm"
	DependentDisk   = "iops-test-vm-boot"
	IndependentDisk = "iops-test-data"
)

// diskState is the fake's mutable view of one disk resource.
type diskState struct {
	iops        int64
	etag        string
	incarnation string
}

// Fake is an in-process double for the MWS Compute API and instance
// metadata service. Construct with New; it registers its own cleanup.
type Fake struct {
	computeServer  *httptest.Server
	metadataServer *httptest.Server

	mu         sync.Mutex
	disks      map[string]diskState // keyed by disk resource name, see DependentDisk/IndependentDisk
	requests   []Request
	idempotent map[string]int64       // idempotency key -> IOPS applied when the key was first used
	injections map[string][]injection // "METHOD path" -> queued fake responses, consumed FIFO
	applies    map[string]int         // disk resource name -> count of actual state mutations (excludes idempotency replays)
}

// Request records a write before replay, injection, or validation. Status is
// zero when AbortNext drops the connection.
type Request struct {
	Method         string
	Path           string
	Query          string
	IdempotencyKey string
	Status         int
}

// injection is a queued response consumed before validation and mutation.
type injection struct {
	abort  bool
	status int
	body   string
}

// New starts the fake's Compute and metadata servers and seeds disk state.
// Both servers are closed via t.Cleanup.
func New(t *testing.T) *Fake {
	t.Helper()

	f := &Fake{
		disks: map[string]diskState{
			DependentDisk:   {iops: 1000, etag: "disk-etag-vm-boot-1", incarnation: "fake-incarnation-vm-boot"},
			IndependentDisk: {iops: 1000, etag: "disk-etag-1", incarnation: "01234567-89ab-cdef-0123-456789abcdef"},
		},
		idempotent: make(map[string]int64),
		injections: make(map[string][]injection),
		applies:    make(map[string]int),
	}

	computeMux := http.NewServeMux()
	computeMux.HandleFunc("GET /endpoint", f.handleDiscovery)
	computeMux.HandleFunc("GET /compute/v1/projects/{project}/virtualMachines/{vm}", f.handleGetVirtualMachine)
	computeMux.HandleFunc("POST /compute/v1/projects/{project}/virtualMachines/{vm}", f.handleUpdateVirtualMachine)
	computeMux.HandleFunc("GET /compute/v1/projects/{project}/disks/{disk}", f.handleGetDisk)
	computeMux.HandleFunc("POST /compute/v1/projects/{project}/disks/{disk}", f.handleUpdateDisk)
	computeMux.HandleFunc("GET /compute/v1/vmTypes/{vmType}", f.handleGetVmType)
	// SDK error decoding requires JSON even for unmatched routes.
	computeMux.HandleFunc("/", handleNotFound)
	// Assign before starting so handleDiscovery can read the server safely.
	f.computeServer = httptest.NewUnstartedServer(computeMux)
	f.computeServer.Start()

	metadataMux := http.NewServeMux()
	metadataMux.HandleFunc("GET /computeMetadata/v1/instance/name", f.handleMetadataName)
	metadataMux.HandleFunc("GET /computeMetadata/v1/instance/id", f.handleMetadataID)
	metadataMux.HandleFunc("GET /computeMetadata/v1/instance/zone", f.handleMetadataZone)
	f.metadataServer = httptest.NewServer(metadataMux)

	t.Cleanup(func() {
		f.computeServer.Close()
		f.metadataServer.Close()
	})

	return f
}

// BaseURL is the fake's Compute/discovery server address — set as
// MWS_BASE_ENDPOINT.
func (f *Fake) BaseURL() string {
	return f.computeServer.URL
}

// MetadataURL is the fake's instance metadata server address — set as
// MWS_COMPUTE_METADATA_HOST.
func (f *Fake) MetadataURL() string {
	return f.metadataServer.URL
}

// SetEnv points the SDK at the fake and avoids an IAM round trip.
func (f *Fake) SetEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MWS_BASE_ENDPOINT", f.BaseURL())
	t.Setenv("MWS_TOKEN", "fake-token")
	t.Setenv("MWS_PROJECT", Project)
	t.Setenv("MWS_COMPUTE_METADATA_HOST", f.MetadataURL())
}

func (f *Fake) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, []discoveryEntry{{ID: "compute", Address: f.computeServer.URL}})
}

func (f *Fake) handleGetVirtualMachine(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("project") != Project || r.PathValue("vm") != VMName {
		writeNotFound(w, "no such virtual machine")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	bootIOPS := f.disks[DependentDisk].iops

	writeJSON(w, newVMFixture(bootIOPS))
}

func (f *Fake) handleGetDisk(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("project") != Project {
		writeNotFound(w, "no such disk")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.disks[r.PathValue("disk")]
	if !ok {
		writeNotFound(w, "no such disk")
		return
	}

	writeJSON(w, newDiskFixture(state))
}

func (f *Fake) handleGetVmType(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, vmTypeFixture{Spec: vmTypeSpecFixture{Cpu: vmTypeCpuFixture{VcpuCount: "2"}}})
}

// handleUpdateDisk checks replay before injection, validation, and mutation.
func (f *Fake) handleUpdateDisk(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	idx := f.recordRequestLocked(r)
	var status int
	defer func() { f.requests[idx].Status = status }()
	w = &statusRecordingWriter{ResponseWriter: w, status: &status}

	if r.PathValue("project") != Project {
		writeNotFound(w, "no such disk")
		return
	}
	diskName := r.PathValue("disk")
	if _, ok := f.disks[diskName]; !ok {
		writeNotFound(w, "no such disk")
		return
	}

	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" {
		if _, seen := f.idempotent[idemKey]; seen {
			writeJSON(w, newDiskFixture(f.disks[diskName]))
			return
		}
	}

	if inj, ok := f.takeInjectionLocked(r.Method, r.URL.Path); ok {
		writeInjection(w, inj)
		return
	}

	var body updateDiskRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeBadRequest(w, "invalid request body: "+err.Error())
		return
	}
	if msg, ok := validateIOPS(body.Spec.Iops); !ok {
		writeBadRequest(w, msg)
		return
	}

	state := f.disks[diskName]
	state.iops = body.Spec.Iops
	state.etag = bumpEtag(state.etag)
	f.disks[diskName] = state
	f.applies[diskName]++
	if idemKey != "" {
		f.idempotent[idemKey] = body.Spec.Iops
	}

	writeJSON(w, newDiskFixture(state))
}

// handleUpdateVirtualMachine resolves attachment aliases to resource names.
func (f *Fake) handleUpdateVirtualMachine(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	idx := f.recordRequestLocked(r)
	var status int
	defer func() { f.requests[idx].Status = status }()
	w = &statusRecordingWriter{ResponseWriter: w, status: &status}

	if r.PathValue("project") != Project || r.PathValue("vm") != VMName {
		writeNotFound(w, "no such virtual machine")
		return
	}

	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey != "" {
		if _, seen := f.idempotent[idemKey]; seen {
			writeJSON(w, newVMFixture(f.disks[DependentDisk].iops))
			return
		}
	}

	if inj, ok := f.takeInjectionLocked(r.Method, r.URL.Path); ok {
		writeInjection(w, inj)
		return
	}

	var body updateVMRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeBadRequest(w, "invalid request body: "+err.Error())
		return
	}

	var targetDisk string
	var targetIOPS int64
	found := false
	for _, d := range body.Spec.Storage.Disks {
		if d.Disk.Spec == nil {
			continue
		}
		diskName, ok := aliasToDiskName[d.Name]
		if !ok {
			writeBadRequest(w, "unknown disk alias "+d.Name)
			return
		}
		targetDisk, targetIOPS, found = diskName, d.Disk.Spec.Iops, true
		break
	}
	if !found {
		// Nothing tracked changed; echo current state.
		writeJSON(w, newVMFixture(f.disks[DependentDisk].iops))
		return
	}

	if msg, ok := validateIOPS(targetIOPS); !ok {
		writeBadRequest(w, msg)
		return
	}

	state := f.disks[targetDisk]
	state.iops = targetIOPS
	state.etag = bumpEtag(state.etag)
	f.disks[targetDisk] = state
	f.applies[targetDisk]++
	if idemKey != "" {
		f.idempotent[idemKey] = targetIOPS
	}

	writeJSON(w, newVMFixture(f.disks[DependentDisk].iops))
}

// aliasToDiskName joins VM attachments to disk-resource state.
var aliasToDiskName = map[string]string{
	"bootdisk": DependentDisk,
	"datadisk": IndependentDisk,
}

// updateDiskRequestBody contains the only request field the fake consumes.
type updateDiskRequestBody struct {
	Spec struct {
		Iops int64 `json:"iops"`
	} `json:"spec"`
}

// updateVMRequestBody contains the attachment fields the fake consumes.
type updateVMRequestBody struct {
	Spec struct {
		Storage struct {
			Disks []struct {
				Name string `json:"name"`
				Disk struct {
					Spec *struct {
						Iops int64 `json:"iops"`
					} `json:"spec"`
				} `json:"disk"`
			} `json:"disks"`
		} `json:"storage"`
	} `json:"spec"`
}

// recordRequestLocked appends r to the request log and returns its index,
// so the caller can fill in Status once the handler's outcome is known.
// Callers must hold f.mu.
func (f *Fake) recordRequestLocked(r *http.Request) int {
	f.requests = append(f.requests, Request{
		Method:         r.Method,
		Path:           r.URL.Path,
		Query:          r.URL.RawQuery,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	return len(f.requests) - 1
}

// statusRecordingWriter captures the effective response status.
type statusRecordingWriter struct {
	http.ResponseWriter
	status *int
}

func (s *statusRecordingWriter) WriteHeader(status int) {
	*s.status = status
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecordingWriter) Write(b []byte) (int, error) {
	if *s.status == 0 {
		*s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// takeInjectionLocked pops the next queued FailNext/AbortNext response for
// method+path (path only, no query string), if any. Callers must hold f.mu.
func (f *Fake) takeInjectionLocked(method, path string) (injection, bool) {
	key := method + " " + path
	q := f.injections[key]
	if len(q) == 0 {
		return injection{}, false
	}
	f.injections[key] = q[1:]
	return q[0], true
}

// AbortNext uses http.ErrAbortHandler to drop the connection without a response.
func writeInjection(w http.ResponseWriter, inj injection) {
	if inj.abort {
		panic(http.ErrAbortHandler)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(inj.status)
	_, _ = w.Write([]byte(inj.body))
}

// validateIOPS mirrors measured platform bounds except the distinct zero message.
func validateIOPS(iops int64) (msg string, ok bool) {
	switch {
	case iops > 10000:
		return "Requested iops can't be greater than maximum iops value: 10000", false
	case iops < 1000:
		return "Requested iops can't be less than minimum iops value: 1000", false
	case iops%100 != 0:
		return fmt.Sprintf("iops count %d must be divisible by 100", iops), false
	default:
		return "", true
	}
}

// bumpEtag models etag rotation after a successful write.
func bumpEtag(etag string) string {
	return etag + "'"
}

// writeBadRequest writes a 400 shaped like common.ApiError, classified
// INVALID_ARGUMENT — see writeNotFound for why the shape and Content-Type
// matter to the SDK's decoder.
func writeBadRequest(w http.ResponseWriter, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(apiErrorFixture{Code: "INVALID_ARGUMENT", Description: description})
}

// Requests returns every request recorded so far on the write paths, in
// arrival order.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Request, len(f.requests))
	copy(out, f.requests)
	return out
}

// DiskIOPS returns the fake's current view of a disk's spec.iops, keyed by
// cloud resource name (DependentDisk/IndependentDisk) regardless of which
// write path last set it.
func (f *Fake) DiskIOPS(name string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disks[name].iops
}

// Applies counts actual mutations and excludes idempotency replays.
func (f *Fake) Applies(diskName string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applies[diskName]
}

// FailNext queues one response for the next matching fresh request.
func (f *Fake) FailNext(method, path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := method + " " + path
	f.injections[key] = append(f.injections[key], injection{status: status, body: body})
}

// AbortNext drops the next matching connection before applying the request.
func (f *Fake) AbortNext(method, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := method + " " + path
	f.injections[key] = append(f.injections[key], injection{abort: true})
}

// handleMetadataName serves the instance metadata service's plain-text
// contract — unlike the Compute API, this response is not JSON.
func (f *Fake) handleMetadataName(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(VMName))
}

// handleMetadataID serves the documented "projects/{project}/virtualMachines/{vm}"
// resource ID shape (docs/platform-reference.md).
func (f *Fake) handleMetadataID(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("projects/" + Project + "/virtualMachines/" + VMName))
}

// handleMetadataZone serves the fake VM's bare zone.
func (f *Fake) handleMetadataZone(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(Zone))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleNotFound is the Compute server's catch-all for any request whose
// path this fake does not implement at all.
func handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeNotFound(w, "no route for "+r.Method+" "+r.URL.Path)
}

// writeNotFound emits the JSON shape required by SDK error decoding.
func writeNotFound(w http.ResponseWriter, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(apiErrorFixture{Code: "NOT_FOUND", Description: description})
}

// apiErrorFixture contains the fields required for SDK classification.
type apiErrorFixture struct {
	Code        string `json:"code"`
	Description string `json:"description,omitempty"`
}

// discoveryEntry mirrors the wire shape of GET /endpoint entries.
type discoveryEntry struct {
	ID      string `json:"id"`
	Address string `json:"address"`
}

// VM resource references must be bare strings for the generated decoder.
type vmFixture struct {
	Metadata vmMetadataFixture `json:"metadata"`
	Spec     vmSpecFixture     `json:"spec"`
	Status   vmStatusFixture   `json:"status"`
}

type vmMetadataFixture struct {
	Etag string `json:"etag"`
}

type vmSpecFixture struct {
	VmType  string        `json:"vmType"`
	Storage vmSpecStorage `json:"storage"`
}

type vmSpecStorage struct {
	Disks []vmSpecDisk `json:"disks"`
}

type vmSpecDisk struct {
	Name string         `json:"name"`
	Boot bool           `json:"boot,omitempty"`
	Disk vmSpecDiskBody `json:"disk"`
}

// vmSpecDiskBody is the oneOf(ref, spec) attachment body: exactly one of Ref
// (independent — attached by reference) or Spec (dependent — owned inline)
// is set per entry.
type vmSpecDiskBody struct {
	Ref  *string         `json:"ref,omitempty"`
	Spec *vmSpecDiskSpec `json:"spec,omitempty"`
}

type vmSpecDiskSpec struct {
	DiskType string `json:"diskType"`
	// Size must match bytesize.ByteSize's regexp: an SI-style unit suffix
	// ("10GB"), not IEC ("10Gi") — the latter fails to decode.
	Size string `json:"size"`
	Iops int64  `json:"iops"`
}

type vmStatusFixture struct {
	Storage vmStatusStorage `json:"storage"`
}

type vmStatusStorage struct {
	Disks []vmStatusDisk `json:"disks"`
}

type vmStatusDisk struct {
	Name       string `json:"name"`
	DeviceName string `json:"deviceName"`
	Ref        string `json:"ref"`
}

func newVMFixture(bootIOPS int64) vmFixture {
	independentDisk := IndependentDisk
	return vmFixture{
		Metadata: vmMetadataFixture{Etag: "vm-etag-1"},
		Spec: vmSpecFixture{
			VmType: "base-2-8",
			Storage: vmSpecStorage{Disks: []vmSpecDisk{
				{
					Name: "bootdisk",
					Boot: true,
					Disk: vmSpecDiskBody{Spec: &vmSpecDiskSpec{DiskType: "nbs-pl2", Size: "10GB", Iops: bootIOPS}},
				},
				{
					Name: "datadisk",
					Disk: vmSpecDiskBody{Ref: &independentDisk},
				},
			}},
		},
		Status: vmStatusFixture{Storage: vmStatusStorage{Disks: []vmStatusDisk{
			{Name: "bootdisk", DeviceName: "mws-bootdisk", Ref: DependentDisk},
			{Name: "datadisk", DeviceName: "mws-datadisk", Ref: IndependentDisk},
		}}},
	}
}

// The diskFixture* types mirror the subset of
// go.mws.cloud/go-sdk/service/compute/model.DiskOptionalResponse that
// GetDisk (internal/compute/sdk_client.go) reads.
type diskFixture struct {
	Metadata diskMetadataFixture `json:"metadata"`
	Spec     diskSpecFixture     `json:"spec"`
	Status   diskStatusFixture   `json:"status"`
}

type diskMetadataFixture struct {
	Etag string `json:"etag"`
	ID   string `json:"id"`
}

type diskSpecFixture struct {
	Iops int64 `json:"iops"`
}

type diskStatusFixture struct {
	Iops diskStatusIops `json:"iops"`
}

type diskStatusIops struct {
	Base  int64 `json:"base"`
	Extra int64 `json:"extra"`
	Total int64 `json:"total"`
}

func newDiskFixture(s diskState) diskFixture {
	return diskFixture{
		Metadata: diskMetadataFixture{Etag: s.etag, ID: s.incarnation},
		Spec:     diskSpecFixture{Iops: s.iops},
		Status:   diskStatusFixture{Iops: diskStatusIops{Base: s.iops, Extra: 0, Total: s.iops}},
	}
}

// VcpuCount is quoted because the SDK decodes it with StringInt32.
type vmTypeFixture struct {
	Spec vmTypeSpecFixture `json:"spec"`
}

type vmTypeSpecFixture struct {
	Cpu vmTypeCpuFixture `json:"cpu"`
}

type vmTypeCpuFixture struct {
	VcpuCount string `json:"vcpuCount"`
}
