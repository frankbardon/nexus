package ui

import "context"

// UIAdapter defines the contract between the engine and a UI transport.
// IO plugins implement this interface; the engine consumes it.
type UIAdapter interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error

	// Outbound (engine -> user)
	SendOutput(msg OutputMessage) error
	SendStreamChunk(msg StreamChunkMessage) error
	SendStreamEnd(msg StreamEndMessage) error
	SendStatus(msg StatusMessage) error
	RequestApproval(msg ApprovalRequestMessage) (ApprovalResponseMessage, error)

	// Inbound (user -> engine) — delivered via callback
	OnInput(handler func(InputMessage))
	OnApprovalResponse(handler func(ApprovalResponseMessage))

	// Session
	Sessions() []SessionInfo
}
