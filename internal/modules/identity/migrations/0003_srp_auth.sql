ALTER TABLE identity.auth
    ADD COLUMN srp_salt BYTEA,
    ADD COLUMN srp_verifier BYTEA,
    ADD CONSTRAINT auth_srp_credentials_pair_check
        CHECK ((srp_salt IS NULL) = (srp_verifier IS NULL));

CREATE TABLE identity.auth_challenges (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES identity.users (id) ON DELETE CASCADE,
    server_state BYTEA NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX auth_challenges_expires_at_idx ON identity.auth_challenges (expires_at);
