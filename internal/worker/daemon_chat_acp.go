package worker

import "github.com/BryanSant/konsert/internal/worker/chat"

// registerACPEngines adds the ACP chat engine for grok, agy, copilot and cursor once it
// is available (replaced when the ACP engine lands).
var registerACPEngines = func(engines map[string]chat.Engine) {}
