package store

import (
	"context"
	"testing"
	"time"

	"rtk_account_manager/internal/model"
)

func TestChipsetProviderSnapshotLifecycle(t *testing.T) {
	env := newStoreIntegrationEnv(t)
	ctx := context.Background()

	provider, err := env.store.CreateChipsetProvider(ctx, ChipsetProviderWriteInput{
		Name:        "Ameba IoT",
		ManifestURL: "https://provider.example.com/amebapro2.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.Status != model.ChipsetProviderStatusDraft || !provider.Unavailable {
		t.Fatalf("unexpected draft provider: %+v", provider)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	provider, err = env.store.CommitChipsetProviderRefresh(ctx, ChipsetProviderRefreshInput{
		ProviderID:      provider.ID,
		ManifestVersion: "1",
		ManifestSHA256:  "d34db33f",
		ETag:            `"amebapro2-v1"`,
		Chipsets: []model.DeveloperChipset{{
			ID:         "74c1b648-d0d9-5c45-84d8-5b35448328d8",
			ChipsetKey: "realtek-amebapro2",
			Vendor:     "Realtek",
			Name:       "AmebaPro2",
			SDKReleases: []model.ChipsetSDKRelease{{
				Name: "Ameba Arduino Pro2", Version: "main", Recommended: true,
				SupportedModels: []string{}, Endpoints: []model.ChipsetEndpoint{},
			}},
		}},
		AttemptedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.Unavailable || provider.ChipsetCount != 1 || provider.SDKReleaseCount != 1 {
		t.Fatalf("unexpected refreshed provider: %+v", provider)
	}
	provider, err = env.store.SetChipsetProviderStatus(ctx, provider.ID, model.ChipsetProviderStatusPublished)
	if err != nil {
		t.Fatal(err)
	}
	chipsets, err := env.store.ListPublishedChipsets(ctx)
	if err != nil || len(chipsets) != 1 || chipsets[0].ProviderName != "Ameba IoT" {
		t.Fatalf("published chipsets = %+v, %v", chipsets, err)
	}

	provider, err = env.store.MarkChipsetProviderRefreshFailed(ctx, provider.ID, "provider fetch failed", now.Add(time.Minute))
	if err != nil || !provider.Stale {
		t.Fatalf("stale provider = %+v, %v", provider, err)
	}
	chipsets, err = env.store.ListPublishedChipsets(ctx)
	if err != nil || len(chipsets) != 1 || !chipsets[0].Stale {
		t.Fatalf("last-known-good chipsets = %+v, %v", chipsets, err)
	}

	if _, err := env.store.SetChipsetProviderStatus(ctx, provider.ID, model.ChipsetProviderStatusUnpublished); err != nil {
		t.Fatal(err)
	}
	chipsets, err = env.store.ListPublishedChipsets(ctx)
	if err != nil || len(chipsets) != 0 {
		t.Fatalf("unpublished chipsets = %+v, %v", chipsets, err)
	}
}
