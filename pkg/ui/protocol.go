package ui

// Message type constants for the WebSocket protocol.
const (
	// Inbound (client -> server)
	TypeInput            = "input"
	TypeApprovalResponse = "approval_response"
	TypePing             = "ping"
	TypeFileList         = "file_list"
	TypeFileDownload     = "file_download"
	TypeCancelRequest    = "cancel_request"
	TypeResumeRequest    = "resume_request"
	TypeHITLResponse     = "hitl_response"

	// Outbound (server -> client)
	TypeOutput      = "output"
	TypeStreamChunk = "stream_chunk"
	// TypeStreamHold announces that an output gate has suspended the stream
	// pending review, and (with resumed set) that it has released it again. A
	// stream that simply goes quiet is indistinguishable from a hung turn, so
	// a client renders a reviewing indicator rather than a stalled cursor.
	TypeStreamHold = "stream_hold"
	// TypeStreamRetract tells a client that an output gate blocked the stream
	// mid-flight and the text rendered for that turn is disowned. A client
	// that owns its render buffer should erase it; the replacement arrives as
	// an ordinary output message.
	TypeStreamRetract   = "stream_retract"
	TypeStreamEnd       = "stream_end"
	TypeStatus          = "status"
	TypeApprovalRequest = "approval_request"
	TypePong            = "pong"
	TypeFileListResult  = "file_list_result"
	TypeFileContent     = "file_content"
	TypeFileChanged     = "file_changed"
	TypeSessionReset    = "session_reset"
	TypeCancelComplete  = "cancel_complete"
	TypeHITLRequest     = "hitl_request"
	TypeCodeExecStdout  = "code_exec_stdout"
	TypeWorkerStatus    = "worker_status"
	TypeWorkflowStatus  = "workflow_status"
)
