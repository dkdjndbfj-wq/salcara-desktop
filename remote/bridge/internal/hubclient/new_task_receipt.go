package hubclient

// Only post-adapter failures use this condition. They cannot authorize a new
// UUID: the original native thread/turn may already exist. No provider error,
// project path or credential is embedded in the reply/log.
type newTaskUncertainError struct{}

func (newTaskUncertainError) Error() string {
	return "任务可能已在电脑上创建，请先核对原任务，不会自动重复创建"
}
func (newTaskUncertainError) ExecutionErrorCode() string { return "command_delivery_uncertain" }
