package arrangement

import "testing"

// The park status is FREE-FORM (40-impl.go reserves only the engine's own six),
// and the engine itself mints rejected_no_upstream. A parked count that
// enumerated known statuses would leave those items inside "Live items" and in
// none of the three splits — invisible exactly when they matter.
func TestParkedTextCountsFreeFormStatuses(t *testing.T) {
	byStatus := map[string]int64{
		"queued":               4,
		"in_flight":            2,
		"blocked":              1,
		"rejected_no_upstream": 3,
		"waiting_customer":     1, // a worker's own park label
	}
	live := int64(0)
	for _, n := range byStatus {
		live += n
	}
	parked := live - byStatus["queued"] - byStatus["in_flight"]
	if parked != 5 {
		t.Fatalf("want 5 parked, got %d", parked)
	}
	want := "5 (blocked 1, rejected_no_upstream 3, waiting_customer 1)"
	if got := parkedText(parked, byStatus); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestParkedTextNothingParked(t *testing.T) {
	byStatus := map[string]int64{"queued": 7, "in_flight": 1}
	if got := parkedText(0, byStatus); got != "0" {
		t.Fatalf("want bare 0, got %q", got)
	}
}

func TestWarnIfPositive(t *testing.T) {
	if got := warnIfPositive(0); got != "" {
		t.Fatalf("zero must not be coloured, got %q", got)
	}
	if got := warnIfPositive(1); got != "warn" {
		t.Fatalf("want warn, got %q", got)
	}
}
