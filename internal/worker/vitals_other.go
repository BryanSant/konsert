//go:build !darwin && !linux && !windows

package worker

import "github.com/BryanSant/syre/internal/protocol"

// vitalsSampler reports nothing on platforms without a fork-free reader.
type vitalsSampler struct{}

func newVitalsSampler() *vitalsSampler { return &vitalsSampler{} }

func (s *vitalsSampler) sample(*protocol.WorkerMetrics) {}
