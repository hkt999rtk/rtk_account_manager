package store

import (
	"encoding/json"
	"reflect"
	"rtk_account_manager/internal/model"
	"testing"
	"time"
)

func TestChipsetSnapshotRetainsBoardsAndSharedSDKs(t *testing.T) {
	board := model.ChipsetBoard{BoardKey: "amb82-mini", Name: "AMB82 MINI", Vendor: "Realtek", Summary: "Camera board", Dimensions: &model.ChipsetBoardDimensions{LengthMM: 60, WidthMM: 37.4}, Specs: []model.ChipsetBoardSpec{{Label: "Pitch", Value: "2.54 mm"}}, Resources: []model.ChipsetEndpoint{{Type: "documentation", Title: "Guide", URL: "https://example.com/guide", Metadata: map[string]any{"revision": "0.3"}}}, Model: &model.ChipsetBoardModel{AssetPath: "/assets/boards/amb82-mini/v1/model.glb", PosterPath: "/assets/boards/amb82-mini/v1/poster.webp", Note: "Approximate"}, Components: []model.ChipsetBoardComponent{{Key: "camera", Name: "Camera", Description: "F37"}}}
	source := []model.DeveloperChipset{{ID: "chipset", ICModel: "RTL8735B", Boards: []model.ChipsetBoard{board}, SDKReleases: []model.ChipsetSDKRelease{{Name: "SDK", SupportedBoardKeys: []string{"amb82-mini", "another-board"}, SupportedModels: []string{"legacy"}}}}}
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	provider := model.ChipsetProvider{ID: "provider", Name: "Realtek", Stale: true, LastSuccessfulRefreshAt: &now}
	got, err := decodeChipsetSnapshot(provider, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[0].Boards, source[0].Boards) || !reflect.DeepEqual(got[0].SDKReleases, source[0].SDKReleases) || got[0].ICModel != "RTL8735B" {
		t.Fatalf("snapshot lost fields: %#v", got)
	}
	if !got[0].Stale || got[0].ProviderID != "provider" || got[0].LastSuccessfulRefreshAt != now {
		t.Fatal("board snapshot lost provider lifecycle metadata")
	}
}
