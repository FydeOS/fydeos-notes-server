// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

func postCommerceQuery(t *testing.T, query string) (int, []byte) {
	t.Helper()
	handler := NewHandlerWithServices(config.Config{}, noSessionAuthenticator{}, nil, nil, nil, nil, nil, nil, HandlerDependencies{})
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response := newDeadlineTestResponseWriter()
	handler.ServeHTTP(response, request)
	return response.Code, response.Body.Bytes()
}

func TestPricesQueryReturnsEmptyList(t *testing.T) {
	code, body := postCommerceQuery(t, `query { prices { plan } }`)
	var result struct {
		Data struct {
			Prices []any `json:"prices"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode GraphQL response: %v", err)
	}
	if code != http.StatusOK || len(result.Errors) != 0 || result.Data.Prices == nil || len(result.Data.Prices) != 0 {
		t.Fatalf("response = %d %s, want empty prices without GraphQL errors", code, body)
	}
}

func TestCommerceMutationsReportFeatureNotAvailable(t *testing.T) {
	for name, query := range map[string]string{
		"createCheckoutSession":                 `mutation { createCheckoutSession(input: {plan: Pro, recurring: Monthly, successCallbackLink: "https://example.test"}) }`,
		"createCustomerPortal":                  `mutation { createCustomerPortal }`,
		"cancelSubscription":                    `mutation { cancelSubscription(plan: Pro) { id } }`,
		"resumeSubscription":                    `mutation { resumeSubscription(plan: Pro) { id } }`,
		"updateSubscriptionRecurring":           `mutation { updateSubscriptionRecurring(plan: Pro, recurring: Yearly) { id } }`,
		"activateLicense":                       `mutation { activateLicense(workspaceId: "w", license: "l") { quantity } }`,
		"deactivateLicense":                     `mutation { deactivateLicense(workspaceId: "w") }`,
		"createSelfhostWorkspaceCustomerPortal": `mutation { createSelfhostWorkspaceCustomerPortal(workspaceId: "w") }`,
	} {
		t.Run(name, func(t *testing.T) {
			_, body := postCommerceQuery(t, query)
			var result struct {
				Errors []struct {
					Message    string         `json:"message"`
					Extensions map[string]any `json:"extensions"`
				} `json:"errors"`
			}
			if err := json.Unmarshal(body, &result); err != nil {
				t.Fatalf("decode GraphQL response: %v", err)
			}
			if len(result.Errors) != 1 || result.Errors[0].Message != "This feature is not available on this server." ||
				result.Errors[0].Extensions["name"] != "feature_not_available" {
				t.Fatalf("response = %s, want feature_not_available error", body)
			}
		})
	}
}
