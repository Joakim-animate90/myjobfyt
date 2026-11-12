package events

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestDeadLetterMonitorConfigDefaults(t *testing.T) {
	// A zero interval panics time.NewTicker, which would take the worker down on a
	// config omission.
	cfg := DeadLetterMonitorConfig{}.withDefaults()
	if cfg.Interval <= 0 {
		t.Fatalf("Interval = %v; time.NewTicker panics on a non-positive duration", cfg.Interval)
	}

	explicit := DeadLetterMonitorConfig{Interval: 5 * time.Second}.withDefaults()
	if explicit.Interval != 5*time.Second {
		t.Errorf("withDefaults overrode an explicit interval: %v", explicit.Interval)
	}
}

func TestDeadLetterDepthIsAGaugeNotACounter(t *testing.T) {
	// The alerting question is "is anything still waiting for a human", which a counter
	// cannot answer: dead_lettered_total only ever increases, so a threshold on it
	// fires for ever after one incident. A gauge must therefore be able to go down.
	eventsDeadLetterDepth.WithLabelValues("TEST").Set(7)
	if got := testutil.ToFloat64(eventsDeadLetterDepth.WithLabelValues("TEST")); got != 7 {
		t.Fatalf("depth = %v, want 7", got)
	}

	eventsDeadLetterDepth.WithLabelValues("TEST").Set(0)
	if got := testutil.ToFloat64(eventsDeadLetterDepth.WithLabelValues("TEST")); got != 0 {
		t.Fatalf("depth = %v after draining, want 0; the metric cannot decrease, so it "+
			"would alert for ever after a single incident", got)
	}
}

func TestStaleDepthIsDistinctFromZeroDepth(t *testing.T) {
	// "I cannot read the depth" must never render as "nothing is waiting". If a failed
	// read zeroed the depth gauge, a monitor that lost its NATS connection would
	// silence the dead-letter alert precisely while work piled up unseen.
	eventsDeadLetterDepth.WithLabelValues("STALE_TEST").Set(3)
	eventsDeadLetterDepthStale.WithLabelValues("STALE_TEST").Set(1)

	depth := testutil.ToFloat64(eventsDeadLetterDepth.WithLabelValues("STALE_TEST"))
	stale := testutil.ToFloat64(eventsDeadLetterDepthStale.WithLabelValues("STALE_TEST"))

	if depth == 0 {
		t.Error("the depth gauge was zeroed on a failed read, which reads as \"nothing waiting\"")
	}
	if stale != 1 {
		t.Errorf("stale = %v, want 1 so the alert can distrust the depth", stale)
	}
}

func TestDeadLetterStreamNameMatchesTheTopology(t *testing.T) {
	// The monitor reads this stream by name. If the topology renamed it, the monitor
	// would report a permanent zero for a stream that does not exist, which is the
	// worst possible failure for this particular metric: silently reassuring.
	var found bool
	for _, spec := range Streams() {
		if spec.Name == dlqStreamName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no stream named %q in the topology; the depth monitor would report a "+
			"permanent zero", dlqStreamName)
	}
}
