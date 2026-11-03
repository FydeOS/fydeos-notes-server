// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"fmt"

	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
)

func (r *Resolver) grantCommercialEntitlement(ctx context.Context, targetType, targetID, plan string, quantity *int) (bool, error) {
	if err := requireAdministrator(ctx); err != nil {
		return false, err
	}
	if r.AdminEntitlements == nil {
		return false, entitlements.ErrAdminGrantUnavailable
	}
	var grantQuantity *int32
	if quantity != nil {
		converted := int32(*quantity)
		grantQuantity = &converted
	}
	change, err := r.AdminEntitlements.Upsert(ctx, entitlements.AdminGrantInput{
		TargetType: targetType, TargetID: targetID, Plan: plan, Quantity: grantQuantity,
	})
	if err != nil {
		return false, adminGrantPublicError(err)
	}
	r.publishAdminEntitlementChange(change)
	return true, nil
}

func (r *Resolver) revokeCommercialEntitlement(ctx context.Context, targetType, targetID string) (bool, error) {
	if err := requireAdministrator(ctx); err != nil {
		return false, err
	}
	if r.AdminEntitlements == nil {
		return false, entitlements.ErrAdminGrantUnavailable
	}
	change, err := r.AdminEntitlements.Revoke(ctx, targetType, targetID)
	if err != nil {
		return false, adminGrantPublicError(err)
	}
	r.publishAdminEntitlementChange(change)
	return true, nil
}

func adminGrantPublicError(err error) error {
	var invalid *entitlements.AdminGrantError
	if errors.As(err, &invalid) {
		return err
	}
	return fmt.Errorf("%w: %v", entitlements.ErrAdminGrantUnavailable, err)
}

func (r *Resolver) publishAdminEntitlementChange(change entitlements.AdminGrantChange) {
	if change.TargetType == "workspace" {
		if r.LicenseWorkspacePublisher != nil {
			r.LicenseWorkspacePublisher.PublishWorkspaceEntitlementChanged(change.TargetID)
		}
		if r.LicenseQuotaPublisher != nil {
			for _, ownerID := range change.OwnerIDs {
				r.LicenseQuotaPublisher.PublishUserQuotaStateChanged(ownerID, "runtime-invalidation")
			}
		}
		return
	}
	if r.LicenseQuotaPublisher != nil {
		r.LicenseQuotaPublisher.PublishUserQuotaStateChanged(change.TargetID, "runtime-invalidation")
	}
}
