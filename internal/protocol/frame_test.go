package protocol

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/ids"
)

func TestEncodeDecodeFrame(t *testing.T) {
	taskID := "TASK-7QK3M"
	payload := []byte("\x1b[32mhello world\x1b[0m\r\n")

	for _, ft := range []byte{FramePtySnapshot, FramePtyData, FramePtyStdin} {
		encoded, err := EncodeFrame(ft, taskID, payload)
		if err != nil {
			t.Fatalf("EncodeFrame(0x%02x) error: %v", ft, err)
		}
		if len(encoded) != FrameHeaderSize+len(payload) {
			t.Fatalf("unexpected length: got %d want %d", len(encoded), FrameHeaderSize+len(payload))
		}
		gotType, gotID, gotData, err := DecodeFrame(encoded)
		if err != nil {
			t.Fatalf("DecodeFrame error: %v", err)
		}
		if gotType != ft || gotID != taskID || !bytes.Equal(gotData, payload) {
			t.Fatalf("mismatch: type=%x id=%q data=%q", gotType, gotID, gotData)
		}
	}
}

func TestEncodeDecodeShortTaskIDAndEmptyPayload(t *testing.T) {
	encoded, err := EncodeFrame(FramePtyData, "task-123", nil)
	if err != nil {
		t.Fatalf("EncodeFrame error: %v", err)
	}
	ft, id, data, err := DecodeFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeFrame error: %v", err)
	}
	if ft != FramePtyData || id != "task-123" || len(data) != 0 {
		t.Fatalf("unexpected decoded values: ft=%x id=%q len=%d", ft, id, len(data))
	}
}

func TestFrameErrors(t *testing.T) {
	if _, err := EncodeFrame(0x99, "task-1", nil); !errors.Is(err, ErrInvalidFrameType) {
		t.Fatalf("expected ErrInvalidFrameType, got %v", err)
	}
	if _, err := EncodeFrame(FramePtyData, "", nil); !errors.Is(err, ErrInvalidTaskID) {
		t.Fatalf("expected ErrInvalidTaskID for empty id, got %v", err)
	}
	if _, err := EncodeFrame(FramePtyData, strings.Repeat("a", FrameTaskIDSize+1), nil); !errors.Is(err, ErrInvalidTaskID) {
		t.Fatalf("expected ErrInvalidTaskID for an over-long id, got %v", err)
	}
	if _, _, _, err := DecodeFrame(make([]byte, FrameHeaderSize-1)); !errors.Is(err, ErrFrameTooShort) {
		t.Fatalf("expected ErrFrameTooShort, got %v", err)
	}
	badType := make([]byte, FrameHeaderSize)
	badType[0] = 0x04
	if _, _, _, err := DecodeFrame(badType); !errors.Is(err, ErrInvalidFrameType) {
		t.Fatalf("expected ErrInvalidFrameType, got %v", err)
	}
	emptyID := make([]byte, FrameHeaderSize)
	emptyID[0] = FramePtyData
	if _, _, _, err := DecodeFrame(emptyID); !errors.Is(err, ErrInvalidTaskID) {
		t.Fatalf("expected ErrInvalidTaskID for zeroed id, got %v", err)
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	raw, err := NewEnvelope(MsgTaskCancel, "req-1", TaskCancelPayload{TaskID: "abc"})
	if err != nil {
		t.Fatalf("NewEnvelope error: %v", err)
	}
	env, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("DecodeEnvelope error: %v", err)
	}
	if env.Type != MsgTaskCancel || env.ID != "req-1" {
		t.Fatalf("unexpected envelope header: %+v", env)
	}
	p, err := DecodePayload[TaskCancelPayload](env)
	if err != nil {
		t.Fatalf("DecodePayload error: %v", err)
	}
	if p.TaskID != "abc" {
		t.Fatalf("unexpected payload: %+v", p)
	}
}

func TestFrameFitsLongestMintedTaskID(t *testing.T) {
	longest := string(ids.Task) + "-" + strings.Repeat("Z", 12)
	if err := ValidateTaskID(longest); err != nil {
		t.Fatalf("ValidateTaskID(%q) = %v", longest, err)
	}
}
