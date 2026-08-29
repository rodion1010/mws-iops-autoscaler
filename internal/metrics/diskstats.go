package metrics

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// diskStats is one device's line from /proc/diskstats — the cumulative
// counters the kernel maintains since boot. Field order and meaning:
// https://www.kernel.org/doc/Documentation/admin-guide/iostats.rst
// Sector size for this file is always 512 bytes, regardless of the device's
// actual logical block size — a long-standing kernel ABI convention, not
// something read from the device.
type diskStats struct {
	device string

	readsCompleted uint64
	sectorsRead    uint64
	msReading      uint64

	writesCompleted uint64
	sectorsWritten  uint64
	msWriting       uint64

	msDoingIO    uint64 // field 13, io_ticks — not split by direction
	weightedMsIO uint64 // field 14, time_in_queue — not split by direction either
}

const diskstatsSectorBytes = 512

// parseDiskstats reads every device line in /proc/diskstats, keyed by
// kernel device name (e.g. "sdb"). Lines with fewer than the 14 required
// fields (older kernels, or a malformed line) are skipped rather than
// failing the whole read — one bad line must not blind the sampler to
// every other device.
func parseDiskstats(r io.Reader) (map[string]diskStats, error) {
	out := make(map[string]diskStats)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 14 {
			continue
		}
		nums := make([]uint64, 11)
		ok := true
		// fields[0], fields[1] are major/minor — not needed.
		for i, f := range fields[3:14] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				ok = false
				break
			}
			nums[i] = v
		}
		if !ok {
			continue
		}
		device := fields[2]
		out[device] = diskStats{
			device:          device,
			readsCompleted:  nums[0],
			sectorsRead:     nums[2],
			msReading:       nums[3],
			writesCompleted: nums[4],
			sectorsWritten:  nums[6],
			msWriting:       nums[7],
			msDoingIO:       nums[9],
			weightedMsIO:    nums[10],
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading diskstats: %w", err)
	}
	return out, nil
}

// computeSample derives a rate/average Sample from two cumulative snapshots
// of the same device. ok is false when the counters went backwards (a
// device reset, or wraparound) — the caller should discard this interval
// and simply re-baseline, not emit a garbage sample.
func computeSample(diskID string, prev, curr diskStats, elapsed time.Duration, ts time.Time) (Sample, bool) {
	if curr.readsCompleted < prev.readsCompleted ||
		curr.sectorsRead < prev.sectorsRead ||
		curr.msReading < prev.msReading ||
		curr.writesCompleted < prev.writesCompleted ||
		curr.sectorsWritten < prev.sectorsWritten ||
		curr.msWriting < prev.msWriting ||
		curr.weightedMsIO < prev.weightedMsIO {
		return Sample{}, false
	}

	secs := elapsed.Seconds()
	if secs <= 0 {
		return Sample{}, false
	}

	readOpsDelta := curr.readsCompleted - prev.readsCompleted
	writeOpsDelta := curr.writesCompleted - prev.writesCompleted

	s := Sample{
		DiskID:     diskID,
		Timestamp:  ts,
		ReadOps:    float64(readOpsDelta) / secs,
		WriteOps:   float64(writeOpsDelta) / secs,
		ReadBytes:  float64(curr.sectorsRead-prev.sectorsRead) * diskstatsSectorBytes / secs,
		WriteBytes: float64(curr.sectorsWritten-prev.sectorsWritten) * diskstatsSectorBytes / secs,
		// aqu-sz: weighted time-in-queue is milliseconds of queue-seconds
		// accumulated: dividing by elapsed milliseconds gives the average
		// number of requests queued or in flight (sysstat's own formula).
		QueueDepth: QueueDepth{
			Value: float64(curr.weightedMsIO-prev.weightedMsIO) / (secs * 1000),
			Unit:  QueueDepthOps,
		},
		Source: SourceGuest,
	}
	if readOpsDelta > 0 {
		s.ReadLatAvg = time.Duration(float64(curr.msReading-prev.msReading) / float64(readOpsDelta) * float64(time.Millisecond))
	}
	if writeOpsDelta > 0 {
		s.WriteLatAvg = time.Duration(float64(curr.msWriting-prev.msWriting) / float64(writeOpsDelta) * float64(time.Millisecond))
	}
	return s, true
}
