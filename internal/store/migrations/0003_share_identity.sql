-- Historical logins cannot prove a recipient after rename/reuse. Owners must
-- explicitly regrant legacy rows before they confer access.
CREATE TABLE shares_identity (
    id              TEXT    PRIMARY KEY,
    vault_id        INTEGER NOT NULL REFERENCES vaults (id) ON DELETE CASCADE,
    owner_user_id   INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    path            TEXT    NOT NULL,
    is_folder       INTEGER NOT NULL DEFAULT 0,
    grantee_login   TEXT    NOT NULL,
    perm            TEXT    NOT NULL CHECK (perm IN ('read', 'write')),
    created_at      INTEGER NOT NULL,
    grantee_user_id INTEGER REFERENCES users (id) ON DELETE CASCADE
);

INSERT INTO shares_identity (id, vault_id, owner_user_id, path, is_folder, grantee_login, perm, created_at)
SELECT id, vault_id, owner_user_id, path, is_folder, grantee_login, perm, created_at FROM shares;

DROP TABLE shares;
ALTER TABLE shares_identity RENAME TO shares;
CREATE INDEX shares_grantee ON shares (grantee_user_id, vault_id, path);
CREATE INDEX shares_vault ON shares (vault_id);
