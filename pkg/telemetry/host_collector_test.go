package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestThreadStatsWithProcfs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "42")
	for _, tid := range []string{"43", "44"} {
		if err := os.MkdirAll(filepath.Join(dir, "task", tid), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Supply a complete Linux stat fixture. A missing stat for TID 44 simulates
	// a thread exiting after task enumeration, which must not fail the scrape.
	fields := make([]string, 42)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0], fields[11], fields[12], fields[17], fields[40] = "S", "150", "50", "2", "100"
	process := "42 (fc worker ) main) " + strings.Join(fields, " ")
	thread := "43 (fc_vcpu 0) " + strings.Join(fields, " ")
	for path, data := range map[string]string{filepath.Join(dir, "stat"): process, filepath.Join(dir, "task", "43", "stat"): thread} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	count, threads, err := readThreadStats(root, 42)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || len(threads) != 1 || threads[0].PID != 43 || threads[0].Comm != "fc_vcpu 0" {
		t.Fatalf("unexpected thread stats: %d %+v", count, threads)
	}
	if threads[0].CPUTime() != 2 {
		t.Fatal("CPU time must include guest time once, not add it again")
	}
	if _, _, err := readThreadStats(root, 99); err == nil {
		t.Fatal("accepted a missing process")
	}
}
