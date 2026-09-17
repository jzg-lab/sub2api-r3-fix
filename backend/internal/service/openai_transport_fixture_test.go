package service

func openAITransportTestProxyID() *int64 {
	id := int64(1)
	return &id
}

// Only positive transport fixtures use this route. Negative route tests
// construct their own assignments and must never be repaired by a stub.
func openAITransportTestProxy() *Proxy {
	return &Proxy{ID: 1, Protocol: "http", Host: "127.0.0.1", Port: 18080, Status: StatusActive}
}

func openAITransportTestRoute(account *Account) string {
	if account == nil || account.Proxy == nil {
		return ""
	}
	return account.Proxy.URL()
}

func assignOpenAITransportTestProxy(account *Account) {
	if account != nil && account.IsOpenAIOAuth() &&
		!account.IsOpenAIPersonalAccessToken() && !account.IsOpenAIAgentIdentity() {
		account.Proxy = openAITransportTestProxy()
		account.ProxyID = &account.Proxy.ID
	}
}
