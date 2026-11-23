package protocol

import (
	"bytes"
	"errors"
	"fmt"
)

const (
	FramePtySnapshot byte = 0x01
	FramePtyData     byte = 0x02
	FramePtyStdin    byte = 0x03

	// FrameTaskIDSize fits the longest task ID internal/ids can mint ("t-" + 12) with room.
	FrameTaskIDSize = 16
	FrameHeaderSize = 1 + FrameTaskIDSize
)

var (
	ErrInvalidFrameType = errors.New("invalid binary PTY frame type")
	ErrFrameTooShort    = errors.New("binary PTY frame shorter than header")
	ErrInvalidTaskID    = errors.New("invalid task ID for binary PTY frame")
)

// ValidateTaskID checks that id is usable as a task ID everywhere a worker uses one: in
// binary PTY frame headers (at most FrameTaskIDSize bytes) and as a single path component
// for git worktrees. Valid IDs are 1..FrameTaskIDSize characters of [A-Za-z0-9-]; the hub
// mints ka-style IDs such as t-7qk3m (internal/ids).
func ValidateTaskID(id string) error {
	if len(id) == 0 || len(id) > FrameTaskIDSize {
		return fmt.Errorf("%w: length %d (must be 1..%d)", ErrInvalidTaskID, len(id), FrameTaskIDSize)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return fmt.Errorf("%w: character %q at %d (allowed: A-Z a-z 0-9 -)", ErrInvalidTaskID, c, i)
	}
	return nil
}

func isValidFrameType(t byte) bool {
	return t == FramePtySnapshot || t == FramePtyData || t == FramePtyStdin
}

// EncodeFrame builds a binary PTY frame:
// [FrameType:1B][TaskID:16B UTF-8 zero-padded][Raw PTY Bytes:NB]
func EncodeFrame(frameType byte, taskID string, data []byte) ([]byte, error) {
	if !isValidFrameType(frameType) {
		return nil, fmt.Errorf("%w: 0x%02x", ErrInvalidFrameType, frameType)
	}
	if len(taskID) == 0 || len(taskID) > FrameTaskIDSize {
		return nil, fmt.Errorf("%w: length %d (must be 1..%d)", ErrInvalidTaskID, len(taskID), FrameTaskIDSize)
	}
	buf := make([]byte, FrameHeaderSize+len(data))
	buf[0] = frameType
	copy(buf[1:FrameHeaderSize], taskID)
	copy(buf[FrameHeaderSize:], data)
	return buf, nil
}

// DecodeFrame parses a binary PTY frame into its type, task ID, and payload bytes.
func DecodeFrame(raw []byte) (frameType byte, taskID string, data []byte, err error) {
	if len(raw) < FrameHeaderSize {
		return 0, "", nil, fmt.Errorf("%w: got %d bytes", ErrFrameTooShort, len(raw))
	}
	frameType = raw[0]
	if !isValidFrameType(frameType) {
		return 0, "", nil, fmt.Errorf("%w: 0x%02x", ErrInvalidFrameType, frameType)
	}
	idBytes := bytes.TrimRight(raw[1:FrameHeaderSize], "\x00")
	if len(idBytes) == 0 {
		return 0, "", nil, fmt.Errorf("%w: empty task ID", ErrInvalidTaskID)
	}
	taskID = string(idBytes)
	data = raw[FrameHeaderSize:]
	return frameType, taskID, data, nil
}
