package aksmachine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
)

func operationConflictResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)), Request: request,
	}
}

func TestARMOperationInProgressClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		status     int
		want       bool
	}{
		{name: "observed AKS subcode", status: 409, body: `{"code":"OperationNotAllowed","subcode":"AnotherOperationInProgress","message":"pool update is running"}`, want: true},
		{name: "wrapped AKS subcode", status: 409, body: `{"error":{"code":"OperationNotAllowed","subcode":"AnotherOperationInProgress"}}`, want: true},
		{name: "direct error code", status: 409, body: `{"error":{"code":"AnotherOperationInProgress"}}`, want: true},
		{name: "case insensitive", status: 409, body: `{"code":"operationnotallowed","subcode":"anotheroperationinprogress"}`, want: true},
		{name: "unrelated conflict", status: 409, body: `{"error":{"code":"ResourceConflict","message":"resource already exists"}}`},
		{name: "other operation not allowed", status: 409, body: `{"code":"OperationNotAllowed","subcode":"DifferentFailure"}`},
		{name: "message is not a code", status: 409, body: `{"code":"OperationNotAllowed","message":"AnotherOperationInProgress"}`},
		{name: "unauthorized", status: 401, body: `{"code":"OperationNotAllowed","subcode":"AnotherOperationInProgress"}`},
		{name: "forbidden", status: 403, body: `{"code":"OperationNotAllowed","subcode":"AnotherOperationInProgress"}`},
		{name: "non-conflict", status: 400, body: `{"code":"AnotherOperationInProgress"}`},
		{name: "malformed response", status: 409, body: `{"code":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			response := operationConflictResponse(nil, tc.status, tc.body)
			err := runtime.NewResponseError(response)
			for range 2 {
				if got := isARMOperationInProgress(err); got != tc.want {
					t.Fatalf("classification = %t, want %t; error=%v", got, tc.want, err)
				}
			}
		})
	}
	for _, err := range []error{nil, errors.New("network failure"), &azcore.ResponseError{StatusCode: 409, ErrorCode: "OperationNotAllowed"}} {
		if isARMOperationInProgress(err) {
			t.Fatalf("accepted an unproven operation conflict: %v", err)
		}
	}
}

func TestARMMachineCreateRetriesOnlyOperationConflict(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"observed subcode", "direct code"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int32
			var path string
			client := newTestARMMachineClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPut {
					t.Fatalf("unexpected method during rejected PUT retry: %s", request.Method)
				}
				if attempts.Add(1) == 1 {
					path = request.URL.String()
					body := `{"code":"OperationNotAllowed","subcode":"AnotherOperationInProgress"}`
					if code == "direct code" {
						body = `{"error":{"code":"AnotherOperationInProgress"}}`
					}
					return operationConflictResponse(request, 409, body), nil
				}
				if request.URL.String() != path {
					t.Fatal("retry changed the target resource")
				}
				deadline, ok := request.Context().Deadline()
				if !ok || time.Until(deadline) > armMachineOperationConflictTimeout {
					t.Fatal("operation-conflict retry lacks a bounded context")
				}
				return successfulMachineResponse(t, request), nil
			}))
			machine, err := client.Create(t.Context(), GoalState{KubernetesVersion: "1.34.0"})
			if err != nil {
				t.Fatal(err)
			}
			if attempts.Load() != 2 || machine.Goal.SettingsVersion != "settings-1" {
				t.Fatalf("unexpected retry result: attempts=%d machine=%+v", attempts.Load(), machine)
			}
		})
	}
}

func TestARMMachineCreateDoesNotRetryPermanentConflictsOrAuthFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{name: "resource conflict", status: 409, body: `{"error":{"code":"ResourceConflict"}}`},
		{name: "operation denied", status: 409, body: `{"error":{"code":"OperationNotAllowed"}}`},
		{name: "unauthorized", status: 401, body: `{"error":{"code":"Unauthorized"}}`},
		{name: "forbidden", status: 403, body: `{"error":{"code":"AuthorizationFailed"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int32
			client := newTestARMMachineClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempts.Add(1)
				return operationConflictResponse(request, tc.status, tc.body), nil
			}))
			_, err := client.Create(t.Context(), GoalState{KubernetesVersion: "1.34.0"})
			var responseErr *azcore.ResponseError
			if !errors.As(err, &responseErr) || responseErr.StatusCode != tc.status || attempts.Load() != 1 {
				t.Fatalf("permanent error was retried or lost: attempts=%d error=%v", attempts.Load(), err)
			}
		})
	}
}

func TestARMMachineCreateDoesNotReplayAcceptedOperation(t *testing.T) {
	t.Parallel()
	var puts, polls atomic.Int32
	client := newTestARMMachineClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPut {
			puts.Add(1)
			response := successfulMachineResponse(t, request)
			response.StatusCode = http.StatusCreated
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			response.Body = io.NopCloser(strings.NewReader(strings.Replace(string(body), "Succeeded", "Creating", 1)))
			response.Header.Set("Azure-AsyncOperation", "https://management.azure.com/operations/test-machine")
			return response, nil
		}
		polls.Add(1)
		return operationConflictResponse(request, 409, `{"code":"OperationNotAllowed","subcode":"AnotherOperationInProgress"}`), nil
	}))
	_, err := client.Create(t.Context(), GoalState{KubernetesVersion: "1.34.0"})
	var responseErr *azcore.ResponseError
	if !errors.As(err, &responseErr) || responseErr.StatusCode != 409 || puts.Load() != 1 || polls.Load() != 1 {
		t.Fatalf("accepted operation was replayed or its polling error was lost: puts=%d polls=%d error=%v", puts.Load(), polls.Load(), err)
	}
}

func TestARMMachineOperationConflictWaitRespectsContext(t *testing.T) {
	t.Parallel()
	for _, canceled := range []bool{false, true} {
		name := "deadline"
		if canceled {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			var attempts atomic.Int32
			client := newTestARMMachineClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempts.Add(1)
				if canceled {
					cancel()
				}
				return operationConflictResponse(request, 409, `{"code":"OperationNotAllowed","subcode":"AnotherOperationInProgress"}`), nil
			}))
			_, err := client.Create(ctx, GoalState{KubernetesVersion: "1.34.0"})
			want := context.DeadlineExceeded
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) || attempts.Load() != 1 {
				t.Fatalf("did not stop during retry backoff: attempts=%d error=%v", attempts.Load(), err)
			}
		})
	}
}
