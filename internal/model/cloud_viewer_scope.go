package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
)

var ErrInvalidCloudViewerScope = errors.New("viewer scope must select unique Product UUIDs or all Products")
var cloudProductUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type CloudViewerScope struct {
	Kind       string   `json:"kind"`
	ProductIDs []string `json:"product_ids,omitempty"`
}

func (s CloudViewerScope) Normalized() (CloudViewerScope, error) {
	if s.Kind == "all_products" && s.ProductIDs == nil {
		return s, nil
	}
	if s.Kind != "selected_products" || len(s.ProductIDs) == 0 {
		return CloudViewerScope{}, ErrInvalidCloudViewerScope
	}
	s.ProductIDs = append([]string(nil), s.ProductIDs...)
	for i, id := range s.ProductIDs {
		if !cloudProductUUID.MatchString(id) {
			return CloudViewerScope{}, ErrInvalidCloudViewerScope
		}
		s.ProductIDs[i] = strings.ToLower(id)
	}
	sort.Strings(s.ProductIDs)
	for i := 1; i < len(s.ProductIDs); i++ {
		if s.ProductIDs[i] == s.ProductIDs[i-1] {
			return CloudViewerScope{}, ErrInvalidCloudViewerScope
		}
	}
	return s, nil
}

func (s *CloudViewerScope) UnmarshalJSON(data []byte) error {
	type wire CloudViewerScope
	var value wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for field := range fields {
		if field != "kind" && field != "product_ids" {
			return ErrInvalidCloudViewerScope
		}
	}
	if value.Kind == "all_products" {
		if _, present := fields["product_ids"]; present {
			return ErrInvalidCloudViewerScope
		}
	}
	normalized, err := CloudViewerScope(value).Normalized()
	if err != nil {
		return err
	}
	*s = normalized
	return nil
}
