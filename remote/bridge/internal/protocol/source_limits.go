package protocol

// These budgets match Hub source_limits.go. Legal 4096-byte URL paths can
// expand sixfold in JSON, and a base may appear in both API and proxy scopes.
const (
	MaxStationPolicyJSONBytes = 2 << 20
	MaxSourcePolicyWireBytes  = 4 << 20
	MaxSourceSnapshotBytes    = 64 << 10
	MaxSourcePayloadBytes     = (MaxSourceSnapshotBytes*4 + 2) / 3
	MaxSignedSourceGrantBytes = 96 << 10
)
