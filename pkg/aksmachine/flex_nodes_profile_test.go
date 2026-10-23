package aksmachine

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
)

// rpMachineJSON mirrors the full Flex Machine GET shape returned by RP, with the
// disconnectTolerance member inside flexNodesProfile. The typed SDK model has no
// DT field, so fixtures are raw JSON on purpose.
func rpMachineJSON(dtMember string) string {
	if dtMember == "" {
		return rpMachineJSONWithProperties("")
	}
	return rpMachineJSONWithProperties(`"flexNodesProfile":{"disconnectTolerance":` + dtMember + `}`)
}

// rpMachineJSONWithProperties appends raw extra members to the Machine properties.
func rpMachineJSONWithProperties(extra string) string {
	properties := `"eTag":"settings-1","provisioningState":"Succeeded","kubernetes":{"orchestratorVersion":"1.34.0"}`
	if extra != "" {
		properties += `,` + extra
	}
	return `{"id":"` + testClusterResourceID + `/agentPools/aksflexnodes/machines/flex-node-1","name":"flex-node-1","properties":{` + properties + `}}`
}

func TestDisconnectToleranceFromMachineJSON(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		body    string
		want    bool
		wantErr bool
	}{
		"true":                         {body: rpMachineJSON(`{"enabled":true}`), want: true},
		"false":                        {body: rpMachineJSON(`{"enabled":false}`)},
		"object omitted is false":      {body: rpMachineJSON("")},
		"enabled omitted is false":     {body: rpMachineJSON(`{}`)},
		"extra members ignored":        {body: rpMachineJSON(`{"enabled":true,"future":1}`), want: true},
		"no properties":                {body: `{"name":"flex-node-1"}`},
		"null properties":              {body: `{"name":"flex-node-1","properties":null}`},
		"object null is malformed":     {body: rpMachineJSON("null"), wantErr: true},
		"dt not object is malformed":   {body: rpMachineJSON("true"), wantErr: true},
		"object is string":             {body: rpMachineJSON(`"true"`), wantErr: true},
		"enabled null":                 {body: rpMachineJSON(`{"enabled":null}`), wantErr: true},
		"enabled string":               {body: rpMachineJSON(`{"enabled":"true"}`), wantErr: true},
		"enabled number":               {body: rpMachineJSON(`{"enabled":1}`), wantErr: true},
		"invalid json":                 {body: `{"properties":`, wantErr: true},
		"profile omitted is false":     {body: rpMachineJSONWithProperties(`"flexNodesProfile":{}`)},
		"other profile settings":       {body: rpMachineJSONWithProperties(`"flexNodesProfile":{"future":1,"disconnectTolerance":{"enabled":true}}`), want: true},
		"profile null is malformed":    {body: rpMachineJSONWithProperties(`"flexNodesProfile":null`), wantErr: true},
		"profile not object":           {body: rpMachineJSONWithProperties(`"flexNodesProfile":true`), wantErr: true},
		"old unnested field ignored":   {body: rpMachineJSONWithProperties(`"disconnectTolerance":{"enabled":true}`)},
		"old singular profile ignored": {body: rpMachineJSONWithProperties(`"flexNodeProfile":{"disconnectTolerance":{"enabled":true}}`)},
		"old flat field ignored":       {body: rpMachineJSONWithProperties(`"disconnectToleranceEnabled":true`)},
		"properties not obj":           {body: `{"properties":true}`, wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := disconnectToleranceFromMachineJSON([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "flex-node-1") {
				t.Fatalf("error includes response content: %v", err)
			}
			if got != tt.want {
				t.Fatalf("enabled = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestARMMachineClientGetDecodesDisconnectTolerance(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		dtMember string
		want     *bool
	}{
		"true":                {dtMember: `{"enabled":true}`, want: ptr(true)},
		"false":               {dtMember: `{"enabled":false}`, want: ptr(false)},
		"omitted maps false":  {want: ptr(false)},
		"malformed keeps nil": {dtMember: `{"enabled":"yes"}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := newTestARMMachineClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(rpMachineJSON(tt.dtMember))),
					Request:    request,
				}, nil
			}))
			machine, err := client.Get(t.Context())
			if err != nil {
				// A malformed DT value must not fail the read: upgrade and
				// deletion handling still depend on it.
				t.Fatalf("Get() error = %v", err)
			}
			got := machine.Goal.DisconnectToleranceEnabled
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Fatalf("DisconnectToleranceEnabled = %v, want %v", got, tt.want)
			}
			if machine.Goal.SettingsVersion != "settings-1" || machine.Goal.KubernetesVersion != "1.34.0" {
				t.Fatalf("other goal fields lost: %+v", machine.Goal)
			}
		})
	}
}

func TestEndpointMachineDecodesDisconnectTolerance(t *testing.T) {
	t.Parallel()
	for name, dt := range map[string]string{"true": `{"enabled":true}`, "omitted": "", "malformed": "null"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			machine, err := machineFromEndpointJSON([]byte(rpMachineJSON(dt)))
			if err != nil {
				t.Fatal(err)
			}
			got := machine.Goal.DisconnectToleranceEnabled
			switch name {
			case "true":
				if got == nil || !*got {
					t.Fatalf("got %v, want true", got)
				}
			case "omitted":
				if got == nil || *got {
					t.Fatalf("got %v, want false", got)
				}
			case "malformed":
				if got != nil {
					t.Fatalf("got %v, want nil", *got)
				}
			}
		})
	}
}

func TestARMMachineClientAPIVersionOverride(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		override string
		want     string
	}{
		"default":  {},
		"explicit": {override: "2026-10-02-preview", want: "2026-10-02-preview"},
		"older":    {override: "2025-10-02-preview", want: "2025-10-02-preview"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				got := request.URL.Query().Get("api-version")
				if tt.want == "" && (got == "" || got == "2026-10-02-preview") {
					t.Errorf("%s api-version = %q, want SDK default", request.Method, got)
				}
				if tt.want != "" && got != tt.want {
					t.Errorf("%s api-version = %q, want %q", request.Method, got, tt.want)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(rpMachineJSON(`{"enabled":true}`))),
					Request:    request,
				}, nil
			})
			client := newTestARMMachineClient(t, transport)
			sdkClient, err := armcontainerservice.NewMachinesClient(client.machineID.SubscriptionID, staticARMProxyCredential{},
				armMachineClientOptions(azcore.ClientOptions{Transport: transport}, tt.override))
			if err != nil {
				t.Fatalf("NewMachinesClient() error = %v", err)
			}
			client.client = sdkClient
			if _, err := client.Get(t.Context()); err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if _, err := client.Create(t.Context(), GoalState{KubernetesVersion: "1.34.0"}); err != nil {
				t.Fatalf("Create() error = %v", err)
			}
		})
	}
}
