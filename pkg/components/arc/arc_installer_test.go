package arc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/sirupsen/logrus"
)

// mockRoleAssignmentsClient is a mock implementation for testing
type mockRoleAssignmentsClient struct {
	createFunc func(ctx context.Context, scope string, roleAssignmentName string, parameters armauthorization.RoleAssignmentCreateParameters, options *armauthorization.RoleAssignmentsClientCreateOptions) (armauthorization.RoleAssignmentsClientCreateResponse, error)
	callCount  int
}

func (m *mockRoleAssignmentsClient) Create(ctx context.Context, scope string, roleAssignmentName string, parameters armauthorization.RoleAssignmentCreateParameters, options *armauthorization.RoleAssignmentsClientCreateOptions) (armauthorization.RoleAssignmentsClientCreateResponse, error) {
	m.callCount++
	return m.createFunc(ctx, scope, roleAssignmentName, parameters, options)
}

func (m *mockRoleAssignmentsClient) Delete(ctx context.Context, scope string, roleAssignmentName string, options *armauthorization.RoleAssignmentsClientDeleteOptions) (armauthorization.RoleAssignmentsClientDeleteResponse, error) {
	// Not used in these tests
	return armauthorization.RoleAssignmentsClientDeleteResponse{}, nil
}

func (m *mockRoleAssignmentsClient) NewListForScopePager(scope string, options *armauthorization.RoleAssignmentsClientListForScopeOptions) *runtime.Pager[armauthorization.RoleAssignmentsClientListForScopeResponse] {
	// Not used in these tests
	return nil
}

// mockResponseError creates a mock Azure error response
type mockResponseError struct {
	code    string
	message string
}

func (m *mockResponseError) Error() string {
	return fmt.Sprintf("RESPONSE 400: 400 Bad Request\nERROR CODE: %s\n%s", m.code, m.message)
}

func newMockResponseError(code, message string) error {
	return &mockResponseError{code: code, message: message}
}

func TestAssignRole_Success(t *testing.T) {
	// Setup
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel) // Reduce noise in tests

	cfg := &config.Config{
		Azure: config.AzureConfig{
			SubscriptionID: "test-sub-id",
		},
	}

	mockClient := &mockRoleAssignmentsClient{
		createFunc: func(ctx context.Context, scope string, roleAssignmentName string, parameters armauthorization.RoleAssignmentCreateParameters, options *armauthorization.RoleAssignmentsClientCreateOptions) (armauthorization.RoleAssignmentsClientCreateResponse, error) {
			// Verify PrincipalType is set correctly
			if parameters.Properties == nil || parameters.Properties.PrincipalType == nil {
				t.Error("PrincipalType should be set")
			} else if *parameters.Properties.PrincipalType != armauthorization.PrincipalTypeServicePrincipal {
				t.Errorf("Expected PrincipalType to be ServicePrincipal, got %s", *parameters.Properties.PrincipalType)
			}
			return armauthorization.RoleAssignmentsClientCreateResponse{}, nil
		},
	}

	installer := &Installer{
		base: &base{
			config:                cfg,
			logger:                logger,
			roleAssignmentsClient: mockClient,
		},
	}

	// Execute
	ctx := context.Background()
	err := installer.assignRole(ctx, "test-principal-id", "test-role-id", "/test/scope", "TestRole")

	// Verify
	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}
	if mockClient.callCount != 1 {
		t.Errorf("Expected 1 API call, got %d", mockClient.callCount)
	}
}

func TestAssignRole_PrincipalNotFound_RetriesAndSucceeds(t *testing.T) {
	// Setup
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	cfg := &config.Config{
		Azure: config.AzureConfig{
			SubscriptionID: "test-sub-id",
		},
	}

	attemptCount := 0
	mockClient := &mockRoleAssignmentsClient{
		createFunc: func(ctx context.Context, scope string, roleAssignmentName string, parameters armauthorization.RoleAssignmentCreateParameters, options *armauthorization.RoleAssignmentsClientCreateOptions) (armauthorization.RoleAssignmentsClientCreateResponse, error) {
			attemptCount++
			// Fail first 2 attempts, succeed on 3rd
			if attemptCount < 3 {
				return armauthorization.RoleAssignmentsClientCreateResponse{}, newMockResponseError("PrincipalNotFound", "Principal does not exist")
			}
			return armauthorization.RoleAssignmentsClientCreateResponse{}, nil
		},
	}

	installer := &Installer{
		base: &base{
			config:                cfg,
			logger:                logger,
			roleAssignmentsClient: mockClient,
		},
	}

	// Execute
	ctx := context.Background()
	startTime := time.Now()
	err := installer.assignRole(ctx, "test-principal-id", "test-role-id", "/test/scope", "TestRole")
	duration := time.Since(startTime)

	// Verify
	if err != nil {
		t.Errorf("Expected no error after retries, got: %v", err)
	}
	if mockClient.callCount != 3 {
		t.Errorf("Expected 3 API calls (2 failures + 1 success), got %d", mockClient.callCount)
	}
	// Should have delays: 5s + 10s = 15s (with some tolerance)
	if duration < 14*time.Second {
		t.Errorf("Expected at least 15s of retries, got %v", duration)
	}
}

func TestAssignRole_PrincipalNotFound_ExhaustsRetries(t *testing.T) {
	// Setup
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	cfg := &config.Config{
		Azure: config.AzureConfig{
			SubscriptionID: "test-sub-id",
		},
	}

	mockClient := &mockRoleAssignmentsClient{
		createFunc: func(ctx context.Context, scope string, roleAssignmentName string, parameters armauthorization.RoleAssignmentCreateParameters, options *armauthorization.RoleAssignmentsClientCreateOptions) (armauthorization.RoleAssignmentsClientCreateResponse, error) {
			// Always fail with PrincipalNotFound
			return armauthorization.RoleAssignmentsClientCreateResponse{}, newMockResponseError("PrincipalNotFound", "Principal does not exist")
		},
	}

	installer := &Installer{
		base: &base{
			config:                cfg,
			logger:                logger,
			roleAssignmentsClient: mockClient,
		},
	}

	// Execute
	ctx := context.Background()
	err := installer.assignRole(ctx, "test-principal-id", "test-role-id", "/test/scope", "TestRole")

	// Verify
	if err == nil {
		t.Error("Expected error after exhausting retries, got nil")
	}
	if !strings.Contains(err.Error(), "failed to assign role after") {
		t.Errorf("Expected 'failed to assign role after' error message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Azure AD replication delay") {
		t.Errorf("Expected 'Azure AD replication delay' in error message, got: %v", err)
	}
	if mockClient.callCount != 5 {
		t.Errorf("Expected 5 API calls (max retries), got %d", mockClient.callCount)
	}
}

func TestAssignRole_ForbiddenError_NoRetry(t *testing.T) {
	// Setup
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	cfg := &config.Config{
		Azure: config.AzureConfig{
			SubscriptionID: "test-sub-id",
		},
	}

	mockClient := &mockRoleAssignmentsClient{
		createFunc: func(ctx context.Context, scope string, roleAssignmentName string, parameters armauthorization.RoleAssignmentCreateParameters, options *armauthorization.RoleAssignmentsClientCreateOptions) (armauthorization.RoleAssignmentsClientCreateResponse, error) {
			return armauthorization.RoleAssignmentsClientCreateResponse{}, errors.New("403 Forbidden: insufficient permissions")
		},
	}

	installer := &Installer{
		base: &base{
			config:                cfg,
			logger:                logger,
			roleAssignmentsClient: mockClient,
		},
	}

	// Execute
	ctx := context.Background()
	err := installer.assignRole(ctx, "test-principal-id", "test-role-id", "/test/scope", "TestRole")

	// Verify
	if err == nil {
		t.Error("Expected error, got nil")
	}
	if !strings.Contains(err.Error(), "insufficient permissions") {
		t.Errorf("Expected 'insufficient permissions' error message, got: %v", err)
	}
	if mockClient.callCount != 1 {
		t.Errorf("Expected 1 API call (no retry on 403), got %d", mockClient.callCount)
	}
}

func TestAssignRole_RoleAssignmentExists_ReturnsSuccess(t *testing.T) {
	// Setup
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	cfg := &config.Config{
		Azure: config.AzureConfig{
			SubscriptionID: "test-sub-id",
		},
	}

	mockClient := &mockRoleAssignmentsClient{
		createFunc: func(ctx context.Context, scope string, roleAssignmentName string, parameters armauthorization.RoleAssignmentCreateParameters, options *armauthorization.RoleAssignmentsClientCreateOptions) (armauthorization.RoleAssignmentsClientCreateResponse, error) {
			return armauthorization.RoleAssignmentsClientCreateResponse{}, newMockResponseError("RoleAssignmentExists", "Role assignment already exists")
		},
	}

	installer := &Installer{
		base: &base{
			config:                cfg,
			logger:                logger,
			roleAssignmentsClient: mockClient,
		},
	}

	// Execute
	ctx := context.Background()
	err := installer.assignRole(ctx, "test-principal-id", "test-role-id", "/test/scope", "TestRole")

	// Verify - should succeed even though API returned error
	if err != nil {
		t.Errorf("Expected no error when role already exists, got: %v", err)
	}
	if mockClient.callCount != 1 {
		t.Errorf("Expected 1 API call, got %d", mockClient.callCount)
	}
}

func TestAssignRole_ContextCancellation(t *testing.T) {
	// Setup
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	cfg := &config.Config{
		Azure: config.AzureConfig{
			SubscriptionID: "test-sub-id",
		},
	}

	mockClient := &mockRoleAssignmentsClient{
		createFunc: func(ctx context.Context, scope string, roleAssignmentName string, parameters armauthorization.RoleAssignmentCreateParameters, options *armauthorization.RoleAssignmentsClientCreateOptions) (armauthorization.RoleAssignmentsClientCreateResponse, error) {
			// Always fail to trigger retry
			return armauthorization.RoleAssignmentsClientCreateResponse{}, newMockResponseError("PrincipalNotFound", "Principal does not exist")
		},
	}

	installer := &Installer{
		base: &base{
			config:                cfg,
			logger:                logger,
			roleAssignmentsClient: mockClient,
		},
	}

	// Execute with cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after first attempt triggers retry
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := installer.assignRole(ctx, "test-principal-id", "test-role-id", "/test/scope", "TestRole")

	// Verify - should fail with context error
	if err == nil {
		t.Error("Expected context cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Expected context.Canceled error, got: %v", err)
	}
}

func TestAssignRole_GenericError_NoRetry(t *testing.T) {
	// Setup
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	cfg := &config.Config{
		Azure: config.AzureConfig{
			SubscriptionID: "test-sub-id",
		},
	}

	mockClient := &mockRoleAssignmentsClient{
		createFunc: func(ctx context.Context, scope string, roleAssignmentName string, parameters armauthorization.RoleAssignmentCreateParameters, options *armauthorization.RoleAssignmentsClientCreateOptions) (armauthorization.RoleAssignmentsClientCreateResponse, error) {
			return armauthorization.RoleAssignmentsClientCreateResponse{}, errors.New("some other Azure error")
		},
	}

	installer := &Installer{
		base: &base{
			config:                cfg,
			logger:                logger,
			roleAssignmentsClient: mockClient,
		},
	}

	// Execute
	ctx := context.Background()
	err := installer.assignRole(ctx, "test-principal-id", "test-role-id", "/test/scope", "TestRole")

	// Verify
	if err == nil {
		t.Error("Expected error, got nil")
	}
	if mockClient.callCount != 1 {
		t.Errorf("Expected 1 API call (no retry on generic error), got %d", mockClient.callCount)
	}
}

func TestAssignRole_PrincipalTypeIsSetCorrectly(t *testing.T) {
	// Setup
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	cfg := &config.Config{
		Azure: config.AzureConfig{
			SubscriptionID: "test-sub-id",
		},
	}

	var capturedPrincipalType *armauthorization.PrincipalType
	mockClient := &mockRoleAssignmentsClient{
		createFunc: func(ctx context.Context, scope string, roleAssignmentName string, parameters armauthorization.RoleAssignmentCreateParameters, options *armauthorization.RoleAssignmentsClientCreateOptions) (armauthorization.RoleAssignmentsClientCreateResponse, error) {
			if parameters.Properties != nil && parameters.Properties.PrincipalType != nil {
				capturedPrincipalType = parameters.Properties.PrincipalType
			}
			return armauthorization.RoleAssignmentsClientCreateResponse{}, nil
		},
	}

	installer := &Installer{
		base: &base{
			config:                cfg,
			logger:                logger,
			roleAssignmentsClient: mockClient,
		},
	}

	// Execute
	ctx := context.Background()
	_ = installer.assignRole(ctx, "test-principal-id", "test-role-id", "/test/scope", "TestRole")

	// Verify
	if capturedPrincipalType == nil {
		t.Fatal("PrincipalType was not set in the role assignment")
	}
	if *capturedPrincipalType != armauthorization.PrincipalTypeServicePrincipal {
		t.Errorf("Expected PrincipalType to be ServicePrincipal, got %s", *capturedPrincipalType)
	}
}

func TestAssignRole_ExponentialBackoff(t *testing.T) {
	// Setup
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	cfg := &config.Config{
		Azure: config.AzureConfig{
			SubscriptionID: "test-sub-id",
		},
	}

	var attemptTimes []time.Time
	mockClient := &mockRoleAssignmentsClient{
		createFunc: func(ctx context.Context, scope string, roleAssignmentName string, parameters armauthorization.RoleAssignmentCreateParameters, options *armauthorization.RoleAssignmentsClientCreateOptions) (armauthorization.RoleAssignmentsClientCreateResponse, error) {
			attemptTimes = append(attemptTimes, time.Now())
			// Fail first 3 attempts, succeed on 4th
			if len(attemptTimes) < 4 {
				return armauthorization.RoleAssignmentsClientCreateResponse{}, newMockResponseError("PrincipalNotFound", "Principal does not exist")
			}
			return armauthorization.RoleAssignmentsClientCreateResponse{}, nil
		},
	}

	installer := &Installer{
		base: &base{
			config:                cfg,
			logger:                logger,
			roleAssignmentsClient: mockClient,
		},
	}

	// Execute
	ctx := context.Background()
	err := installer.assignRole(ctx, "test-principal-id", "test-role-id", "/test/scope", "TestRole")

	// Verify
	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}
	if len(attemptTimes) != 4 {
		t.Fatalf("Expected 4 attempts, got %d", len(attemptTimes))
	}

	// Check backoff delays: attempt1->attempt2 (~5s), attempt2->attempt3 (~10s), attempt3->attempt4 (~20s)
	delays := []time.Duration{
		attemptTimes[1].Sub(attemptTimes[0]),
		attemptTimes[2].Sub(attemptTimes[1]),
		attemptTimes[3].Sub(attemptTimes[2]),
	}

	expectedDelays := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}
	tolerance := 500 * time.Millisecond

	for i, delay := range delays {
		if delay < expectedDelays[i]-tolerance || delay > expectedDelays[i]+tolerance {
			t.Errorf("Attempt %d->%d: expected delay ~%v, got %v", i+1, i+2, expectedDelays[i], delay)
		}
	}
}
