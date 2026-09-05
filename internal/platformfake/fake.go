// Package platformfake is an in-process fake of the MWS Compute API and
// instance metadata service, used to prove the repository's SDK translation
// code against real go.mws.cloud/go-sdk decoders without credentials or
// network access (docs/design.md §8.2).
//
// Success bodies are hand-written fixtures mirroring the subset of each SDK
// response type its translator in internal/compute/sdk_client.go actually
// reads, not bodies marshalled from the SDK's own model types: the
// generated encoders and decoders are not provably symmetric for every
// optional/oneOf field, so marshalling from the model would have assumed
// the exact thing this fake exists to verify. fake_test.go's untagged
// tests pin these fixtures by round-tripping them through the real SDK
// client. A go.mws.cloud/go-sdk version bump must re-run those tests — a
// new generated decoder could silently accept or reject a fixture shape
// that used to be right.
package platformfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Fixed identifiers shared by every test using the fake. DependentDisk and
// IndependentDisk are cloud resource names (status.storage.disks[].ref), not
// the VM-local attachment aliases ("bootdisk"/"datadisk") — state below is
// keyed the same way so a write reachable by resource name (e.g. through the
// VM update path) and a direct GetDisk on that name observe the same state.
const (
	Project         = "iops-test-project"
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

// Request is one HTTP request the Compute server observed on a write path,
// recorded before idempotency replay, failure injection or bounds
// validation run — so a test can assert a write was attempted regardless of
// how the fake then handled it. Status is filled in afterwards, once the
// handler's outcome is known; it is 0 for a request whose connection was
// dropped by AbortNext instead of receiving a response.
type Request struct {
	Method         string
	Path           string
	Query          string
	IdempotencyKey string
	Status         int
}

// injection is one queued fake response for a method+path pair (FailNext or
// AbortNext), consumed ahead of the handler's own idempotency, validation
// and mutation logic.
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
	// Catch-all: ServeMux's own default 404 has no Content-Type header, which the
	// generated SDK decoders reject before ever reporting the 404 itself (see
	// handleNotFound). Route every unmatched path through the same JSON error path.
	computeMux.HandleFunc("/", handleNotFound)
	// NewUnstartedServer + Start (instead of NewServer, which starts serving
	// before it returns) keeps this assignment ordered strictly before the
	// server's serving goroutine is spawned, so the goroutine's later read of
	// f.computeServer in handleDiscovery has a happens-before edge through
	// that goroutine's start. Plain network I/O between a client and this
	// server would not otherwise provide one.
	f.computeServer = httptest.NewUnstartedServer(computeMux)
	f.computeServer.Start()

	metadataMux := http.NewServeMux()
	metadataMux.HandleFunc("GET /computeMetadata/v1/instance/name", f.handleMetadataName)
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

// SetEnv points the SDK's environment-based configuration at this fake. The
// token value is a placeholder: it only needs to be non-empty so the SDK
// selects its static-credential path instead of trying an IAM round trip.
func (f *Fake) SetEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MWS_BASE_ENDPOINT", f.BaseURL())
	t.Setenv("MWS_TOKEN", "fake-token")
	t.Setenv("MWS_PROJECT", Project)
	t.Setenv("MWS_COMPUTE_METADATA_HOST", f.MetadataURL())
}

// handleDiscovery serves the SDK's service discovery lookup that every
// service client performs before its first call. f.computeServer is set
// once in New before this handler's goroutine can run (see New), so no
// lock is needed to read it here.
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

// handleUpdateDisk implements POST .../disks/{disk}?updateOnly=true, the
// independent-disk write path (UpdateIndependentDisk). Order matters: an
// idempotency-key replay short-circuits before injection or validation even
// run (the platform answers a repeated key from the first result without
// reprocessing the body); a fresh request then consults injection before
// bounds validation, so a test can inject a platform-side failure on a
// request that would otherwise pass the fake's own bounds check, and only
// a request that clears both actually mutates state.
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

// handleUpdateVirtualMachine implements POST
// .../virtualMachines/{vm}?updateOnly=true, the dependent-disk write path
// (UpdateDependentDisk). spec.storage.disks[] names its entries by
// VM-local alias ("bootdisk"), but state.disks is keyed by cloud resource
// name — aliasToDiskName resolves alias to resource name before touching
// state, or an update reachable through this path would leave a later
// DiskIOPS(resourceName) assertion looking unapplied even though the
// write landed under the wrong key.
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
		// No attachment in this request carries a new spec.iops — nothing
		// to apply. Echo current state back, matching an update that only
		// touches fields this fake does not track.
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

// aliasToDiskName resolves a VM attachment alias (spec.storage.disks[].name,
// unique only within the VM) to the cloud disk resource name that
// state.disks is keyed by; see handleUpdateVirtualMachine.
var aliasToDiskName = map[string]string{
	"bootdisk": DependentDisk,
	"datadisk": IndependentDisk,
}

// updateDiskRequestBody is the minimal shape handleUpdateDisk needs from
// UpdateDiskRequest's wire body — spec.iops only, matching what
// UpdateIndependentDisk (internal/compute/sdk_client.go) actually sends:
// {"metadata":{"etag":"..."},"spec":{"iops":N}}.
type updateDiskRequestBody struct {
	Spec struct {
		Iops int64 `json:"iops"`
	} `json:"spec"`
}

// updateVMRequestBody is the minimal shape handleUpdateVirtualMachine needs
// from UpdateVirtualMachineRequest's wire body, matching what
// UpdateDependentDisk (internal/compute/sdk_client.go) actually sends: every
// current alias in spec.storage.disks[], each carrying only "name" except
// the target alias, which additionally carries disk.spec.iops, e.g.
// {"spec":{"storage":{"disks":[{"name":"bootdisk","disk":{"spec":{"iops":N}}},{"name":"datadisk"}]}}}.
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

// statusRecordingWriter wraps http.ResponseWriter to capture the status code
// a handler writes, defaulting to 200 when the handler only calls Write
// without an explicit WriteHeader (matching net/http's own behaviour).
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

// writeInjection renders a queued injection. AbortNext's entry panics with
// http.ErrAbortHandler, which net/http's connection handler recognizes and
// turns into a silently dropped connection — no response written at all —
// rather than a logged panic (see net/http.Server's conn.serve recover).
func writeInjection(w http.ResponseWriter, inj injection) {
	if inj.abort {
		panic(http.ErrAbortHandler)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(inj.status)
	_, _ = w.Write([]byte(inj.body))
}

// validateIOPS enforces the platform's real disk IOPS bounds
// (docs/platform-reference.md "Server-side validation, measured", all
// [V]). ok is false and msg carries the platform's verbatim 400 message
// when iops is out of range.
// Known divergence from the platform: iops == 0 is not special-cased here,
// so it falls through to the "less than minimum" message below. The real
// platform instead answers 0 with a distinct message, "Invalid request:
// diskUpsertRequest.spec.iops has invalid value." (docs/platform-reference.md
// "Server-side validation, measured").
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

// bumpEtag returns a value that differs from etag, standing in for the
// platform's own etag rotation on every successful write — a caller that
// re-reads after an update must observe a changed etag for optimistic
// concurrency to mean anything.
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

// Applies returns how many times a write to diskName (by cloud resource
// name) actually mutated seeded state, counted at the point state changes
// in handleUpdateDisk or handleUpdateVirtualMachine. An idempotency-key
// replay short-circuits before that point and does not count, so this
// distinguishes "applied once" from "applied, then applied again" even
// when both leave the same final value and the same request-log shape.
func (f *Fake) Applies(diskName string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applies[diskName]
}

// FailNext queues a single fake HTTP status+body response for the next
// request matching method and the URL path (no query string). Consumed
// once; a second matching request falls through to the fake's own
// handling. A fresh request consults injection before bounds validation;
// an idempotency-key replay (a repeated Idempotency-Key header) is answered
// from the first result before either runs — use a key not already used by
// the target path if the test needs the injection to actually fire.
func (f *Fake) FailNext(method, path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := method + " " + path
	f.injections[key] = append(f.injections[key], injection{status: status, body: body})
}

// AbortNext queues a dropped connection — no HTTP response at all — for the
// next request matching method and the URL path (no query string).
// Simulates a network failure mid-request, distinct from FailNext's
// well-formed error response.
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleNotFound is the Compute server's catch-all for any request whose
// path this fake does not implement at all.
func handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeNotFound(w, "no route for "+r.Method+" "+r.URL.Path)
}

// writeNotFound writes a 404 shaped like common.ApiError — the wire body the
// generated SDK decoders require for every non-2xx status, "code" included:
// ApiError.Decode treats "code" as a required field and errors without it,
// and separately, an SDK decoder that receives a non-"application/json"
// Content-Type (plain http.NotFound's default) discards the body and
// reports "invalid Content-Type" instead of ever surfacing the 404. Either
// mistake would turn a deliberate routing-mismatch check into a decode
// error that looks like a fake bug instead of the real 404 it represents.
func writeNotFound(w http.ResponseWriter, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(apiErrorFixture{Code: "NOT_FOUND", Description: description})
}

// apiErrorFixture mirrors the subset of
// go.mws.cloud/go-sdk/service/common/model.ApiError that error responses
// must carry. Code must be one of the SDK's known status strings (here,
// "NOT_FOUND") for mwserrors.IsAPIErrorNotFoundStatus and friends to
// recognize it; an unrecognized string still decodes but classifies as
// errors.Unknown.
type apiErrorFixture struct {
	Code        string `json:"code"`
	Description string `json:"description,omitempty"`
}

// discoveryEntry mirrors the wire shape of GET /endpoint entries.
type discoveryEntry struct {
	ID      string `json:"id"`
	Address string `json:"address"`
}

// The vmFixture* types mirror the subset of
// go.mws.cloud/go-sdk/service/compute/model.VirtualMachineOptionalResponse
// that translateVMResponse (internal/compute/sdk_client.go) reads. Resource
// references (vmType, disk ref) decode as bare strings (their generated
// UnmarshalJSON rejects an object), not the {"disk": "..."} shape a first
// reading of the OpenAPI schema suggests.
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

// vmTypeFixture mirrors the subset of
// go.mws.cloud/go-sdk/service/compute/model.VmTypeOptionalResponse that
// GetVmType (internal/compute/sdk_client.go) reads. VcpuCount decodes with
// decode.StringInt32, which requires a quoted numeric string, not a bare
// number.
type vmTypeFixture struct {
	Spec vmTypeSpecFixture `json:"spec"`
}

type vmTypeSpecFixture struct {
	Cpu vmTypeCpuFixture `json:"cpu"`
}

type vmTypeCpuFixture struct {
	VcpuCount string `json:"vcpuCount"`
}
