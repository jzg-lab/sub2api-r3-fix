-- Owned by AccountTOTPService. Deliberately excluded from generic account DTOs,
-- credentials, exports and refresh writes. Existing accounts remain unchanged.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS totp_secret_encrypted TEXT;
