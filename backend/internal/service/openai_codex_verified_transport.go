package service

import "github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"

// Both native captures have identical handshake parameters. A version must be
// qualified with its transport before it can become an outbound identity.
func IsVerifiedCodexClientVersion(version string) bool {
	return version == codexCLIVersion || version == "0.151.0"
}

func verifiedCodexTLSProfile() *tlsfingerprint.Profile {
	return &tlsfingerprint.Profile{
		Name:                "codex-exec-" + codexCLIVersion + "-macos",
		CipherSuites:        []uint16{255, 49196, 49195, 49188, 49187, 49162, 49161, 49160, 49200, 49199, 49192, 49191, 49172, 49171, 49170, 157, 156, 61, 60, 53, 47, 10},
		Curves:              []uint16{23, 24, 25},
		PointFormats:        []uint16{0},
		SignatureAlgorithms: []uint16{1025, 513, 1281, 1537, 1027, 515, 1283, 1539},
		ALPNProtocols:       []string{},
		SupportedVersions:   []uint16{},
		KeyShareGroups:      []uint16{},
		PSKModes:            []uint16{},
		Extensions:          []uint16{0, 10, 11, 13, 5, 18, 23},
	}
}
