package protocol

import "time"

// MsgOperatorCommand reports a command the user ran in a Konsert terminal.
const MsgOperatorCommand = "worker.operator_command"

// OperatorCommandPayload is one finished (or started) command from a Konsert terminal.
type OperatorCommandPayload struct {
	TaskID    string     `json:"task_id"`    // the terminal task's UUID id
	CommandID string     `json:"command_id"` // UUIDv7, stable from start to end
	Command   string     `json:"command"`    // redacted summary
	Cwd       string     `json:"cwd,omitempty"`
	ExitCode  *int       `json:"exit_code,omitzero"` // nil while running
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitzero"`
}
