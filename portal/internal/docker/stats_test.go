package docker

import (
	"encoding/json"
	"math"
	"testing"
)

// A trimmed real document from GET /containers/{id}/stats?one-shot=true on
// cgroup v2: precpu_stats is empty, as it always is with one-shot.
const oneShotJSON = `{
 "read":"2026-11-12T14:00:00.000000000Z",
 "cpu_stats":{"cpu_usage":{"total_usage":5000000000},"system_cpu_usage":100000000000,"online_cpus":8},
 "precpu_stats":{"cpu_usage":{"total_usage":0},"throttling_data":{}},
 "memory_stats":{"usage":52428800,"stats":{"inactive_file":10485760,"active_file":1},"limit":100000000}
}`

func decode(t *testing.T, s string) Stats {
	t.Helper()
	var raw rawStats
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		t.Fatal(err)
	}
	return raw.parse()
}

func TestParseOneShot(t *testing.T) {
	st := decode(t, oneShotJSON)
	if st.PreCPU.Valid() {
		t.Fatal("one-shot precpu should be invalid")
	}
	if st.CPU.Total != 5e9 || st.CPU.System != 1e11 || st.CPU.CPUs != 8 {
		t.Fatalf("cpu sample %+v", st.CPU)
	}
	// 50 MiB usage - 10 MiB inactive_file = 40 MiB, like docker stats.
	if !st.MemOK || st.Memory != 40<<20 {
		t.Fatalf("memory %d ok=%v", st.Memory, st.MemOK)
	}
}

func TestParseCgroupV1AndEdgeCases(t *testing.T) {
	// cgroup v1: total_inactive_file, CPU count from percpu_usage.
	st := decode(t, `{"cpu_stats":{"cpu_usage":{"total_usage":10,"percpu_usage":[1,2,3,4]},"system_cpu_usage":20},
		"memory_stats":{"usage":1000,"stats":{"total_inactive_file":400}}}`)
	if st.Memory != 600 || st.CPU.CPUs != 4 {
		t.Fatalf("v1: %+v", st)
	}
	// Cache larger than usage (can happen briefly): keep usage, never underflow.
	st = decode(t, `{"memory_stats":{"usage":100,"stats":{"inactive_file":400}}}`)
	if st.Memory != 100 {
		t.Fatalf("underflow guard: %d", st.Memory)
	}
	// Stopped/empty document: no memory reading at all.
	if st := decode(t, `{}`); st.MemOK {
		t.Fatal("empty stats reported memory")
	}
}

func TestCPUPercent(t *testing.T) {
	prev := CPUSample{Total: 1_000_000_000, System: 100_000_000_000, CPUs: 8}
	// 2 s of container CPU while the host (8 cores) accumulated 80 s
	// (= 10 s wall clock): 2/80 * 8 * 100 = 20% (a fifth of one core).
	cur := CPUSample{Total: 3_000_000_000, System: 180_000_000_000, CPUs: 8}
	got, ok := CPUPercent(prev, cur)
	if !ok || math.Abs(got-20) > 1e-9 {
		t.Fatalf("got %v ok=%v, want 20", got, ok)
	}
	// Two full cores busy: 20 s container CPU over 80 s host = 200%.
	if got, _ := CPUPercent(prev, CPUSample{Total: 21_000_000_000, System: 180_000_000_000, CPUs: 8}); math.Abs(got-200) > 1e-9 {
		t.Fatalf("two cores: %v", got)
	}
	for name, c := range map[string][2]CPUSample{
		"no previous":       {{}, cur},
		"no time passed":    {prev, {Total: 2e9, System: prev.System, CPUs: 8}},
		"container restart": {prev, {Total: 10, System: 2e11, CPUs: 8}},
	} {
		if _, ok := CPUPercent(c[0], c[1]); ok {
			t.Errorf("%s: should not produce a value", name)
		}
	}
	// Unknown CPU count falls back to 1 rather than reporting 0.
	if got, ok := CPUPercent(CPUSample{Total: 0, System: 100}, CPUSample{Total: 50, System: 200}); !ok || got != 50 {
		t.Fatalf("cpus=0: %v", got)
	}
}

func TestValidID(t *testing.T) {
	if !ValidID("0123456789ab") || ValidID("../etc") || ValidID("ABCDEF012345") {
		t.Fatal("ValidID")
	}
}
