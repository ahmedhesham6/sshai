-- name: UpsertCapsuleTag :one
INSERT INTO capsule_tags (owner_user_id, name, tag, digest, updated_at)
VALUES (sqlc.arg(owner_user_id), sqlc.arg(name), sqlc.arg(tag), sqlc.arg(digest), sqlc.arg(updated_at))
ON CONFLICT (owner_user_id, name, tag) DO UPDATE
SET digest = EXCLUDED.digest,
    updated_at = CASE
        WHEN capsule_tags.digest = EXCLUDED.digest THEN capsule_tags.updated_at
        ELSE EXCLUDED.updated_at
    END
RETURNING owner_user_id, name, tag, digest, updated_at;

-- name: GetCapsuleTag :one
SELECT owner_user_id, name, tag, digest, updated_at
FROM capsule_tags
WHERE owner_user_id = sqlc.arg(owner_user_id)
  AND name = sqlc.arg(name)
  AND tag = sqlc.arg(tag);
