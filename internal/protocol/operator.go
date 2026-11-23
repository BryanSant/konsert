package protocol

import "time"

// MsgWorkerOperatorCommand (worker → hub) reports a command the user ran in a Konsert
// terminal (shell integration, OSC 133 marks). konsertd keeps the full command line
// locally with a short TTL; the hub only ever receives the redacted summary. A command may
// be reported when it starts (EndedAt and ExitCode nil) and again when it ends (same
// CommandID), or only once when it ends.
const MsgWorkerOperatorCommand = "worker.operator_command"

// OperatorCommandPayload is one command run by the user in a Konsert terminal task.
type OperatorCommandPayload struct {
	// TaskID is the terminal task's UUID id.
	TaskID string `json:"task_id"`
	// Command is a redacted summary of the command line, e.g. "systemctl stop nginx";
	// arguments that look like secrets are replaced by "…".
	Command string `json:"command"`
	// ExitCode is the command's exit status once it has ended.
	ExitCode  *int       `json:"exit_code,omitzero"`
	Cwd       string     `json:"cwd,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitzero"`
	// CommandID is a UUIDv7 identifying this command run (start and end reports share it).
	CommandID string `json:"command_id"`
}
