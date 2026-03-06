package bootstrapper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/sirupsen/logrus"
)

// executor is a common base interface for all executors
// Every installer and uninstaller implements this interface
type Executor interface {
	// Execute performs the step's main operation
	Execute(ctx context.Context) error

	// IsCompleted checks if the step has already been completed
	IsCompleted(ctx context.Context) bool

	// GetName returns the step name
	GetName() string
}

// stepExecutor interface defines the contract for bootstrap step implementations
// only installers need to implement this interface
type StepExecutor interface {
	Executor

	// Validate validates preconditions before execution (optional)
	Validate(ctx context.Context) error
}

// ExecutionResult represents the result of bootstrap or unbootstrap process
type ExecutionResult struct {
	Success     bool          `json:"success"`
	StepCount   int           `json:"step_count"`
	Duration    time.Duration `json:"duration"`
	StepResults []StepResult  `json:"step_results"`
	Error       string        `json:"error,omitempty"`
}

// StepResult represents the result of a single step
type StepResult struct {
	StepName string        `json:"step_name"`
	Success  bool          `json:"success"`
	Duration time.Duration `json:"duration"`
	Error    string        `json:"error,omitempty"`
}

// BaseExecutor provides common functionality for bootstrap and unbootstrap operations
type BaseExecutor struct {
	config *config.Config
	logger *logrus.Logger
}

// NewBaseExecutor creates a new base executor
func NewBaseExecutor(cfg *config.Config, logger *logrus.Logger) *BaseExecutor {
	return &BaseExecutor{
		config: cfg,
		logger: logger,
	}
}

// ExecuteSteps executes a list of steps and returns results
func (be *BaseExecutor) ExecuteSteps(ctx context.Context, steps []Executor, stepType string) (*ExecutionResult, error) {
	be.logger.Infof("Starting AKS node %s", stepType)

	startTime := time.Now()
	result := &ExecutionResult{
		StepResults: make([]StepResult, 0),
	}

	// Execute each step
	for _, step := range steps {
		stepResult := be.executeStep(ctx, step, stepType)
		result.StepResults = append(result.StepResults, stepResult)

		if !stepResult.Success {
			if stepType == "bootstrap" {
				// Bootstrap fails fast on first error
				result.Success = false
				result.Error = stepResult.Error
				result.Duration = time.Since(startTime)
				result.StepCount = len(result.StepResults)

				be.logger.Errorf("Bootstrap failed at step %s: %s (completedSteps: %d, totalSteps: %d)",
					stepResult.StepName, stepResult.Error, len(result.StepResults), len(steps))

				return result, fmt.Errorf("bootstrap failed at step %s: %w", stepResult.StepName, errors.New(stepResult.Error))
			}
			// Unbootstrap continues even if some steps fail for best effort cleanup
			be.logger.Warnf("Cleanup step %s failed: %s (continuing with remaining steps)",
				stepResult.StepName, stepResult.Error)
		}
	}

	// Calculate final result
	successfulSteps := be.countSuccessfulSteps(result.StepResults)
	result.Success = successfulSteps == len(steps)
	result.Duration = time.Since(startTime)
	result.StepCount = len(result.StepResults)

	if result.Success {
		be.logger.Infof("AKS node %s completed successfully (duration: %v, stepCount: %d)",
			stepType, result.Duration, result.StepCount)
	} else if stepType == "unbootstrap" {
		be.logger.Warnf("AKS node %s completed with some failures (duration: %v, successfulSteps: %d, totalSteps: %d)",
			stepType, result.Duration, successfulSteps, len(steps))
		result.Error = fmt.Sprintf("completed with %d failed steps out of %d total steps",
			len(steps)-successfulSteps, len(steps))
	}

	return result, nil
}

// executeStep executes a single step and returns the result
func (be *BaseExecutor) executeStep(ctx context.Context, step Executor, stepType string) StepResult {
	stepName := step.GetName()
	startTime := time.Now()

	be.logger.Infof("Executing %s step %s", stepType, stepName)

	// Check if step is already completed
	if step.IsCompleted(ctx) {
		be.logger.Infof("%s step: %s already completed", stepType, stepName)
		return be.createStepResult(stepName, startTime, true, "")
	}

	var err error
	if bootstrapStep, ok := step.(StepExecutor); ok && stepType == "bootstrap" {
		// Validate preconditions for bootstrap steps
		if validationErr := bootstrapStep.Validate(ctx); validationErr != nil {
			be.logger.Errorf("%s step %s validation failed with error: %s", stepType, stepName, validationErr)
			return be.createStepResult(stepName, startTime, false, fmt.Sprintf("validation failed: %v", validationErr))
		}
	}

	// Execute the step
	err = step.Execute(ctx)
	if err != nil {
		be.logger.Errorf("%s step: %s failed with error: %s with duration %s", stepType, stepName, err, time.Since(startTime))
		return be.createStepResult(stepName, startTime, false, err.Error())
	}

	be.logger.Infof("%s step: %s completed successfully with duration %s", stepType, stepName, time.Since(startTime))
	return be.createStepResult(stepName, startTime, true, "")
}

// createStepResult creates a StepResult with consistent formatting
func (be *BaseExecutor) createStepResult(stepName string, startTime time.Time, success bool, errorMsg string) StepResult {
	return StepResult{
		StepName: stepName,
		Success:  success,
		Duration: time.Since(startTime),
		Error:    errorMsg,
	}
}

// countSuccessfulSteps counts the number of successful steps
func (be *BaseExecutor) countSuccessfulSteps(stepResults []StepResult) int {
	count := 0
	for _, result := range stepResults {
		if result.Success {
			count++
		}
	}
	return count
}
