package store

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestRegisteredOptionsPinProductRunAndFactoryGrant(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	environment := "integration-service-registry-" + strconv.FormatInt(now.UnixNano(), 10)
	cleanupServiceEnvironment(t, env, environment)
	owner := handoffDeveloper(t, env, "service-registry")
	env.store.ConfigurePlatformServiceProductWrites(environment, true)
	principal := PlatformServicePrincipal{Environment: environment, CertificateSubject: "service:mqtt", IssuerFingerprint: strings.Repeat("a", 64)}
	mqttManifest := PlatformServiceRegistration{RequestID: "mqtt-start", ServiceID: "mqtt", InstanceID: "mqtt-1", ManifestVersion: "1", ProtocolVersion: "1", EndpointRef: "mqtt", Ready: true,
		Options: []PlatformServiceOption{{Code: "mqtt", DisplayName: "MQTT"}}}
	if _, err := env.store.RegisterPlatformServiceInstance(ctx, mqttManifest, principal, now); !errors.Is(err, ErrServiceRegistrationDenied) {
		t.Fatalf("unapproved registration = %v", err)
	}
	if err := env.store.ApprovePlatformServiceWorkload(ctx, PlatformServiceWorkloadApproval{Environment: environment, ServiceID: "mqtt", InstanceID: "mqtt-1", CertificateSubject: principal.CertificateSubject, IssuerFingerprint: principal.IssuerFingerprint, AllowedOptionCodes: []string{"mqtt"}, ApprovedBy: owner.User.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.RegisterPlatformServiceInstance(ctx, mqttManifest, principal, now); err != nil {
		t.Fatal(err)
	}
	shadowPrincipal := PlatformServicePrincipal{Environment: environment, CertificateSubject: "service:shadow", IssuerFingerprint: strings.Repeat("a", 64)}
	if err := env.store.ApprovePlatformServiceWorkload(ctx, PlatformServiceWorkloadApproval{Environment: environment, ServiceID: "shadow", InstanceID: "shadow-1", CertificateSubject: shadowPrincipal.CertificateSubject, IssuerFingerprint: shadowPrincipal.IssuerFingerprint, AllowedOptionCodes: []string{"iot_shadow"}, ApprovedBy: owner.User.ID}); err != nil {
		t.Fatal(err)
	}
	shadowManifest := PlatformServiceRegistration{RequestID: "shadow-start", ServiceID: "shadow", InstanceID: "shadow-1", ManifestVersion: "1", ProtocolVersion: "1", EndpointRef: "shadow", Ready: true,
		Options: []PlatformServiceOption{{Code: "iot_shadow", DisplayName: "IoT Shadow", Requires: []string{"mqtt"}}}}
	if _, err := env.store.RegisterPlatformServiceInstance(ctx, shadowManifest, shadowPrincipal, now); err != nil {
		t.Fatal(err)
	}
	shadowManifest.Options[0].Code = "video_storage"
	if _, err := env.store.RegisterPlatformServiceInstance(ctx, shadowManifest, shadowPrincipal, now); !errors.Is(err, ErrServiceRegistrationDenied) {
		t.Fatalf("foreign option = %v", err)
	}
	shadowManifest.Options[0].Code = "iot_shadow"
	catalog, err := env.store.ListPlatformServiceOptions(ctx, environment, now)
	if err != nil || catalog.CatalogRevision < 2 || len(catalog.Options) != 2 {
		t.Fatalf("catalog = %+v %v", catalog, err)
	}
	if !catalog.Options[0].Selectable || catalog.Options[1].Selectable || catalog.Options[1].UnavailableReason != "service_suspended" {
		t.Fatalf("new Shadow service should be visible but suspended: %+v", catalog.Options)
	}
	create := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "registered-product")
	create.ServiceOptions = []string{"iot_shadow", "mqtt"}
	create.CatalogRevision = catalog.CatalogRevision
	if _, err := env.store.CreateDeviceItemProfileAsUser(ctx, create); !errors.Is(err, ErrConflict) {
		t.Fatalf("Product selected suspended Shadow: %v", err)
	}
	if _, err := env.store.SetPlatformServiceStatus(ctx, environment, "shadow", "active", owner.User.ID); err != nil {
		t.Fatal(err)
	}
	catalog, err = env.store.ListPlatformServiceOptions(ctx, environment, now)
	if err != nil || !catalog.Options[1].Selectable {
		t.Fatalf("activated Shadow unavailable: %+v %v", catalog, err)
	}
	create.CatalogRevision = catalog.CatalogRevision
	profile, err := env.store.CreateDeviceItemProfileAsUser(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	readyDevicePKIFixture(t, env, profile.ID)
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	claimInput := platformTokenInput(owner.User.ID, owner.BrandCloud.ID, "registered-claim", &profile.ID)
	claimInput.ServiceOptions = []string{"mqtt", "video_storage"}
	if _, err := env.store.CreateDeviceClaimTokenAsPlatform(ctx, claimInput); !errors.Is(err, ErrClaimServiceOptionsMismatch) {
		t.Fatalf("claim token expanded Product grant: %v", err)
	}
	claimInput.ServiceOptions = nil
	claim, err := env.store.CreateDeviceClaimTokenAsPlatform(ctx, claimInput)
	if err != nil || !serviceOptionSetsEqual(claim.ServiceOptions, []string{"mqtt", "iot_shadow"}) || claim.Metadata["product_service_revision"] == nil {
		t.Fatalf("claim token did not pin Product grant: %+v %v", claim, err)
	}
	runInput := authorizedProductionInput(owner.User.ID, owner.BrandCloud.ID, profile.ID)
	var issuedOptions []string
	firstRun, _, err := env.store.IssueProductionRunAsUser(ctx, runInput, func(run model.ProductionRun, p model.DeviceItemProfile) (string, error) {
		issuedOptions = slices.Clone(p.ServiceOptions)
		return "signed-fixture", nil
	})
	if err != nil || firstRun.ProductServiceRevision == nil || firstRun.ServiceGrantSHA256 == "" || !slices.Equal(issuedOptions, []string{"iot_shadow", "mqtt"}) {
		t.Fatalf("first run = %+v, options=%v, error=%v", firstRun, issuedOptions, err)
	}
	if _, err := env.store.UpdateDeviceItemProfileAsUser(ctx, DeviceItemProfileUpdateInput{ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID, ProfileID: profile.ID, ServiceOptions: []string{"mqtt"}, CatalogRevision: catalog.CatalogRevision}); err != nil {
		t.Fatal(err)
	}
	secondRun, _, err := env.store.IssueProductionRunAsUser(ctx, runInput, func(run model.ProductionRun, p model.DeviceItemProfile) (string, error) { return "signed-fixture", nil })
	if err != nil || secondRun.ProductServiceRevision == nil || *secondRun.ProductServiceRevision <= *firstRun.ProductServiceRevision {
		t.Fatalf("second run = %+v, error=%v", secondRun, err)
	}
	admission := FactoryEnrollmentAdmission{RunID: firstRun.ID, CloudID: owner.BrandCloud.ID, ProductID: profile.ID, RequestID: "factory-1", DeviceID: "device-1", RequestSHA256: strings.Repeat("b", 64), ProductServiceRevision: firstRun.ProductServiceRevision, ServiceGrantSHA256: firstRun.ServiceGrantSHA256, ServiceOptions: issuedOptions}
	if _, err := env.store.ReserveFactoryEnrollment(ctx, admission); err != nil {
		t.Fatalf("pinned first run rejected after Product edit: %v", err)
	}
	bad := admission
	bad.RequestID = "factory-2"
	bad.ServiceOptions = []string{"mqtt", "video_storage"}
	if _, err := env.store.ReserveFactoryEnrollment(ctx, bad); !errors.Is(err, ErrConflict) {
		t.Fatalf("tampered grant admitted: %v", err)
	}
	stale := create
	stale.ProfileKey = "stale-product"
	stale.CatalogRevision--
	if _, err := env.store.CreateDeviceItemProfileAsUser(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale catalog accepted: %v", err)
	}
}

func TestThirdPartyOptionRequiresRegistrationBeforeProductAndRun(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	environment := "integration-third-party-" + strconv.FormatInt(now.UnixNano(), 10)
	cleanupServiceEnvironment(t, env, environment)
	owner := handoffDeveloper(t, env, "third-party-service")
	env.store.ConfigurePlatformServiceProductWrites(environment, true)

	mqttPrincipal := PlatformServicePrincipal{Environment: environment, CertificateSubject: "service:mqtt", IssuerFingerprint: strings.Repeat("a", 64)}
	if err := env.store.ApprovePlatformServiceWorkload(ctx, PlatformServiceWorkloadApproval{Environment: environment, ServiceID: "mqtt", InstanceID: "mqtt-1", CertificateSubject: mqttPrincipal.CertificateSubject, IssuerFingerprint: mqttPrincipal.IssuerFingerprint, AllowedOptionCodes: []string{"mqtt"}, ApprovedBy: owner.User.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.RegisterPlatformServiceInstance(ctx, PlatformServiceRegistration{RequestID: "mqtt-start", ServiceID: "mqtt", InstanceID: "mqtt-1", ManifestVersion: "1", ProtocolVersion: "1", EndpointRef: "mqtt", Ready: true, Options: []PlatformServiceOption{{Code: "mqtt", DisplayName: "MQTT"}}}, mqttPrincipal, now); err != nil {
		t.Fatal(err)
	}
	catalog, err := env.store.ListPlatformServiceOptions(ctx, environment, now)
	if err != nil || len(catalog.Options) != 1 {
		t.Fatalf("MQTT-only catalog = %+v %v", catalog, err)
	}
	create := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "third-party-product")
	create.ServiceOptions = []string{"mqtt", "third_party_option"}
	create.CatalogRevision = catalog.CatalogRevision
	if _, err := env.store.CreateDeviceItemProfileAsUser(ctx, create); !errors.Is(err, ErrClaimUnsupportedService) {
		t.Fatalf("unregistered option accepted: %v", err)
	}

	pluginPrincipal := PlatformServicePrincipal{Environment: environment, CertificateSubject: "service:third-party", IssuerFingerprint: strings.Repeat("b", 64)}
	if err := env.store.ApprovePlatformServiceWorkload(ctx, PlatformServiceWorkloadApproval{Environment: environment, ServiceID: "third-party", InstanceID: "plugin-1", CertificateSubject: pluginPrincipal.CertificateSubject, IssuerFingerprint: pluginPrincipal.IssuerFingerprint, AllowedOptionCodes: []string{"third_party_option"}, ApprovedBy: owner.User.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.RegisterPlatformServiceInstance(ctx, PlatformServiceRegistration{RequestID: "plugin-start", ServiceID: "third-party", InstanceID: "plugin-1", ManifestVersion: "1", ProtocolVersion: "1", EndpointRef: "plugin", Ready: true, Options: []PlatformServiceOption{{Code: "third_party_option", DisplayName: "Third-party option", Requires: []string{"mqtt"}}}}, pluginPrincipal, now); err != nil {
		t.Fatal(err)
	}
	catalog, err = env.store.ListPlatformServiceOptions(ctx, environment, now)
	if err != nil || len(catalog.Options) != 2 || catalog.Options[1].Selectable || catalog.Options[1].UnavailableReason != "service_suspended" {
		t.Fatalf("registered plugin catalog = %+v %v", catalog, err)
	}
	create.CatalogRevision = catalog.CatalogRevision
	if _, err := env.store.CreateDeviceItemProfileAsUser(ctx, create); !errors.Is(err, ErrConflict) {
		t.Fatalf("Product selected suspended plugin: %v", err)
	}
	if _, err := env.store.HeartbeatPlatformServiceInstance(ctx, "third-party", "plugin-1", "1", true, pluginPrincipal, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.RegisterPlatformServiceInstance(ctx, PlatformServiceRegistration{RequestID: "plugin-start", ServiceID: "third-party", InstanceID: "plugin-1", ManifestVersion: "1", ProtocolVersion: "1", EndpointRef: "plugin", Ready: true, Options: []PlatformServiceOption{{Code: "third_party_option", DisplayName: "Third-party option", Requires: []string{"mqtt"}}}}, pluginPrincipal, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	catalog, err = env.store.ListPlatformServiceOptions(ctx, environment, now.Add(2*time.Second))
	if err != nil || catalog.Options[1].Selectable {
		t.Fatalf("heartbeat/re-registration reactivated plugin: %+v %v", catalog, err)
	}
	if _, err := env.store.SetPlatformServiceStatus(ctx, environment, "third-party", "active", owner.User.ID); err != nil {
		t.Fatal(err)
	}
	catalog, err = env.store.ListPlatformServiceOptions(ctx, environment, now.Add(2*time.Second))
	if err != nil || !catalog.Options[1].Selectable {
		t.Fatalf("activated plugin unavailable: %+v %v", catalog, err)
	}
	create.CatalogRevision = catalog.CatalogRevision
	profile, err := env.store.CreateDeviceItemProfileAsUser(ctx, create)
	if err != nil || !serviceOptionSetsEqual(profile.ServiceOptions, create.ServiceOptions) {
		t.Fatalf("registered option Product = %+v %v", profile, err)
	}
	readyDevicePKIFixture(t, env, profile.ID)
	var issuedOptions []string
	run, _, err := env.store.IssueProductionRunAsUser(ctx, authorizedProductionInput(owner.User.ID, owner.BrandCloud.ID, profile.ID), func(_ model.ProductionRun, p model.DeviceItemProfile) (string, error) {
		issuedOptions = slices.Clone(p.ServiceOptions)
		return "signed-fixture", nil
	})
	if err != nil || run.ProductServiceRevision == nil || run.ServiceGrantSHA256 == "" || !serviceOptionSetsEqual(issuedOptions, create.ServiceOptions) {
		t.Fatalf("plugin production grant = %+v, options=%v, error=%v", run, issuedOptions, err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE users SET platform_admin=true WHERE id=$1`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	claimInput := platformTokenInput(owner.User.ID, owner.BrandCloud.ID, "plugin-claim", &profile.ID)
	claimInput.ServiceOptions = nil
	claim, err := env.store.CreateDeviceClaimTokenAsPlatform(ctx, claimInput)
	if err != nil || !serviceOptionSetsEqual(claim.ServiceOptions, create.ServiceOptions) || claim.Metadata["product_service_revision"] == nil {
		t.Fatalf("plugin claim token = %+v %v", claim, err)
	}
}

func TestLegacyShadowOnlyProductMetadataEditDoesNotRequireLiveCatalog(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	owner := handoffDeveloper(t, env, "service-legacy-shadow")
	create := authorizedProductInput(owner.User.ID, owner.BrandCloud.ID, "legacy-shadow-only")
	create.ServiceOptions = []string{"iot_shadow"}
	profile, err := env.store.CreateDeviceItemProfile(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	env.store.ConfigurePlatformServiceProductWrites("integration-empty-catalog", true)
	name := "Renamed legacy Shadow Product"
	updated, err := env.store.UpdateDeviceItemProfileAsUser(ctx, DeviceItemProfileUpdateInput{
		ActorUserID: &owner.User.ID, BrandCloudID: owner.BrandCloud.ID,
		ProfileID: profile.ID, DisplayName: &name,
	})
	if err != nil || updated.DisplayName != name || !slices.Equal(updated.ServiceOptions, []string{"iot_shadow"}) {
		t.Fatalf("legacy metadata edit changed grants: %+v %v", updated, err)
	}
}

func TestServicePublicationRequiresReadyRevisionAndDoesNotRollBackOnOldHeartbeat(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	environment := "integration-publication-" + strconv.FormatInt(now.UnixNano(), 10)
	cleanupServiceEnvironment(t, env, environment)
	owner := handoffDeveloper(t, env, "service-publication")
	principal := PlatformServicePrincipal{Environment: environment, CertificateSubject: "service:mqtt", IssuerFingerprint: strings.Repeat("c", 64)}
	if err := env.store.ApprovePlatformServiceWorkload(ctx, PlatformServiceWorkloadApproval{Environment: environment, ServiceID: "mqtt", InstanceID: "mqtt-1", CertificateSubject: principal.CertificateSubject, IssuerFingerprint: principal.IssuerFingerprint, AllowedOptionCodes: []string{"mqtt"}, ApprovedBy: owner.User.ID}); err != nil {
		t.Fatal(err)
	}
	manifest := PlatformServiceRegistration{RequestID: "mqtt-v1", ServiceID: "mqtt", InstanceID: "mqtt-1", ManifestVersion: "1", ProtocolVersion: "1", EndpointRef: "mqtt", Ready: true,
		Options: []PlatformServiceOption{{Code: "mqtt", DisplayName: "MQTT"}}}
	if _, err := env.store.RegisterPlatformServiceInstance(ctx, manifest, principal, now); err != nil {
		t.Fatal(err)
	}
	oldReplica := manifest
	oldReplica.Options = slices.Clone(manifest.Options)
	oldReplica.RequestID = "mqtt-v1-replica"
	oldReplica.InstanceID = "mqtt-2"
	oldPrincipal := PlatformServicePrincipal{Environment: environment, CertificateSubject: "service:mqtt-2", IssuerFingerprint: principal.IssuerFingerprint}
	if err := env.store.ApprovePlatformServiceWorkload(ctx, PlatformServiceWorkloadApproval{Environment: environment, ServiceID: "mqtt", InstanceID: "mqtt-2", CertificateSubject: oldPrincipal.CertificateSubject, IssuerFingerprint: oldPrincipal.IssuerFingerprint, AllowedOptionCodes: []string{"mqtt"}, ApprovedBy: owner.User.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.RegisterPlatformServiceInstance(ctx, oldReplica, oldPrincipal, now); err != nil {
		t.Fatal(err)
	}
	manifest.Options[0].DisplayName = "MQTT changed"
	if _, err := env.store.RegisterPlatformServiceInstance(ctx, manifest, principal, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("request-id replay with different manifest = %v", err)
	}
	manifest.RequestID = "mqtt-v2"
	manifest.ManifestVersion = "2"
	manifest.Ready = false
	if _, err := env.store.RegisterPlatformServiceInstance(ctx, manifest, principal, now); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PublishPlatformServiceManifest(ctx, "mqtt", "1", "2", principal, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("unready version published: %v", err)
	}
	if _, err := env.store.HeartbeatPlatformServiceInstance(ctx, "mqtt", "mqtt-1", "2", true, principal, now); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PublishPlatformServiceManifest(ctx, "mqtt", "1", "2", principal, now); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.HeartbeatPlatformServiceInstance(ctx, "mqtt", "mqtt-2", "1", true, oldPrincipal, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PublishPlatformServiceManifest(ctx, "mqtt", "1", "2", principal, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale expected version published: %v", err)
	}
	catalog, err := env.store.ListPlatformServiceOptions(ctx, environment, now)
	if err != nil || len(catalog.Options) != 1 || catalog.Options[0].ManifestVersion != "2" || !catalog.Options[0].Selectable {
		t.Fatalf("published catalog = %+v %v", catalog, err)
	}
	if _, err := env.store.SetPlatformServiceStatus(ctx, environment, "mqtt", "suspended", owner.User.ID); err != nil {
		t.Fatal(err)
	}
	suspended, err := env.store.ListPlatformServiceOptions(ctx, environment, now)
	if err != nil || suspended.Options[0].Selectable {
		t.Fatalf("suspended service selectable: %+v %v", suspended, err)
	}
	if _, err := env.store.SetPlatformServiceStatus(ctx, environment, "mqtt", "active", owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RevokePlatformServiceWorkload(ctx, PlatformServiceWorkloadRevocation{Environment: environment, RevokedBy: owner.User.ID, ServiceID: "mqtt", InstanceID: "mqtt-1", CertificateSubject: principal.CertificateSubject, IssuerFingerprint: principal.IssuerFingerprint}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.HeartbeatPlatformServiceInstance(ctx, "mqtt", "mqtt-1", "2", true, principal, now.Add(3*time.Second)); !errors.Is(err, ErrServiceRegistrationDenied) {
		t.Fatalf("revoked workload heartbeat = %v", err)
	}
	revoked, err := env.store.ListPlatformServiceOptions(ctx, environment, now.Add(3*time.Second))
	if err != nil || revoked.Options[0].Selectable {
		t.Fatalf("revoked workload kept published version available: %+v %v", revoked, err)
	}
	expired, err := env.store.ListPlatformServiceOptions(ctx, environment, now.Add(PlatformServiceLeaseDuration+time.Second))
	if err != nil || len(expired.Options) != 1 || expired.Options[0].Selectable {
		t.Fatalf("expired instance remained selectable: %+v %v", expired, err)
	}
}

func cleanupServiceEnvironment(t *testing.T, env storeIntegrationEnv, environment string) {
	t.Helper()
	t.Cleanup(func() {
		for _, table := range []string{"platform_service_instances", "platform_service_workloads", "platform_services", "platform_service_manifests", "platform_service_registration_requests", "platform_service_catalog_revisions"} {
			if _, err := env.db.Exec(context.Background(), "DELETE FROM "+table+" WHERE environment=$1", environment); err != nil {
				t.Errorf("clean service registry test environment: %v", err)
			}
		}
	})
}
