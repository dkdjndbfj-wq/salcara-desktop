package usageledger

import (
	"math"
	"strings"
	"time"
)

// CostCalibration contains no credential, account name, provider ID or raw
// source handle. Its map key is pseudonymized with the ledger's private salt.
// It is a workload estimate, never an official monetary account entitlement.
type CostCalibration struct {
	Epoch      uint64    `json:"epoch"`
	Plan       string    `json:"plan"`
	Reset      time.Time `json:"reset"`
	Seconds    int64     `json:"seconds"`
	At         time.Time `json:"at"`
	Used       float64   `json:"used"`
	Coverage   uint64    `json:"coverage"`
	ScopeHash  string    `json:"scopeHash"`
	ValidAfter time.Time `json:"validAfter"`
	PriceKey   string    `json:"priceKey"`
	Total      float64   `json:"total"`
	LearnedAt  time.Time `json:"learnedAt"`
}

func validCostCalibration(v CostCalibration) bool {
	return v.Epoch > 0 && len(v.Plan) <= 80 && !strings.ContainsAny(v.Plan, "\r\n\x00") && v.Seconds >= 60 && v.Seconds <= 31*24*3600 && !v.Reset.IsZero() && v.Used >= 0 && v.Used <= 100 && !math.IsNaN(v.Used) && !math.IsInf(v.Used, 0) && v.Total >= 0 && v.Total <= 1e12 && !math.IsNaN(v.Total) && !math.IsInf(v.Total, 0) && validHex(v.ScopeHash, 64) && len(v.PriceKey) <= 100 && strings.HasPrefix(v.PriceKey, "user:")
}

func cloneCalibrations(in map[string]CostCalibration) map[string]CostCalibration {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]CostCalibration, len(in))
	for id, v := range in {
		out[id] = v
	}
	return out
}

func (l *Ledger) CostCalibration(id string) (CostCalibration, bool) {
	if l == nil || id == "" {
		return CostCalibration{}, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.calibrations[l.anonymous("acct-", id)]
	return v, ok && !l.blockedRead && v.Coverage == l.costCoverage && v.PriceKey == priceKey(l.prices) && v.Reset.After(time.Now())
}

// QueueCostCalibration never stalls inference or the accounts page behind disk
// IO. Accepted updates are serialized with request writes and flushed on Close.
// A full queue simply keeps learning in memory; it never manufactures dollars.
func (l *Ledger) QueueCostCalibration(id string, v CostCalibration) bool {
	if l == nil || id == "" || !validCostCalibration(v) {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.blockedRead || v.Coverage != l.costCoverage || v.PriceKey != priceKey(l.prices) {
		return false
	}
	key := l.anonymous("acct-", id)
	if old, ok := l.calibrations[key]; ok && old == v {
		return true
	}
	select {
	case l.queue <- command{kind: "calibration", calibrationID: key, calibration: v}:
		return true
	default:
		return false
	}
}
