package clock

import (
	"context"
	"testing"
	"time"

	"agentbob/contract"
)

func TestOffsetAppliedUTC(t *testing.T) {
	old := offsetNanos.Load()
	defer offsetNanos.Store(old)

	offsetNanos.Store(int64(5 * time.Second))
	now := Now()
	if now.Location() != time.UTC {
		t.Errorf("Now() must be UTC, got %v", now.Location())
	}
	delta := now.Sub(time.Now().UTC())
	if delta < 4*time.Second || delta > 6*time.Second {
		t.Fatalf("offset not applied: delta=%v want ~5s", delta)
	}
	// UnixEpoch / UnixSeconds reflect the same shifted instant.
	if got := UnixEpoch(); got < time.Now().Unix()+4 {
		t.Errorf("UnixEpoch not shifted: %d", got)
	}
	if got := UnixSeconds(); got < float64(time.Now().Unix())+4 {
		t.Errorf("UnixSeconds not shifted: %f", got)
	}
}

// fakeDB returns a fixed server epoch (nanoseconds) from QueryRow.Scan.
type fakeDB struct{ epochNanos int64 }

func (f fakeDB) QueryRow(context.Context, string, ...any) contract.Row {
	return fakeRow{f.epochNanos}
}
func (fakeDB) Exec(context.Context, string, ...any) (contract.Result, error) {
	return nil, nil
}
func (fakeDB) Query(context.Context, string, ...any) (contract.Rows, error) { return nil, nil }
func (fakeDB) Begin(context.Context) (contract.Tx, error)                   { return nil, nil }

type fakeRow struct{ epochNanos int64 }

func (r fakeRow) Scan(dst ...any) error {
	if p, ok := dst[0].(*int64); ok {
		*p = r.epochNanos
	}
	return nil
}

func TestCalibrateComputesOffset(t *testing.T) {
	old := offsetNanos.Load()
	defer offsetNanos.Store(old)
	offsetNanos.Store(0)

	// Server is 100s AHEAD of local.
	serverEpoch := time.Now().Add(100 * time.Second).UnixNano()
	if err := calibrate(context.Background(), fakeDB{epochNanos: serverEpoch}); err != nil {
		t.Fatal(err)
	}
	off := time.Duration(offsetNanos.Load())
	if off < 99*time.Second || off > 101*time.Second {
		t.Fatalf("offset = %v, want ~100s", off)
	}
	// And Now() is then ~100s ahead of the raw local clock.
	if d := Now().Sub(time.Now().UTC()); d < 99*time.Second || d > 101*time.Second {
		t.Fatalf("Now() not shifted by offset: %v", d)
	}
}
