package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"rtk_account_manager/internal/model"
)

func validChipsetBoard() model.ChipsetBoard {
	return model.ChipsetBoard{BoardKey: "amb82-mini", Name: "AMB82 MINI", Vendor: "Realtek", Summary: "Camera board", Dimensions: &model.ChipsetBoardDimensions{LengthMM: 60, WidthMM: 37.4}, Specs: []model.ChipsetBoardSpec{{Label: "Header pitch", Value: "2.54 mm"}}, Resources: []model.ChipsetEndpoint{{Type: "documentation", Title: "Guide", URL: "https://example.com/guide", Source: "official", Languages: []string{"en"}, VerifiedAt: "2026-09-05T00:00:00Z"}}, Model: &model.ChipsetBoardModel{AssetPath: "/assets/boards/amb82-mini/v1/model.glb", PosterPath: "/assets/boards/amb82-mini/v1/poster.webp", Note: "Appearance model; dimensions approximate."}, Components: []model.ChipsetBoardComponent{{Key: "camera", Name: "F37 camera", Description: "Captures images."}}}
}

func TestChipsetBoardsRejectInvalidIdentityAssetsAndReferences(t *testing.T) {
	mutations := map[string]func(*model.ChipsetBoard){
		"empty key":                     func(b *model.ChipsetBoard) { b.BoardKey = "" },
		"invalid key":                   func(b *model.ChipsetBoard) { b.BoardKey = "../amb82" },
		"empty name":                    func(b *model.ChipsetBoard) { b.Name = " " },
		"empty vendor":                  func(b *model.ChipsetBoard) { b.Vendor = " " },
		"long summary":                  func(b *model.ChipsetBoard) { b.Summary = strings.Repeat("x", 4001) },
		"negative dimensions":           func(b *model.ChipsetBoard) { b.Dimensions.LengthMM = -1 },
		"excessive dimensions":          func(b *model.ChipsetBoard) { b.Dimensions.WidthMM = 10001 },
		"invalid spec":                  func(b *model.ChipsetBoard) { b.Specs[0].Value = "" },
		"too many specs":                func(b *model.ChipsetBoard) { b.Specs = make([]model.ChipsetBoardSpec, 65) },
		"too many components":           func(b *model.ChipsetBoard) { b.Components = make([]model.ChipsetBoardComponent, 65) },
		"too many links":                func(b *model.ChipsetBoard) { b.Resources = make([]model.ChipsetEndpoint, 65) },
		"invalid link":                  func(b *model.ChipsetBoard) { b.Resources[0].URL = "http://example.com/guide" },
		"remote model":                  func(b *model.ChipsetBoard) { b.Model.AssetPath = "https://example.com/model.glb" },
		"traversal model":               func(b *model.ChipsetBoard) { b.Model.AssetPath = "/assets/boards/../../model.glb" },
		"remote poster":                 func(b *model.ChipsetBoard) { b.Model.PosterPath = "//example.com/poster.webp" },
		"model note required":           func(b *model.ChipsetBoard) { b.Model.Note = "" },
		"invalid component key":         func(b *model.ChipsetBoard) { b.Components[0].Key = "Camera" },
		"duplicate component":           func(b *model.ChipsetBoard) { b.Components = append(b.Components, b.Components[0]) },
		"missing component description": func(b *model.ChipsetBoard) { b.Components[0].Description = "" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			board := validChipsetBoard()
			mutate(&board)
			if err := normalizeChipsetBoards([]model.ChipsetBoard{board}, nil); !errors.Is(err, errChipsetManifestInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for name, boards := range map[string][]model.ChipsetBoard{"duplicate key": {validChipsetBoard(), validChipsetBoard()}, "too many boards": make([]model.ChipsetBoard, 65)} {
		t.Run(name, func(t *testing.T) {
			if normalizeChipsetBoards(boards, nil) == nil {
				t.Fatal("invalid boards accepted")
			}
		})
	}
	for name, keys := range map[string][]string{"unknown": {"missing"}, "duplicate": {"amb82-mini", "amb82-mini"}, "too many": make([]string, 65)} {
		t.Run(name, func(t *testing.T) {
			if normalizeChipsetBoards([]model.ChipsetBoard{validChipsetBoard()}, []model.ChipsetSDKRelease{{SupportedBoardKeys: keys}}) == nil {
				t.Fatal("invalid references accepted")
			}
		})
	}
}

func TestChipsetBoardManifestSupportsSharedSDKAndLegacy(t *testing.T) {
	board := validChipsetBoard()
	second := validChipsetBoard()
	second.BoardKey = "another-board"
	second.Model = nil
	second.Dimensions = nil
	for name, boards := range map[string][]model.ChipsetBoard{"shared SDK": {board, second}, "legacy": nil} {
		t.Run(name, func(t *testing.T) {
			keys := []string{}
			for _, b := range boards {
				keys = append(keys, b.BoardKey)
			}
			source := model.DeveloperChipset{ChipsetKey: "pro2", Name: "AmebaPRO2", Vendor: "Realtek", ICModel: " RTL8735B ", Boards: boards, Resources: board.Resources, SDKReleases: []model.ChipsetSDKRelease{{Name: "SDK", Version: "1", SupportedModels: []string{"AMB82 MINI"}, SupportedBoardKeys: keys, Endpoints: []model.ChipsetEndpoint{}}}}
			chipsetJSON, _ := json.Marshal(source)
			raw := []byte(fmt.Sprintf(`{"$schema":"schema.json","manifest_version":"1","provider":{"name":"Realtek","updated_at":"2026-09-05T00:00:00Z"},"chipsets":[%s]}`, chipsetJSON))
			chipsets, _, err := parseChipsetManifest("provider-one", raw)
			if err != nil {
				t.Fatal(err)
			}
			if chipsets[0].ICModel != "RTL8735B" || len(chipsets[0].Boards) != len(boards) || len(chipsets[0].SDKReleases[0].SupportedBoardKeys) != len(keys) {
				t.Fatalf("lost board data: %#v", chipsets)
			}
			other, _, err := parseChipsetManifest("provider-two", raw)
			if err != nil || other[0].ID == chipsets[0].ID {
				t.Fatal("same named boards from different providers must have distinct chipset IDs")
			}
			if len(boards) > 0 {
				// Provider package governance also covers links nested in boards.
				if err := ValidateChipsetResourcePackage(raw); err != nil {
					t.Fatal(err)
				}
				bad := strings.Replace(string(raw), `"source":"official"`, `"source":"unknown"`, 1)
				if err := ValidateChipsetResourcePackage([]byte(bad)); err == nil {
					t.Fatal("board link governance was skipped")
				}
				bad = strings.Replace(string(raw), `"board_key":"another-board"`, `"board_key":"amb82-mini"`, 1)
				if _, _, err := parseChipsetManifest("provider", []byte(bad)); err == nil {
					t.Fatal("parser accepted duplicate board key")
				}
			}
		})
	}
	if err := normalizeChipsetBoards(nil, nil); err != nil {
		t.Fatal(err)
	}
	trimmed := validChipsetBoard()
	trimmed.BoardKey = " amb82-mini "
	if err := normalizeChipsetBoards([]model.ChipsetBoard{trimmed}, nil); err != nil {
		t.Fatal(err)
	}
}
