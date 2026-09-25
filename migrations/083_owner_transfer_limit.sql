ALTER TABLE organizations
    ADD COLUMN owner_transfer_limit INTEGER NOT NULL DEFAULT 3
    CONSTRAINT organizations_owner_transfer_limit_check CHECK (owner_transfer_limit BETWEEN 0 AND 200);
