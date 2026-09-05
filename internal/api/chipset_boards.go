package api

import (
	"fmt"
	"regexp"
	"rtk_account_manager/internal/model"
	"strings"
)

// Models are curated, self-contained assets served by Cloud Admin. Providers
// cannot make the viewer execute scripts or fetch arbitrary remote models.
var boardModelPath = regexp.MustCompile(`^/assets/boards/[a-z0-9-]+/v[0-9]+/[a-z0-9-]+\.glb$`)
var boardPosterPath = regexp.MustCompile(`^/assets/boards/[a-z0-9-]+/v[0-9]+/[a-z0-9-]+\.(webp|png)$`)

func normalizeChipsetBoards(boards []model.ChipsetBoard, releases []model.ChipsetSDKRelease) error {
	invalid := func(detail string) error { return fmt.Errorf("%w: %s", errChipsetManifestInvalid, detail) }
	if len(boards) > 64 {
		return invalid("too many boards")
	}
	keys := map[string]bool{}
	for i := range boards {
		b := &boards[i]
		b.BoardKey, b.Name, b.Vendor = strings.TrimSpace(b.BoardKey), strings.TrimSpace(b.Name), strings.TrimSpace(b.Vendor)
		if !chipsetPackageKeyPattern.MatchString(b.BoardKey) || len(b.BoardKey) > 200 || keys[b.BoardKey] || b.Name == "" || len(b.Name) > 200 || b.Vendor == "" || len(b.Vendor) > 200 || len(b.Summary) > 4000 {
			return invalid("invalid or duplicate board identity")
		}
		keys[b.BoardKey] = true
		if d := b.Dimensions; d != nil && (d.LengthMM <= 0 || d.WidthMM <= 0 || d.LengthMM > 10000 || d.WidthMM > 10000) {
			return invalid("invalid board dimensions")
		}
		if len(b.Specs) > 64 || len(b.Components) > 64 || len(b.Resources) > 64 {
			return invalid("too many board details")
		}
		for _, spec := range b.Specs {
			if blank(spec.Label) || blank(spec.Value) || len(spec.Label) > 200 || len(spec.Value) > 1000 {
				return invalid("invalid board specification")
			}
		}
		if err := normalizeChipsetLinks(b.Resources); err != nil {
			return err
		}
		if m := b.Model; m != nil && (!boardModelPath.MatchString(m.AssetPath) || !boardPosterPath.MatchString(m.PosterPath) || blank(m.Note) || len(m.Note) > 1000) {
			return invalid("invalid board model asset or note")
		}
		components := map[string]bool{}
		for _, c := range b.Components {
			if !chipsetPackageKeyPattern.MatchString(c.Key) || len(c.Key) > 200 || components[c.Key] || blank(c.Name) || len(c.Name) > 200 || blank(c.Description) || len(c.Description) > 1000 {
				return invalid("invalid or duplicate board component")
			}
			components[c.Key] = true
		}
	}
	for _, release := range releases {
		if len(release.SupportedBoardKeys) > 64 {
			return invalid("too many SDK board references")
		}
		seen := map[string]bool{}
		for _, key := range release.SupportedBoardKeys {
			if !keys[key] || seen[key] {
				return invalid("unknown or duplicate SDK board reference")
			}
			seen[key] = true
		}
	}
	return nil
}
