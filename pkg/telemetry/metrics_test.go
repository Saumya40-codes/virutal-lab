package telemetry

import (
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

func newTestMetrics(t *testing.T, id string) (*Metrics, *prometheus.Registry) {
	t.Helper()
	metrics := NewMetrics()
	registry := prometheus.NewRegistry()
	if err := metrics.RegisterMetrics(prometheus.WrapRegistererWith(prometheus.Labels{"vm_id": id}, registry)); err != nil {
		t.Fatal(err)
	}
	return metrics, registry
}

func snapshotText(t *testing.T, registry *prometheus.Registry) string {
	t.Helper()
	data, err := Snapshot(registry, expfmt.NewFormat(expfmt.TypeTextPlain))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertMetric(t *testing.T, registry *prometheus.Registry, name string, value any) {
	t.Helper()
	want := fmt.Sprintf("%s{vm_id=\"test-vm\"} %v\n", name, value)
	if got := snapshotText(t, registry); !strings.Contains(got, want) {
		t.Fatalf("missing %q in:\n%s", want, got)
	}
}

func TestRegistrationFailureRollsBackOnlyNewCollectors(t *testing.T) {
	reg := prometheus.NewRegistry()
	existing := NewMetrics().APIRequests
	if err := reg.Register(existing); err != nil {
		t.Fatal(err)
	}
	metrics := NewMetrics()
	if err := metrics.RegisterMetrics(reg); err == nil {
		t.Fatal("accepted conflicting registration")
	}
	if reg.Unregister(metrics.VMStarts) {
		t.Fatal("partial registration was not rolled back")
	}
	if !reg.Unregister(existing) {
		t.Fatal("rollback removed a preexisting collector")
	}
}
