package store

import (
	"context"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestDeviceInventoryFiltersAndConveniencePathsIntegration(t *testing.T) {
	env := newProjectionIntegrationEnv(t)
	registered, first := createProjectionDevice(t, env, map[string]any{
		"region":           "jp-east",
		"readiness":        "ready",
		"firmware_version": "1.2.3",
	})
	ctx := context.Background()
	modelName := "RTK-CAM"
	second, err := env.store.CreateDevice(ctx, registered.Organization.ID, DeviceInput{
		Name:     "camera-2",
		Category: model.DeviceCategoryIPCamera,
		Model:    &modelName,
		Metadata: map[string]any{
			"region":           "us-west",
			"readiness":        "pending",
			"firmware_version": "2.0.0",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	group, err := env.store.CreateDeviceGroup(ctx, registered.Organization.ID, DeviceGroupInput{Name: "qualification"})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.AddDeviceToGroup(ctx, registered.Organization.ID, group.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RemoveDeviceFromGroup(ctx, registered.Organization.ID, group.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RemoveDeviceFromGroup(ctx, registered.Organization.ID, group.ID, second.ID); err != nil {
		t.Fatalf("idempotent group removal failed: %v", err)
	}
	if err := env.store.AddDeviceToGroup(ctx, registered.Organization.ID, group.ID, second.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := env.store.AddDeviceTag(ctx, registered.Organization.ID, second.ID, "qualification"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.AddDeviceTag(ctx, registered.Organization.ID, second.ID, "qualification"); err != nil {
		t.Fatalf("idempotent tag add failed: %v", err)
	}
	tags, err := env.store.ListDeviceTags(ctx, registered.Organization.ID, second.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tags.Page.Total != 1 || len(tags.Tags) != 1 {
		t.Fatalf("device tags = %+v, want one tag", tags)
	}
	if err := env.store.DeleteDeviceTag(ctx, registered.Organization.ID, second.ID, "qualification"); err != nil {
		t.Fatal(err)
	}
	if err := env.store.DeleteDeviceTag(ctx, registered.Organization.ID, second.ID, "qualification"); err != nil {
		t.Fatalf("idempotent tag delete failed: %v", err)
	}

	page, err := env.store.ListDevices(ctx, registered.Organization.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Page.Total != 2 || len(page.Devices) != 2 {
		t.Fatalf("ListDevices() = total %d, devices %d; want 2/2", page.Page.Total, len(page.Devices))
	}

	filtered, err := env.store.ListDevicesFiltered(ctx, DeviceListFilter{
		OrganizationID: registered.Organization.ID,
		Query:          "camera-2",
		GroupID:        " ",
		GroupIDs:       []string{" ", group.ID},
		Region:         " ",
		Regions:        []string{"", "us-west"},
		Category:       string(model.DeviceCategoryIPCamera),
		Model:          modelName,
		Status:         " ",
		Statuses:       []string{"", string(model.DeviceStatusUnknown)},
		Readiness:      "pending",
		Firmware:       " ",
		Firmwares:      []string{"", "2.0.0"},
		Sort:           "product",
		Direction:      "ASC",
		Limit:          500,
		Offset:         -10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Page.Total != 1 || len(filtered.Devices) != 1 || filtered.Devices[0].ID != second.ID {
		t.Fatalf("filtered devices = %+v, want only %s", filtered.Devices, second.ID)
	}
	if filtered.Page.Limit != 250 || filtered.Page.Offset != 0 {
		t.Fatalf("normalized page = %d/%d, want 250/0", filtered.Page.Limit, filtered.Page.Offset)
	}

	unauthorized, err := env.store.ListDevicesFiltered(ctx, DeviceListFilter{
		OrganizationID:   registered.Organization.ID,
		BrandCloudUserID: "00000000-0000-0000-0000-000000000000",
		Limit:            1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if unauthorized.Page.Total != 0 || len(unauthorized.Devices) != 0 {
		t.Fatalf("unauthorized inventory = %+v, want empty", unauthorized)
	}
	unauthorizedUser, err := env.store.ListDevicesFiltered(ctx, DeviceListFilter{OrganizationID: registered.Organization.ID, UserID: "00000000-0000-0000-0000-000000000000", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if unauthorizedUser.Page.Total != 0 || len(unauthorizedUser.Devices) != 0 {
		t.Fatalf("unauthorized global developer inventory = %+v, want empty", unauthorizedUser)
	}

	summary, err := env.store.FleetSummary(ctx, registered.Organization.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 2 || summary.ByRegion["jp-east"] != 1 || summary.ByRegion["us-west"] != 1 {
		t.Fatalf("fleet summary = %+v, want two devices across both regions", summary)
	}
	if summary.ByFirmware["1.2.3"] != 1 || summary.ByFirmware["2.0.0"] != 1 {
		t.Fatalf("firmware summary = %+v, want both versions", summary.ByFirmware)
	}
	restrictedSummary, err := env.store.FleetSummaryForBrandCloudUser(
		ctx,
		registered.Organization.ID,
		"00000000-0000-0000-0000-000000000000",
	)
	if err != nil {
		t.Fatal(err)
	}
	if restrictedSummary.Total != 0 {
		t.Fatalf("restricted fleet summary total = %d, want 0", restrictedSummary.Total)
	}
	restrictedUserSummary, err := env.store.FleetSummaryForUser(ctx, registered.Organization.ID, "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if restrictedUserSummary.Total != 0 {
		t.Fatalf("restricted global developer fleet summary total = %d, want 0", restrictedUserSummary.Total)
	}

	count, err := env.store.countDevices(ctx, registered.Organization.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("countDevices() = %d, want 2", count)
	}
	if err := env.store.createScopedLoginActivationToken(
		ctx,
		registered.User.ID,
		"coverage-target",
		"coverage-target-token",
		time.Now().Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("fixture devices must be distinct")
	}

	secondaryAccount, err := env.store.Register(ctx, RegisterInput{
		Email:            "coverage-member@example.com",
		PasswordHash:     "member-password-hash",
		OrganizationName: "Coverage Member Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	member, err := env.store.AddMember(
		ctx,
		registered.Organization.ID,
		secondaryAccount.User.Email,
		model.RoleAdmin,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.DisableMemberUser(ctx, registered.Organization.ID, member.UserID); err != nil {
		t.Fatal(err)
	}
	enabled, err := env.store.EnableMemberUser(ctx, registered.Organization.ID, member.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if enabled.UserID != member.UserID {
		t.Fatalf("enabled member = %+v, want user %s", enabled, member.UserID)
	}
	if err := env.store.UpdateUserPassword(ctx, member.UserID, "rotated-password-hash"); err != nil {
		t.Fatal(err)
	}
	if err := env.store.UpdateUserPassword(ctx, "00000000-0000-0000-0000-000000000000", "unused"); err != ErrNotFound {
		t.Fatalf("missing user password update error = %v, want ErrNotFound", err)
	}
}

func TestStoreOperationsRespectCanceledContextIntegration(t *testing.T) {
	env := newProjectionIntegrationEnv(t)
	registered, device := createProjectionDevice(t, env, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assertError := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s unexpectedly succeeded with canceled context", name)
		}
	}

	_, err := env.store.Register(ctx, RegisterInput{Email: "canceled@example.com", OrganizationName: "Canceled"})
	assertError("Register", err)
	_, err = env.store.EnsurePlatformAdmin(ctx, "canceled-admin@example.com", "hash", nil)
	assertError("EnsurePlatformAdmin", err)
	_, err = env.store.CreateDevice(ctx, registered.Organization.ID, DeviceInput{Name: "canceled", Category: model.DeviceCategoryIPCamera})
	assertError("CreateDevice", err)
	_, err = env.store.UpdateMemberRole(ctx, registered.Organization.ID, registered.User.ID, model.RoleOwner)
	assertError("UpdateMemberRole", err)
	_, err = env.store.DisableMemberUser(ctx, registered.Organization.ID, registered.User.ID)
	assertError("DisableMemberUser", err)
	_, err = env.store.EnableMemberUser(ctx, registered.Organization.ID, registered.User.ID)
	assertError("EnableMemberUser", err)
	assertError("RemoveMember", env.store.RemoveMember(ctx, registered.Organization.ID, registered.User.ID))
	assertError("UpdateUserPassword", env.store.UpdateUserPassword(ctx, registered.User.ID, "hash"))
	_, err = env.store.FleetSummary(ctx, registered.Organization.ID)
	assertError("FleetSummary", err)
	_, err = env.store.ListDevicesFiltered(ctx, DeviceListFilter{OrganizationID: registered.Organization.ID})
	assertError("ListDevicesFiltered", err)
	_, err = env.store.CreateDeviceGroup(ctx, registered.Organization.ID, DeviceGroupInput{Name: "canceled"})
	assertError("CreateDeviceGroup", err)
	_, err = env.store.AddDeviceTag(ctx, registered.Organization.ID, device.ID, "canceled")
	assertError("AddDeviceTag", err)
	_, err = env.store.GetBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferQuery{}, time.Now())
	assertError("GetBrandCloudOwnerTransfer", err)
	_, err = env.store.CreateBrandCloudOwnerTransfer(ctx, BrandCloudOwnerTransferInput{})
	assertError("CreateBrandCloudOwnerTransfer", err)
	_, _, _, err = env.store.RequeueOutboxMessage(ctx, "canceled", time.Now())
	assertError("RequeueOutboxMessage", err)
	_, _, _, err = env.store.RequeueInboxMessage(ctx, "canceled")
	assertError("RequeueInboxMessage", err)
}
