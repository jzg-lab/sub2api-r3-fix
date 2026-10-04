package admin

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (h *AccountHandler) consumeInitialAuthorizationProof(ctx context.Context, req *CreateAccountRequest) (context.Context, error) {
	return h.openaiOAuthService.ConsumeInitialAuthorizationProof(ctx, req.InitialAuthorizationProof, &service.Account{
		Platform: req.Platform, Type: req.Type, Credentials: req.Credentials, ProxyID: req.ProxyID,
	})
}
