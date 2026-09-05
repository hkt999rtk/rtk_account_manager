package api

import (
	"encoding/json"
	"testing"
)

func TestDeviceCreateRequestRetainsProductAssociation(t *testing.T) {
	var request deviceRequest
	if err := json.Unmarshal([]byte(`{"name":"Test device","category":"ip_camera","device_item_profile_id":"9e03c191-f87d-4e19-a24a-0974c98c6d2a"}`), &request); err != nil {
		t.Fatal(err)
	}
	in := request.input()
	if in.DeviceItemProfileID == nil || *in.DeviceItemProfileID != "9e03c191-f87d-4e19-a24a-0974c98c6d2a" {
		t.Fatal("Product ID discarded before store creation")
	}
}
