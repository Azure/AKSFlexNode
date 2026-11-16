package aksmachine

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

func operationConflictResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)), Request: request,
	}
}

func TestARMMachineCreateReturnsConflictsAndAuthFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{name: "pool operation subcode", status: 409, body: `{"code":"OperationNotAllowed","subcode":"AnotherOperationInProgress"}`},
		{name: "wrapped pool operation subcode", status: 409, body: `{"error":{"code":"OperationNotAllowed","subcode":"AnotherOperationInProgress"}}`},
		{name: "pool operation code", status: 409, body: `{"error":{"code":"AnotherOperationInProgress"}}`},
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
				t.Fatalf("error was retried or lost: attempts=%d error=%v", attempts.Load(), err)
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
