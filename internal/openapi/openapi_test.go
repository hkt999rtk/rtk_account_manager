package openapi_test

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestOpenAPIContractIsValid(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAPIOperationsHaveCompleteMetadata(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}

	declaredTags := map[string]struct{}{}
	for _, tag := range doc.Tags {
		declaredTags[tag.Name] = struct{}{}
	}
	operationIDs := map[string]string{}
	for path, item := range doc.Paths.Map() {
		for method, operation := range item.Operations() {
			location := method + " " + path
			if operation.Summary == "" {
				t.Fatalf("%s is missing summary", location)
			}
			if operation.OperationID == "" {
				t.Fatalf("%s is missing operationId", location)
			}
			if previous := operationIDs[operation.OperationID]; previous != "" {
				t.Fatalf("duplicate operationId %q on %s and %s", operation.OperationID, previous, location)
			}
			operationIDs[operation.OperationID] = location
			if len(operation.Tags) == 0 {
				t.Fatalf("%s is missing tags", location)
			}
			for _, tag := range operation.Tags {
				if _, ok := declaredTags[tag]; !ok {
					t.Fatalf("%s references undeclared tag %q", location, tag)
				}
			}
			if !hasSuccessfulOrRedirectResponse(operation) {
				t.Fatalf("%s is missing a 2xx/3xx response", location)
			}
		}
	}
	if len(operationIDs) == 0 {
		t.Fatal("expected documented OpenAPI operations")
	}
}

func hasSuccessfulOrRedirectResponse(operation *openapi3.Operation) bool {
	for status := range operation.Responses.Map() {
		if len(status) == 3 && (status[0] == '2' || status[0] == '3') {
			return true
		}
	}
	return false
}
