// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package entitlements

import (
	"errors"
	"testing"
)

func TestValidateAdminGrantMatchesCatalogAndTargetRules(t *testing.T) {
	quantity := int32(8)
	tests := []struct {
		name       string
		deployment string
		input      AdminGrantInput
		wantPlan   string
		wantQty    *int32
		wantReason string
	}{
		{
			name:       "workspace team defaults to one seat",
			deployment: "cloud",
			input:      AdminGrantInput{TargetType: "workspace", TargetID: "ws-1", Plan: "team"},
			wantPlan:   "team", wantQty: int32Pointer(1),
		},
		{
			name:       "workspace team preserves requested quantity",
			deployment: "cloud",
			input:      AdminGrantInput{TargetType: "workspace", TargetID: "ws-1", Plan: "team", Quantity: &quantity},
			wantPlan:   "team", wantQty: &quantity,
		},
		{
			name:       "non-team quantity is ignored",
			deployment: "cloud",
			input:      AdminGrantInput{TargetType: "user", TargetID: "user-1", Plan: "pro", Quantity: &quantity},
			wantPlan:   "pro",
		},
		{
			name:       "unknown plan",
			deployment: "cloud",
			input:      AdminGrantInput{TargetType: "user", TargetID: "user-1", Plan: "enterprise"},
			wantReason: "unknown entitlement plan",
		},
		{
			name:       "target plan mismatch",
			deployment: "cloud",
			input:      AdminGrantInput{TargetType: "user", TargetID: "user-1", Plan: "team"},
			wantReason: "entitlement plan is not configurable for target type",
		},
		{
			name:       "invalid team quantity",
			deployment: "cloud",
			input:      AdminGrantInput{TargetType: "workspace", TargetID: "ws-1", Plan: "team", Quantity: int32Pointer(100001)},
			wantReason: "quantity must be between 1 and 100000",
		},
		{
			name:       "self-hosted requires signed license",
			deployment: "selfhosted",
			input:      AdminGrantInput{TargetType: "workspace", TargetID: "ws-1", Plan: "team"},
			wantReason: "Self-hosted commercial entitlements require a signed license.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := validateAdminGrant(test.deployment, test.input)
			if test.wantReason != "" {
				var invalid *AdminGrantError
				if !errors.As(err, &invalid) || invalid.Reason != test.wantReason {
					t.Fatalf("error = %v, want reason %q", err, test.wantReason)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate admin grant: %v", err)
			}
			if got.Plan != test.wantPlan {
				t.Fatalf("plan = %q, want %q", got.Plan, test.wantPlan)
			}
			if (got.Quantity == nil) != (test.wantQty == nil) || got.Quantity != nil && *got.Quantity != *test.wantQty {
				t.Fatalf("quantity = %v, want %v", got.Quantity, test.wantQty)
			}
		})
	}
}

func TestAdminGrantIdentityMatchesNativeSubjectAndSourceLock(t *testing.T) {
	_, subject := adminGrantIdentity(AdminGrantInput{TargetType: "workspace", TargetID: "ws-1"})
	if subject != "admin_grant:workspace:ws-1" {
		t.Fatalf("subject id = %q", subject)
	}
}

func int32Pointer(value int32) *int32 { return &value }
