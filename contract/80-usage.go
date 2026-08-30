package contract

import "context"

// Consumption = a USER's token spend, booked per-handle (the per-user billing
// ledger). It is deliberately named apart from the model pool's own per-backend
// operational telemetry (ModelUsage / model_usage / /model stats): one tracks
// "how much has this user consumed", the other "how is each backend performing".
//
// The consuming user's billing HANDLE (platform-family + stable uid) rides the
// turn ctx behind an unexported key. The flow stamps it once before the turn
// (from ev.AccountHandle()); the model pool reads it per call and reports that
// call's tokens against it — so side-LLM calls (OCR/ASR/compress) deep in the
// turn are attributed for free, with zero threading. Handle-keying (not
// account_id) makes attribution merge-safe and free on the hot path: no store
// lookup to bill, and an unbound user's spend is still recorded (the account is
// the read-time rollup).
//
// The handle is a (source, uid) pair, NOT an exported ctx-value type — the
// membership rule binds exported types in contract, and a billing handle is
// data, not a trunk-mediated capability. Helper FUNCTIONS carry it.

type consumerKeyT struct{}

type consumerHandle struct{ source, uid string }

// WithConsumer stamps the consuming user's billing handle on ctx. A blank handle
// is a no-op (the pool then reports nothing).
func WithConsumer(ctx context.Context, source, uid string) context.Context {
	if source == "" || uid == "" {
		return ctx
	}
	return context.WithValue(ctx, consumerKeyT{}, consumerHandle{source: source, uid: uid})
}

// ConsumerFrom returns the consuming user's billing handle on ctx; ok=false when
// none is set.
func ConsumerFrom(ctx context.Context) (source, uid string, ok bool) {
	h, ok := ctx.Value(consumerKeyT{}).(consumerHandle)
	return h.source, h.uid, ok
}

// ConsumptionReporter receives each model call's real token usage, tagged by the
// kind of model that served it, to be booked against the consuming user's handle
// (read from ctx via ConsumerFrom) — the per-user BILLING ledger. Provided by
// leaf/accounts; the model pool consumes it OPTIONALLY — absent → spend is simply
// not booked. Distinct from the pool's own per-backend ModelUsageStore (ops
// telemetry), so the two ledgers never get conflated.
type ConsumptionReporter interface {
	ReportConsumption(ctx context.Context, kind string, in, out int64)
}
