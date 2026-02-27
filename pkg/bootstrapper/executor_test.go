package bootstrapper

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

// mockExecutor is a mock implementation of Executor interface for testing bootstrap execution flow.
// It tracks execution state and can simulate successful or failed execution.
type mockExecutor struct {
	name        string
	shouldFail  bool
	isCompleted bool
	executed    bool
}

func (m *mockExecutor) Execute(ctx context.Context) error {
	m.executed = true
	if m.shouldFail {
		return errors.New("mock execution error")
	}
	return nil
}

func (m *mockExecutor) IsCompleted(ctx context.Context) bool {
	return m.isCompleted
}

func (m *mockExecutor) GetName() string {
	return m.name
}

// mockStepExecutor extends mockExecutor with Validate method for testing validation logic.
// Used to test bootstrap steps that implement the StepExecutor interface.
type mockStepExecutor struct {
	mockExecutor
	validateError error
}

func (m *mockStepExecutor) Validate(ctx context.Context) error {
	return m.validateError
}

// TestNewBaseExecutor verifies BaseExecutor constructor initialization.
// Test: Creates BaseExecutor with config and logger
// Expected: Returns non-nil executor with config and logger properly set
func TestNewBaseExecutor(t *testing.T) {
	cfg := &config.Config{}
	logger := logrus.New()

	executor := NewBaseExecutor(cfg, logger)

	if executor == nil {
		t.Fatal("NewBaseExecutor should not return nil")
	}

	if executor.config != cfg {
		t.Error("Config should be set")
	}

	if executor.logger != logger {
		t.Error("Logger should be set")
	}
}

// TestExecuteSteps_Success verifies successful execution of all steps in a bootstrap sequence.
// Test: Executes 3 steps that all succeed
// Expected: All steps execute successfully, result shows Success=true with StepCount=3
func TestExecuteSteps_Success(t *testing.T) {
	cfg := &config.Config{}
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel) // Reduce noise
	executor := NewBaseExecutor(cfg, logger)

	steps := []Executor{
		&mockExecutor{name: "step1", shouldFail: false, isCompleted: false},
		&mockExecutor{name: "step2", shouldFail: false, isCompleted: false},
		&mockExecutor{name: "step3", shouldFail: false, isCompleted: false},
	}

	ctx := context.Background()
	result, err := executor.ExecuteSteps(ctx, steps, "bootstrap")

	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}

	if result == nil {
		t.Fatal("Result should not be nil")
	}

	if !result.Success {
		t.Error("Result should be successful")
	}

	if result.StepCount != 3 {
		t.Errorf("Expected 3 steps, got %d", result.StepCount)
	}

	if len(result.StepResults) != 3 {
		t.Errorf("Expected 3 step results, got %d", len(result.StepResults))
	}

	// Verify all steps were executed
	for i, step := range steps {
		mockStep := step.(*mockExecutor)
		if !mockStep.executed {
			t.Errorf("Step %d should have been executed", i)
		}
	}
}

// TestExecuteSteps_BootstrapFailure verifies bootstrap fails fast on first error.
// Test: Executes steps where step2 fails
// Expected: Execution stops at step2, step3 never executes, returns error with StepCount=2
func TestExecuteSteps_BootstrapFailure(t *testing.T) {
	cfg := &config.Config{}
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	executor := NewBaseExecutor(cfg, logger)

	steps := []Executor{
		&mockExecutor{name: "step1", shouldFail: false, isCompleted: false},
		&mockExecutor{name: "step2", shouldFail: true, isCompleted: false}, // This should fail
		&mockExecutor{name: "step3", shouldFail: false, isCompleted: false},
	}

	ctx := context.Background()
	result, err := executor.ExecuteSteps(ctx, steps, "bootstrap")

	if err == nil {
		t.Error("Expected error for bootstrap failure")
	}

	if result == nil {
		t.Fatal("Result should not be nil")
	}

	if result.Success {
		t.Error("Result should not be successful")
	}

	if result.StepCount != 2 {
		t.Errorf("Expected 2 steps executed before failure, got %d", result.StepCount)
	}

	if result.Error == "" {
		t.Error("Result should have error message")
	}

	// Verify step3 was not executed (bootstrap fails fast)
	step3 := steps[2].(*mockExecutor)
	if step3.executed {
		t.Error("Step 3 should not have been executed after failure")
	}
}

// TestExecuteSteps_UnbootstrapContinuesOnFailure verifies unbootstrap continues after failures.
// Test: Executes unbootstrap steps where step2 fails
// Expected: All 3 steps execute despite failure, result shows Success=false but StepCount=3
func TestExecuteSteps_UnbootstrapContinuesOnFailure(t *testing.T) {
	cfg := &config.Config{}
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	executor := NewBaseExecutor(cfg, logger)

	steps := []Executor{
		&mockExecutor{name: "step1", shouldFail: false, isCompleted: false},
		&mockExecutor{name: "step2", shouldFail: true, isCompleted: false}, // This fails
		&mockExecutor{name: "step3", shouldFail: false, isCompleted: false},
	}

	ctx := context.Background()
	result, err := executor.ExecuteSteps(ctx, steps, "unbootstrap")

	// Unbootstrap doesn't return error, continues on failure
	if err != nil {
		t.Errorf("Unbootstrap should not return error, got: %v", err)
	}

	if result == nil {
		t.Fatal("Result should not be nil")
	}

	if result.Success {
		t.Error("Result should not be fully successful with one failed step")
	}

	if result.StepCount != 3 {
		t.Errorf("Expected all 3 steps to be attempted, got %d", result.StepCount)
	}

	// Verify all steps were executed (unbootstrap continues)
	for i, step := range steps {
		mockStep := step.(*mockExecutor)
		if !mockStep.executed {
			t.Errorf("Step %d should have been executed", i)
		}
	}
}

// TestExecuteSteps_SkipsCompletedSteps verifies already-completed steps are skipped.
// Test: Executes steps where step1 and step3 are marked as completed
// Expected: Only step2 executes, completed steps are skipped but counted as successful
func TestExecuteSteps_SkipsCompletedSteps(t *testing.T) {
	cfg := &config.Config{}
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	executor := NewBaseExecutor(cfg, logger)

	steps := []Executor{
		&mockExecutor{name: "step1", shouldFail: false, isCompleted: true}, // Already completed
		&mockExecutor{name: "step2", shouldFail: false, isCompleted: false},
		&mockExecutor{name: "step3", shouldFail: false, isCompleted: true}, // Already completed
	}

	ctx := context.Background()
	result, err := executor.ExecuteSteps(ctx, steps, "bootstrap")

	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}

	if !result.Success {
		t.Error("Result should be successful")
	}

	// Verify completed steps were not executed
	step1 := steps[0].(*mockExecutor)
	if step1.executed {
		t.Error("Completed step 1 should not have been executed")
	}

	step2 := steps[1].(*mockExecutor)
	if !step2.executed {
		t.Error("Incomplete step 2 should have been executed")
	}

	step3 := steps[2].(*mockExecutor)
	if step3.executed {
		t.Error("Completed step 3 should not have been executed")
	}
}

// TestExecuteSteps_ValidationFailure verifies bootstrap fails when validation fails.
// Test: Executes a step that fails validation (before execution)
// Expected: Step never executes, returns error indicating validation failure
func TestExecuteSteps_ValidationFailure(t *testing.T) {
	cfg := &config.Config{}
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	executor := NewBaseExecutor(cfg, logger)

	steps := []Executor{
		&mockStepExecutor{
			mockExecutor:  mockExecutor{name: "step1", shouldFail: false, isCompleted: false},
			validateError: errors.New("validation failed"),
		},
	}

	ctx := context.Background()
	result, err := executor.ExecuteSteps(ctx, steps, "bootstrap")

	if err == nil {
		t.Error("Expected error for validation failure")
	}

	if result == nil {
		t.Fatal("Result should not be nil")
	}

	if result.Success {
		t.Error("Result should not be successful")
	}

	// Verify step was not executed due to validation failure
	step1 := steps[0].(*mockStepExecutor)
	if step1.executed {
		t.Error("Step should not have been executed after validation failure")
	}
}

// TestCountSuccessfulSteps verifies counting of successful steps from results.
// Test: Counts successful steps in a mixed success/failure result set
// Expected: Returns count of 3 successful steps out of 4 total
func TestCountSuccessfulSteps(t *testing.T) {
	cfg := &config.Config{}
	logger := logrus.New()
	executor := NewBaseExecutor(cfg, logger)

	stepResults := []StepResult{
		{StepName: "step1", Success: true},
		{StepName: "step2", Success: false},
		{StepName: "step3", Success: true},
		{StepName: "step4", Success: true},
	}

	count := executor.countSuccessfulSteps(stepResults)

	if count != 3 {
		t.Errorf("Expected 3 successful steps, got %d", count)
	}
}

// TestCreateStepResult verifies StepResult creation with timing and status.
// Test: Creates step results for both successful and failed scenarios
// Expected: StepResult contains correct name, success status, duration, and error message
func TestCreateStepResult(t *testing.T) {
	cfg := &config.Config{}
	logger := logrus.New()
	executor := NewBaseExecutor(cfg, logger)

	startTime := time.Now()
	time.Sleep(10 * time.Millisecond)

	result := executor.createStepResult("test-step", startTime, true, "")

	if result.StepName != "test-step" {
		t.Errorf("Expected step name 'test-step', got '%s'", result.StepName)
	}

	if !result.Success {
		t.Error("Expected success to be true")
	}

	if result.Duration == 0 {
		t.Error("Duration should be greater than 0")
	}

	if result.Error != "" {
		t.Error("Error should be empty for successful result")
	}

	// Test with error
	resultWithError := executor.createStepResult("test-step", startTime, false, "test error")

	if resultWithError.Success {
		t.Error("Expected success to be false")
	}

	if resultWithError.Error != "test error" {
		t.Errorf("Expected error 'test error', got '%s'", resultWithError.Error)
	}
}

// TestExecutionResult verifies ExecutionResult structure and field population.
// Test: Creates an ExecutionResult with multiple step results
// Expected: All fields (Success, StepCount, Duration, StepResults) are properly populated
func TestExecutionResult(t *testing.T) {
	result := &ExecutionResult{
		Success:   true,
		StepCount: 3,
		Duration:  time.Second,
		StepResults: []StepResult{
			{StepName: "step1", Success: true, Duration: time.Millisecond * 100},
			{StepName: "step2", Success: true, Duration: time.Millisecond * 200},
			{StepName: "step3", Success: true, Duration: time.Millisecond * 300},
		},
	}

	if !result.Success {
		t.Error("Result should be successful")
	}

	if result.StepCount != 3 {
		t.Errorf("Expected 3 steps, got %d", result.StepCount)
	}

	if len(result.StepResults) != 3 {
		t.Errorf("Expected 3 step results, got %d", len(result.StepResults))
	}
}
