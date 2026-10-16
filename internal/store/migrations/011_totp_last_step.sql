-- Last accepted TOTP time step; a code is single-use because steps only move forward.
ALTER TABLE users ADD COLUMN totp_last_step BIGINT NOT NULL DEFAULT 0;
