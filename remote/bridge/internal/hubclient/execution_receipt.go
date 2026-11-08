package hubclient

// An authorization gap after Dispatch cannot prove that the operation did not
// run, even when an adapter returned an error after a partial write. Return no
// private result, but keep the phone's original durable submission pending.
type commandDeliveryUncertainError struct{}

func (commandDeliveryUncertainError) Error() string {
	return "指令可能已在电脑执行，连接变化导致结果未确认，请核对原会话；不会自动重复发送"
}
func (commandDeliveryUncertainError) ExecutionErrorCode() string { return "command_delivery_uncertain" }
