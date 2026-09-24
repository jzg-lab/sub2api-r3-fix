package claude

import "sync/atomic"

type cliVersionResolverFunc func() string

var cliVersionResolver atomic.Pointer[cliVersionResolverFunc]

func SetCLIVersionResolver(resolver func() string) {
	if resolver == nil {
		cliVersionResolver.Store(nil)
		return
	}
	r := cliVersionResolverFunc(resolver)
	cliVersionResolver.Store(&r)
}

func EffectiveCLIVersion() string {
	if r := cliVersionResolver.Load(); r != nil {
		if version := (*r)(); IsSupportedCLIVersion(version) {
			return version
		}
	}
	return CLIVersion()
}

func DefaultUserAgent() string {
	return "claude-cli/" + EffectiveCLIVersion() + " (external, cli)"
}
