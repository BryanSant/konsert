package protocol

// Human control (PRD §5.8a): suspend and resume a session's process tree.

const (
	// MsgTaskSuspend (hub → worker) freezes a task's process tree (SIGSTOP of its process
	// group on Unix; every thread of its processes on Windows). The worker answers with a
	// task.status of TaskStatusSuspended, or a task.event of kind "error" when it can't.
	MsgTaskSuspend = "task.suspend"
	// MsgTaskResume (hub → worker) thaws a suspended task; the worker reports its status again.
	MsgTaskResume = "task.resume"

	// MsgAgentChatSuspend (hub → worker) freezes a chat's engine process tree. The worker
	// answers with an EventStatus of ChatStatusSuspended (or an EventError when it can't).
	MsgAgentChatSuspend = "agent.chat.suspend"
	// MsgAgentChatResume (hub → worker) thaws it; the worker reports the chat's status again.
	MsgAgentChatResume = "agent.chat.resume"
)

// TaskStatusSuspended is a task whose process tree is frozen by the user (or Pause everything).
const TaskStatusSuspended = "suspended"

// ChatStatusSuspended is an agent chat whose engine process tree is frozen.
const ChatStatusSuspended = "suspended"

// TaskSuspendPayload names the task for task.suspend and task.resume.
type TaskSuspendPayload struct {
	TaskID string `json:"task_id"`
}

// AgentChatSuspend names the chat for agent.chat.suspend and agent.chat.resume.
type AgentChatSuspend struct {
	ChatID string `json:"chat_id"`
}
