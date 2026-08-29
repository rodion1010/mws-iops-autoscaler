package metrics

import (
	"strings"
	"testing"
	"time"
)

const sampleDiskstats = `   8       0 sda 12345 678 901234 5678 9012 345 678901 2345 0 6789 12345
   8      16 sdb 1000 12 8000 500 2000 34 16000 1000 0 1500 3000
   8      32 sdc 1 1 1 1 1 1 1 1 1 1 1
`

func TestParseDiskstats(t *testing.T) {
	got, err := parseDiskstats(strings.NewReader(sampleDiskstats))
	if err != nil {
		t.Fatalf("parseDiskstats: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	sdb, ok := got["sdb"]
	if !ok {
		t.Fatal("sdb missing from parsed result")
	}
	want := diskStats{
		device:          "sdb",
		readsCompleted:  1000,
		sectorsRead:     8000,
		msReading:       500,
		writesCompleted: 2000,
		sectorsWritten:  16000,
		msWriting:       1000,
		msDoingIO:       1500,
		weightedMsIO:    3000,
	}
	if sdb != want {
		t.Errorf("sdb = %+v, want %+v", sdb, want)
	}
}

func TestParseDiskstats_malformedLineSkipped(t *testing.T) {
	input := "not a valid line at all\n" + sampleDiskstats
	got, err := parseDiskstats(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseDiskstats: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("len = %d, want 3 (malformed line skipped, not fatal)", len(got))
	}
}

func TestComputeSample(t *testing.T) {
	prev := diskStats{
		readsCompleted: 1000, sectorsRead: 8000, msReading: 500,
		writesCompleted: 2000, sectorsWritten: 16000, msWriting: 1000,
		weightedMsIO: 3000,
	}
	curr := diskStats{
		readsCompleted: 1100, sectorsRead: 8800, msReading: 600,
		writesCompleted: 2200, sectorsWritten: 17600, msWriting: 1400,
		weightedMsIO: 8000,
	}
	ts := time.Unix(100, 0)

	got, ok := computeSample("disk-1", prev, curr, 10*time.Second, ts)
	if !ok {
		t.Fatal("computeSample returned ok=false")
	}

	want := Sample{
		DiskID:      "disk-1",
		Timestamp:   ts,
		ReadOps:     10.0,
		WriteOps:    20.0,
		ReadBytes:   40960.0,
		WriteBytes:  81920.0,
		QueueDepth:  QueueDepth{Value: 0.5, Unit: QueueDepthOps},
		ReadLatAvg:  1 * time.Millisecond,
		WriteLatAvg: 2 * time.Millisecond,
		Source:      SourceGuest,
	}
	if got != want {
		t.Errorf("computeSample = %+v, want %+v", got, want)
	}
}

func TestComputeSample_noOpsThisIntervalLeavesLatencyZero(t *testing.T) {
	prev := diskStats{readsCompleted: 100, writesCompleted: 100, weightedMsIO: 100}
	curr := diskStats{readsCompleted: 100, writesCompleted: 100, weightedMsIO: 100} // no change
	got, ok := computeSample("disk-1", prev, curr, 10*time.Second, time.Now())
	if !ok {
		t.Fatal("computeSample returned ok=false")
	}
	if got.ReadOps != 0 || got.WriteOps != 0 {
		t.Errorf("ReadOps/WriteOps = %v/%v, want 0/0", got.ReadOps, got.WriteOps)
	}
	if got.ReadLatAvg != 0 || got.WriteLatAvg != 0 {
		t.Errorf("no ops this interval must not divide by zero: ReadLatAvg=%v WriteLatAvg=%v", got.ReadLatAvg, got.WriteLatAvg)
	}
}

func TestComputeSample_countersWentBackwards(t *testing.T) {
	prev := diskStats{readsCompleted: 1000}
	curr := diskStats{readsCompleted: 900} // a reset, or wraparound
	_, ok := computeSample("disk-1", prev, curr, 10*time.Second, time.Now())
	if ok {
		t.Fatal("computeSample returned ok=true for counters that went backwards")
	}
}

func TestComputeSample_zeroElapsedRejected(t *testing.T) {
	prev := diskStats{readsCompleted: 1000}
	curr := diskStats{readsCompleted: 1000}
	_, ok := computeSample("disk-1", prev, curr, 0, time.Now())
	if ok {
		t.Fatal("computeSample returned ok=true for zero elapsed time")
	}
}
