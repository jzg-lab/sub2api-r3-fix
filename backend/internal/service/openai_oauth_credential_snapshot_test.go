package service

import "time"

func openAIOAuthAccountEditFixture() *Account {
	return &Account{
		ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		UpdatedAt: time.Now().UTC(),
		Credentials: map[string]any{
			"access_token": "fixture-before", "email": "user@example.com",
			"client_id": "fixture-client", "chatgpt_account_id": "fixture-account",
			"model_mapping": map[string]any{"fixture-model": "fixture-model"},
		},
	}
}
