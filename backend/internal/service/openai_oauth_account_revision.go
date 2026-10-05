package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

const openAIOAuthAccountRevisionPrefix = "oauth-v1:"

// OpenAIOAuthAccountRevision binds authorization identity, not runtime health.
// The admin DTO exposes only this digest, never the credential snapshot.
func OpenAIOAuthAccountRevision(account *Account) string {
	if !IsOpenAIBrowserOAuthAccount(account) {
		return ""
	}
	snapshot := struct {
		ID          int64          `json:"id"`
		Platform    string         `json:"platform"`
		Type        string         `json:"type"`
		ParentID    *int64         `json:"parent_id"`
		ProxyID     *int64         `json:"proxy_id"`
		Credentials map[string]any `json:"credentials"`
		Qualified   any            `json:"qualified_proxy"`
		LoginIP     any            `json:"login_ip"`
	}{
		ID: account.ID, Platform: account.Platform, Type: account.Type,
		ParentID: account.ParentAccountID, ProxyID: account.ProxyID,
		Credentials: account.Credentials,
		Qualified:   account.Extra[OpenAIOAuthQualifiedProxyExtraKey],
		LoginIP:     account.Extra[OpenAIOAuthLoginExitIPExtraKey],
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(payload)
	return openAIOAuthAccountRevisionPrefix + hex.EncodeToString(hash[:])
}

func validOpenAIOAuthAccountRevision(revision string) bool {
	return strings.HasPrefix(revision, openAIOAuthAccountRevisionPrefix) &&
		validOpenAIAuthBrowserLowerHex(strings.TrimPrefix(revision, openAIOAuthAccountRevisionPrefix), 64)
}
