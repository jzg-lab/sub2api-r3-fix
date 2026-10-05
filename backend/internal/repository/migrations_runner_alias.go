package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
)

type migrationFilenameAlias struct {
	filename string
	checksum string
}

// Only these published r3 migrations have verified, byte-equivalent upstream names.
var migrationFilenameAliases = map[string]migrationFilenameAlias{
	"238_scheduler_account_revisions.sql":     {"r3_0238_scheduler_account_revisions.sql", "c2457f9b58c2fdba821af13a52566c671b71901fa29d5028512ed610beff4210"},
	"239_openai_probe_ownership.sql":          {"r3_0239_openai_probe_ownership.sql", "92745e53f0b52aa095ba4dbbffb92ac1f5a6878f513929461cdc95d3934a6637"},
	"240_openai_probe_rate_limit_streak.sql":  {"r3_0240_openai_probe_rate_limit_streak.sql", "4b21041c4018bc36fa1aa052b5a2a9f167514e987c31c3614f35e6ae71682a01"},
	"241_openai_probe_turn_state_len.sql":     {"r3_0241_openai_probe_turn_state_len.sql", "41fd07cdcd59d1de2400e1c7095300d8e3290d6f6a8e45cb9e00bc8008592156"},
	"242_openai_codex_tickets.sql":            {"r3_0242_openai_codex_tickets.sql", "5105aeebb8eba55fc7188a57ebd51bd9ae1128c6ae0a9414deafe8b039589bd1"},
	"243_openai_probe_retention_indexes.sql":  {"r3_0243_openai_probe_retention_indexes.sql", "1501d49320aa9cbf53eb4ee203b2421ffcda6c8d5cabe208aeff447619d91b0e"},
	"244_openai_probe_harvest_mode.sql":       {"r3_0244_openai_probe_harvest_mode.sql", "ec2deec85b9fa5a1124ebfd2cb32d268a4e88f40e293a84aa6bf611b1cd5e358"},
	"245_openai_codex_ticket_cookie_pair.sql": {"r3_0245_openai_codex_ticket_cookie_pair.sql", "1ceb863bab7d25cf04e882592057d331516ee905a3d16bac25a08db0914a4364"},
	"246_openai_probe_nullable_verdict.sql":   {"r3_0246_openai_probe_nullable_verdict.sql", "fe7be52ff2cd118df3e3b885158dbd2095eab8240046d02e81e60982f2095743"},
}

func migrationAppliedUnderAlias(ctx context.Context, conn *sql.Conn, name, checksum string) (bool, error) {
	alias, ok := migrationFilenameAliases[name]
	if !ok {
		return false, nil
	}
	var existing string
	err := conn.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE filename = $1", alias.filename).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check migration alias %s for %s: %w", alias.filename, name, err)
	}
	if existing != alias.checksum || checksum != alias.checksum {
		return false, fmt.Errorf("migration %s alias %s checksum mismatch (db=%s file=%s expected=%s)", name, alias.filename, existing, checksum, alias.checksum)
	}
	log.Printf("migration %s already applied as %s; preserving historical record", name, alias.filename)
	return true, nil
}
