package types

type ExecRequest struct {
	Language string            `json:"language" binding:"required,oneof=python nodejs bash"`
	Code     string            `json:"code" binding:"required"`
	Stdin    string            `json:"stdin,omitempty" binding:"max=1048576"`
	Timeout  int               `json:"timeout,omitempty" binding:"min=0,max=3600"`
	Env      map[string]string `json:"env,omitempty"`
}

type ExecResponse struct {
	ExitCode int     `json:"exit_code"`
	Stdout   string  `json:"stdout"`
	Stderr   string  `json:"stderr"`
	Duration float64 `json:"duration"` // seconds
}

// ExecuteRequest is for one-shot execution (no pre-created sandbox).
type ExecuteRequest struct {
	Language     string            `json:"language" binding:"required,oneof=python nodejs bash"`
	Code         string            `json:"code" binding:"required"`
	Stdin        string            `json:"stdin,omitempty" binding:"max=1048576"`
	Timeout      int               `json:"timeout,omitempty" binding:"min=0,max=3600"`
	Env          map[string]string `json:"env,omitempty"`
	Resources    *ResourceLimits   `json:"resources,omitempty"`
	Network      *NetworkConfig    `json:"network,omitempty"`
	Dependencies []DependencySpec  `json:"dependencies,omitempty"`
}

// SSEEvent represents a Server-Sent Event for streamed execution.
type SSEEvent struct {
	Event string `json:"event"` // "stdout", "stderr", "status", "done", "error"
	Data  any    `json:"data"`
}

type SSEStdoutData struct {
	Content string `json:"content"`
}

type SSEStderrData struct {
	Content string `json:"content"`
}

type SSEStatusData struct {
	State   string  `json:"state"`
	Elapsed float64 `json:"elapsed"`
}

type SSEDoneData struct {
	ExitCode int     `json:"exit_code"`
	Elapsed  float64 `json:"elapsed"`
}

type SSEErrorData struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}
