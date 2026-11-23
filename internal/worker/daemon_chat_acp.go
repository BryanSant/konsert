package worker

import "github.com/BryanSant/konsert/internal/worker/chat"

// registerACPEngines adds the ACP chat engine for grok, agy, copilot and cursor.
var registerACPEngines = func(engines map[string]chat.Engine) {
	for _, agent := range chat.ACPAgents() {
		if e, err := chat.NewACPEngine(agent); err == nil {
			engines[agent] = e
		}
	}
}
