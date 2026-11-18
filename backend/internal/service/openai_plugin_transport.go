package service

import "net/http"

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
	s.httpUpstream = withCaptureUpstream(s.httpUpstream)
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
func (s *OpenAIGatewayService) roundTripOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if err := validateOpenAIAccountProxyRoute(account, proxyURL); err != nil {
		return nil, err
	}
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			captureHTTPResponse(request, response, err)
			return response, err
		}
	}
	return s.doHTTPUpstream(request, proxyURL, account)
}

func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	return s.doOpenAIUpstreamWithContext(request.Context(), request, proxyURL, account)
}

// doOpenAIAccountTestUpstream 让 OpenAI OAuth 账号测试与真实转发使用同一插件路径。
// API Key 和未命中插件的账号保持各自原有的 HTTPUpstream 行为。
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
) (*http.Response, error) {
	roundTrip := func(req *http.Request, route string, current *Account) (*http.Response, error) {
		if s.pluginManager != nil {
			response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(req.Context(), req, route, current)
			if handled {
				return response, err
			}
		}
		if useTLSFallback {
			return s.httpUpstream.DoWithTLS(req, route, current.ID, current.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(current))
		}
		return s.httpUpstream.Do(req, route, current.ID, current.Concurrency)
	}
	return doOpenAIUpstreamWithRecovery(request.Context(), request, proxyURL, account, s.openAITokenProvider, true, roundTrip)
}
