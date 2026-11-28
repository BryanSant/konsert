package store

import "time"

// PushSubscription is one browser's Web Push subscription (SYR-Z15J6).
type PushSubscription struct {
	Endpoint   string     `json:"endpoint"`
	DeviceID   string     `json:"device_id"`
	P256DH     string     `json:"p256dh"`
	Auth       string     `json:"auth"`
	PrefsJSON  string     `json:"prefs_json"`
	UserAgent  string     `json:"user_agent,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSentAt *time.Time `json:"last_sent_at,omitzero"`
	LastError  string     `json:"last_error,omitempty"`
}
