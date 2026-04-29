package bootstrapper

import "time"

// ExecutionResult represents the result of bootstrap or unbootstrap process.
type ExecutionResult struct {
	Success     bool          `json:"success"`
	StepCount   int           `json:"step_count"`
	Duration    time.Duration `json:"duration"`
	StepResults []StepResult  `json:"step_results"`
	Error       string        `json:"error,omitempty"`
}

// StepResult represents the result of a single step.
type StepResult struct {
	StepName string        `json:"step_name"`
	Success  bool          `json:"success"`
	Duration time.Duration `json:"duration"`
	Error    string        `json:"error,omitempty"`
}
