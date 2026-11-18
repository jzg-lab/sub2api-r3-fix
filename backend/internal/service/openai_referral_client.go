package service

import "context"

type OpenAIReferralCall struct {
	ProxyURL  string
	Headers   map[string]string
	ProgramID string
}

type OpenAIReferralClient interface {
	QueryEligibility(context.Context, OpenAIReferralCall) (*OpenAIReferralEligibility, error)
	SendInvite(context.Context, OpenAIReferralCall, string) error
}
