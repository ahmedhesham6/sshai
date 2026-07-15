-- name: LockSSHKeyRegistration :one
SELECT pg_advisory_xact_lock(
    hashtextextended('ssh-key' || chr(31) || sqlc.arg(owner_user_id)::text || chr(31) || sqlc.arg(idempotency_key)::text, 0)
);

-- name: LockSSHKeyFingerprint :one
SELECT pg_advisory_xact_lock(
    hashtextextended('ssh-key-fingerprint' || chr(31) || sqlc.arg(owner_user_id)::text || chr(31) || sqlc.arg(fingerprint)::text, 0)
);

-- name: GetSSHKeyRegistration :one
SELECT key.id, key.owner_user_id, key.label, key.algorithm, key.fingerprint,
       key.public_key, key.created_at, key.revoked_at, registration.input
FROM ssh_key_registrations registration
JOIN ssh_keys key ON key.id = registration.ssh_key_id
                 AND key.owner_user_id = registration.owner_user_id
WHERE registration.owner_user_id = sqlc.arg(owner_user_id)
  AND registration.idempotency_key = sqlc.arg(idempotency_key);

-- name: GetOwnedSSHKeyByFingerprint :one
SELECT id, owner_user_id, label, algorithm, fingerprint, public_key, created_at, revoked_at
FROM ssh_keys
WHERE owner_user_id = sqlc.arg(owner_user_id)
  AND fingerprint = sqlc.arg(fingerprint);

-- name: InsertSSHKey :exec
INSERT INTO ssh_keys (
    id, owner_user_id, label, algorithm, fingerprint, public_key, created_at, revoked_at
) VALUES (
    sqlc.arg(id), sqlc.arg(owner_user_id), sqlc.arg(label), sqlc.arg(algorithm),
    sqlc.arg(fingerprint), sqlc.arg(public_key), sqlc.arg(created_at), sqlc.narg(revoked_at)
);

-- name: InsertSSHKeyRegistration :exec
INSERT INTO ssh_key_registrations (owner_user_id, idempotency_key, input, ssh_key_id)
VALUES (sqlc.arg(owner_user_id), sqlc.arg(idempotency_key), sqlc.arg(input), sqlc.arg(ssh_key_id));

-- name: ListActiveOwnedSSHKeys :many
SELECT id, owner_user_id, label, algorithm, fingerprint, public_key, created_at, revoked_at
FROM ssh_keys
WHERE owner_user_id = sqlc.arg(owner_user_id)
  AND revoked_at IS NULL
ORDER BY created_at, id;

-- name: GetOwnedSSHKeyForUpdate :one
SELECT id, owner_user_id, label, algorithm, fingerprint, public_key, created_at, revoked_at
FROM ssh_keys
WHERE owner_user_id = sqlc.arg(owner_user_id)
  AND id = sqlc.arg(id)
FOR UPDATE;

-- name: RevokeOwnedSSHKey :execrows
UPDATE ssh_keys
SET revoked_at = COALESCE(revoked_at, sqlc.arg(revoked_at))
WHERE owner_user_id = sqlc.arg(owner_user_id)
  AND id = sqlc.arg(id);
