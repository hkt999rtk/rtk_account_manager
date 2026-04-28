package store

import (
	"encoding/json"
	"errors"
	"testing"

	"rtk_account_manager/internal/model"
)

func TestCompareOperationCreate(t *testing.T) {
	existing := model.DeviceOperation{
		CorrelationID:  "corr-1",
		OrganizationID: "org-1",
		DeviceID:       "device-1",
		OperationType:  model.DeviceOperationTypeProvision,
		RequestPayload: map[string]any{"video_cloud_devid": "device-1"},
	}
	requestPayload, err := json.Marshal(map[string]any{"video_cloud_devid": "device-1"})
	if err != nil {
		t.Fatal(err)
	}

	if err := compareOperationCreate(existing, DeviceOperationCreateInput{
		CorrelationID:  "corr-1",
		OrganizationID: "org-1",
		DeviceID:       "device-1",
		OperationType:  model.DeviceOperationTypeProvision,
	}, requestPayload); err != nil {
		t.Fatalf("expected identical operation create input to match, got %v", err)
	}

	if err := compareOperationCreate(existing, DeviceOperationCreateInput{
		CorrelationID:  "corr-2",
		OrganizationID: "org-1",
		DeviceID:       "device-1",
		OperationType:  model.DeviceOperationTypeProvision,
	}, requestPayload); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected correlation mismatch to conflict, got %v", err)
	}

	if err := compareOperationCreate(existing, DeviceOperationCreateInput{
		CorrelationID:  "corr-1",
		OrganizationID: "org-2",
		DeviceID:       "device-1",
		OperationType:  model.DeviceOperationTypeProvision,
	}, requestPayload); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected org mismatch to conflict, got %v", err)
	}

	if err := compareOperationCreate(existing, DeviceOperationCreateInput{
		CorrelationID:  "corr-1",
		OrganizationID: "org-1",
		DeviceID:       "device-2",
		OperationType:  model.DeviceOperationTypeProvision,
	}, requestPayload); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected device mismatch to conflict, got %v", err)
	}

	if err := compareOperationCreate(existing, DeviceOperationCreateInput{
		CorrelationID:  "corr-1",
		OrganizationID: "org-1",
		DeviceID:       "device-1",
		OperationType:  model.DeviceOperationTypeDeactivate,
	}, requestPayload); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected operation type mismatch to conflict, got %v", err)
	}

	differentPayload, err := json.Marshal(map[string]any{"video_cloud_devid": "device-2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := compareOperationCreate(existing, DeviceOperationCreateInput{
		CorrelationID:  "corr-1",
		OrganizationID: "org-1",
		DeviceID:       "device-1",
		OperationType:  model.DeviceOperationTypeProvision,
	}, differentPayload); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected payload mismatch to conflict, got %v", err)
	}
}

func TestJSONHelpers(t *testing.T) {
	if !sameStringPtr(nil, nil) {
		t.Fatal("expected nil pointers to match")
	}
	if sameStringPtr(stringPtr("left"), nil) {
		t.Fatal("expected nil mismatch to fail")
	}
	if !sameStringPtr(stringPtr("same"), stringPtr("same")) {
		t.Fatal("expected equal string pointers to match")
	}
	if sameStringPtr(stringPtr("left"), stringPtr("right")) {
		t.Fatal("expected different string pointers to fail")
	}

	want, err := marshalJSONMap(map[string]any{"ok": true})
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSONMap(map[string]any{"ok": true}, want) {
		t.Fatal("expected equivalent JSON maps to match")
	}
	if sameJSONMap(map[string]any{"bad": make(chan int)}, want) {
		t.Fatal("expected invalid JSON input to fail comparison")
	}

	decoded, err := unmarshalJSONMap(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 0 {
		t.Fatalf("expected empty map for nil payload, got %+v", decoded)
	}

	decoded, err = unmarshalJSONMap([]byte("null"))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 0 {
		t.Fatalf("expected empty map for null payload, got %+v", decoded)
	}

	decoded, err = unmarshalJSONMap([]byte(`{"key":"value"}`))
	if err != nil {
		t.Fatal(err)
	}
	if decoded["key"] != "value" {
		t.Fatalf("expected decoded value, got %+v", decoded)
	}

	if _, err := unmarshalJSONMap([]byte("{")); err == nil {
		t.Fatal("expected invalid JSON payload to fail")
	}
}
