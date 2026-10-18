package service

import "context"

type gatewayGroupAccessKey struct{}

type gatewayGroupAccess struct {
	user User
}

// WithGatewayGroupAccess captures only the authenticated grants needed by
// fallback selection. Later group changes must not widen this request's scope.
func WithGatewayGroupAccess(ctx context.Context, apiKey *APIKey) context.Context {
	if _, bound := ctx.Value(gatewayGroupAccessKey{}).(gatewayGroupAccess); bound {
		return ctx
	}
	access := gatewayGroupAccess{}
	if apiKey != nil && apiKey.ID > 0 && apiKey.User != nil &&
		apiKey.User.ID > 0 && apiKey.UserID == apiKey.User.ID && apiKey.User.IsActive() &&
		apiKey.GroupID != nil && *apiKey.GroupID > 0 &&
		apiKey.Group != nil && apiKey.Group.ID == *apiKey.GroupID && apiKey.Group.IsActive() {
		access.user = User{
			ID:                   apiKey.User.ID,
			Status:               apiKey.User.Status,
			AllowedGroups:        append([]int64(nil), apiKey.User.AllowedGroups...),
			RestrictPublicGroups: apiKey.User.RestrictPublicGroups,
		}
	}
	return context.WithValue(ctx, gatewayGroupAccessKey{}, access)
}

// CanUseGatewayFallbackGroup applies the direct group access policy to every
// fallback hop. A different subscription group needs its own entitlement and
// billing check; a configured fallback edge alone cannot grant that access.
func CanUseGatewayFallbackGroup(ctx context.Context, group *Group) bool {
	if ctx == nil || group == nil || group.ID <= 0 || !group.IsActive() || group.IsSubscriptionType() {
		return false
	}
	access, ok := ctx.Value(gatewayGroupAccessKey{}).(gatewayGroupAccess)
	return ok && access.user.ID > 0 && access.user.IsActive() &&
		access.user.CanBindGroup(group.ID, group.IsExclusive)
}
