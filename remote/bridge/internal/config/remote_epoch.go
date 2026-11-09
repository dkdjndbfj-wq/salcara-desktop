package config

// fenceRemoteConnectionEpoch runs with Store.mu held before an update is
// published. Restoring the old epoch first prevents update callbacks from
// reviving a retired connection operation, including after A -> B -> A.
// Persistence rollback restores the old Config and its epoch together.
func fenceRemoteConnectionEpoch(old Config, next *Config) {
	next.RemoteConnectionEpoch = old.RemoteConnectionEpoch
	if next.RemoteConnectionEpoch == "" ||
		RemoteIdentity(old) != RemoteIdentity(*next) ||
		old.EffectiveRemoteMode() != next.EffectiveRemoteMode() ||
		old.GatewayRuntimeKey != next.GatewayRuntimeKey {
		next.RemoteConnectionEpoch = RandomToken(32)
	}
}
