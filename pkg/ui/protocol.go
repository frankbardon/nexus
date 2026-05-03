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
	TypeOutput          = "output"
	TypeStreamChunk     = "stream_chunk"
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
)
