package service

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"
)

// An audit is evidence only. Shared import times or identifier prefixes do not
// establish weak entropy, and missing provenance must not be backdated.
type OpenAIEnvironmentAudit struct {
	Accounts      []OpenAIEnvironmentAuditAccount `json:"accounts"`
	Collisions    []OpenAIEnvironmentAuditGroup   `json:"collisions"`
	ImportBatches []OpenAIEnvironmentAuditGroup   `json:"import_batches"`
	GeneratedAt   time.Time                       `json:"generated_at"`
}

type OpenAIEnvironmentAuditAccount struct {
	AccountID           int64      `json:"account_id"`
	ConfiguredMode      string     `json:"configured_mode"`
	EffectiveMode       string     `json:"effective_mode"`
	SeedPresent         bool       `json:"seed_present"`
	InstallationDigest  string     `json:"installation_digest,omitempty"`
	IdentitySource      string     `json:"identity_source"`
	IdentityGeneratedAt *time.Time `json:"identity_generated_at"`
	AccountCreatedAt    time.Time  `json:"account_created_at"`
	LoginIPRecorded     bool       `json:"login_ip_recorded"`
	ProxyID             *int64     `json:"proxy_id"`
}

type OpenAIEnvironmentAuditGroup struct {
	Kind       string  `json:"kind"`
	AccountIDs []int64 `json:"account_ids"`
}

func environmentIdentifierDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func BuildOpenAIEnvironmentAudit(accounts []Account, now time.Time) OpenAIEnvironmentAudit {
	report := OpenAIEnvironmentAudit{
		Accounts:      make([]OpenAIEnvironmentAuditAccount, 0, len(accounts)),
		Collisions:    []OpenAIEnvironmentAuditGroup{},
		ImportBatches: []OpenAIEnvironmentAuditGroup{},
		GeneratedAt:   now.UTC(),
	}
	seeds := make(map[string][]int64)
	installations := make(map[string][]int64)
	imports := make(map[string][]int64)
	for i := range accounts {
		account := &accounts[i]
		if !account.IsOpenAIOAuth() {
			continue
		}
		seed, hasSeed := codexFingerprintSeed(account.Extra)
		_, loginIPErr := OpenAIOAuthLoginExitIP(account)
		entry := OpenAIEnvironmentAuditAccount{
			AccountID:        account.ID,
			ConfiguredMode:   string(account.GetCodexFingerprintMode()),
			EffectiveMode:    string(activeCodexFingerprintMode(account)),
			SeedPresent:      hasSeed,
			IdentitySource:   "unknown",
			AccountCreatedAt: account.CreatedAt,
			LoginIPRecorded:  loginIPErr == nil,
			ProxyID:          account.ProxyID,
		}
		if hasSeed {
			seeds[environmentIdentifierDigest(seed)] = append(seeds[environmentIdentifierDigest(seed)], account.ID)
		}
		if activeCodexFingerprintMode(account) != codexFingerprintOff {
			installation := resolveConvergedInstallationID(account, seed)
			entry.InstallationDigest = environmentIdentifierDigest(installation)
			entry.IdentitySource = "seed_derived"
			if account.GetOpenAIDeviceID() != "" {
				entry.IdentitySource = "configured_device"
			}
			installations[entry.InstallationDigest] = append(installations[entry.InstallationDigest], account.ID)
		}
		if !account.CreatedAt.IsZero() {
			batch := account.CreatedAt.UTC().Truncate(time.Minute).Format(time.RFC3339)
			imports[batch] = append(imports[batch], account.ID)
		}
		report.Accounts = append(report.Accounts, entry)
	}
	addGroups := func(groups map[string][]int64, kind string, target *[]OpenAIEnvironmentAuditGroup) {
		for _, ids := range groups {
			if len(ids) > 1 {
				sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
				*target = append(*target, OpenAIEnvironmentAuditGroup{Kind: kind, AccountIDs: ids})
			}
		}
	}
	addGroups(seeds, "duplicate_seed", &report.Collisions)
	addGroups(installations, "duplicate_effective_installation", &report.Collisions)
	addGroups(imports, "same_import_minute_only", &report.ImportBatches)
	sort.Slice(report.Accounts, func(i, j int) bool { return report.Accounts[i].AccountID < report.Accounts[j].AccountID })
	for _, groups := range [][]OpenAIEnvironmentAuditGroup{report.Collisions, report.ImportBatches} {
		sort.Slice(groups, func(i, j int) bool {
			if groups[i].Kind != groups[j].Kind {
				return groups[i].Kind < groups[j].Kind
			}
			return groups[i].AccountIDs[0] < groups[j].AccountIDs[0]
		})
	}
	return report
}
