package install

import (
	"fmt"
	"reflect"

	"github.com/BryanSant/konsert/internal/config"
	"gopkg.in/yaml.v3"
)

const configHeader = "# konsertd worker config written by `konsertd install`.\n" +
	"# It can hold the fleet token: keep it readable only by you.\n"

// MarshalWorkerConfig renders cfg as YAML with only the fields that were set. Unset
// fields are left out (not written as zero values) so `konsertd run` still applies its
// defaults to them, e.g. the hostname as the worker name.
func MarshalWorkerConfig(cfg config.WorkerConfig) ([]byte, error) {
	full, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(full, &m); err != nil {
		return nil, err
	}
	for k, v := range m {
		if v == nil || reflect.ValueOf(v).IsZero() {
			delete(m, k)
			continue
		}
		if rv := reflect.ValueOf(v); (rv.Kind() == reflect.Map || rv.Kind() == reflect.Slice) && rv.Len() == 0 {
			delete(m, k)
		}
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(m) == 0 {
		out = nil
	}
	return append([]byte(configHeader), out...), nil
}

// ParseWorkerConfig parses a persisted config without applying defaults.
func ParseWorkerConfig(data []byte) (config.WorkerConfig, error) {
	var cfg config.WorkerConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse worker config: %w", err)
	}
	return cfg, nil
}
