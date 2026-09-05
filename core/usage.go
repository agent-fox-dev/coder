package core

// UsageField is a presence bit. REQ-PROV-16 permits "a pointer OR an explicit
// presence flag"; a mask is chosen over nine *int64 because Usage is summed on
// every turn, and pointer arithmetic across a summation is where the
// double-counting of REQ-PROV-05.1 actually happens.
type UsageField uint32

const (
	UsageInputTokens UsageField = 1 << iota
	UsageOutputTokens
	UsageReasoningTokens
	UsageCacheReadTokens
	UsageCacheWriteTokens
	UsageCacheWrite1hTokens
	UsageTotalTokens
	UsageCostUSD
)

type Usage struct {
	// InputTokens is NET of cache reads and writes (REQ-PROV-05.1).
	InputTokens  int64
	OutputTokens int64
	// ReasoningTokens is a SUBSET of OutputTokens, not an addend.
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	// CacheWrite1hTokens is a SUBSET of CacheWriteTokens (REQ-PROV-05.3).
	CacheWrite1hTokens int64
	TotalTokens        int64
	CostUSD            float64
	// BilledModel is the model that actually served the request (REQ-PROV-05.5).
	BilledModel string

	// Set records which fields the provider actually reported, so an explicit
	// 0 beats an SDK fallback (REQ-PROV-16.4) and a wholly unreported usage is
	// distinguishable from a reported all-zero one (REQ-GO-15).
	Set UsageField
}

func (u *Usage) SetField(f UsageField, v int64) {
	switch f {
	case UsageInputTokens:
		u.InputTokens = v
	case UsageOutputTokens:
		u.OutputTokens = v
	case UsageReasoningTokens:
		u.ReasoningTokens = v
	case UsageCacheReadTokens:
		u.CacheReadTokens = v
	case UsageCacheWriteTokens:
		u.CacheWriteTokens = v
	case UsageCacheWrite1hTokens:
		u.CacheWrite1hTokens = v
	case UsageTotalTokens:
		u.TotalTokens = v
	}
	u.Set |= f
}

func (u *Usage) SetCost(v float64)    { u.CostUSD = v; u.Set |= UsageCostUSD }
func (u Usage) Has(f UsageField) bool { return u.Set&f != 0 }

// Add sums two usages field-wise and ORs their presence masks. The session
// layer only sums; it never recomputes cost (REQ-PROV-05).
func (u Usage) Add(o Usage) Usage {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.CacheWrite1hTokens += o.CacheWrite1hTokens
	u.TotalTokens += o.TotalTokens
	u.CostUSD += o.CostUSD
	u.Set |= o.Set
	if o.BilledModel != "" {
		u.BilledModel = o.BilledModel
	}
	return u
}

// ContextTokens implements REQ-GO-15's definition exactly: TotalTokens when
// non-zero, else Input+Output+CacheRead+CacheWrite.
func (u Usage) ContextTokens() int64 {
	if u.TotalTokens != 0 {
		return u.TotalTokens
	}
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// Reported is false when the provider reported nothing at all.
func (u Usage) Reported() bool { return u.Set != 0 }

// IsZero is true when nothing was reported OR everything reported was zero —
// the "all-zero-usage response" REQ-GO-15 forbids as an anchor.
func (u Usage) IsZero() bool { return u.ContextTokens() == 0 }

// UsageWire is the decode-side helper providers use: every numeric is a
// pointer so an explicit 0 is distinguishable from absent (REQ-PROV-16.4).
// Into folds it onto a Usage, setting presence bits for exactly the non-nil
// fields.
type UsageWire struct {
	InputTokens        *int64
	OutputTokens       *int64
	ReasoningTokens    *int64
	CacheReadTokens    *int64
	CacheWriteTokens   *int64
	CacheWrite1hTokens *int64
	TotalTokens        *int64
}

func (w UsageWire) Into(u *Usage) {
	for _, p := range []struct {
		f UsageField
		v *int64
	}{
		{UsageInputTokens, w.InputTokens},
		{UsageOutputTokens, w.OutputTokens},
		{UsageReasoningTokens, w.ReasoningTokens},
		{UsageCacheReadTokens, w.CacheReadTokens},
		{UsageCacheWriteTokens, w.CacheWriteTokens},
		{UsageCacheWrite1hTokens, w.CacheWrite1hTokens},
		{UsageTotalTokens, w.TotalTokens},
	} {
		if p.v != nil {
			u.SetField(p.f, *p.v)
		}
	}
}
