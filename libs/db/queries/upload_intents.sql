-- name: LockUploadIntentRegistration :one
SELECT pg_advisory_xact_lock(
    hashtextextended('upload-intent' || chr(31) || sqlc.arg(owner_user_id)::text || chr(31) || sqlc.arg(idempotency_key)::text, 0)
);

-- name: GetUploadIntentRegistration :one
SELECT intent.id, intent.owner_user_id, intent.kind, intent.digest, intent.size_bytes,
       intent.object_key, intent.created_at, intent.expires_at, registration.input
FROM upload_intent_registrations registration
JOIN upload_intents intent ON intent.id = registration.upload_intent_id
                          AND intent.owner_user_id = registration.owner_user_id
WHERE registration.owner_user_id = sqlc.arg(owner_user_id)
  AND registration.idempotency_key = sqlc.arg(idempotency_key);

-- name: InsertUploadIntent :one
INSERT INTO upload_intents (
    id, owner_user_id, kind, digest, size_bytes, object_key, created_at, expires_at
) VALUES (
    sqlc.arg(id), sqlc.arg(owner_user_id), sqlc.arg(kind), sqlc.arg(digest),
    sqlc.arg(size_bytes), sqlc.arg(object_key), sqlc.arg(created_at), sqlc.arg(expires_at)
)
RETURNING id, owner_user_id, kind, digest, size_bytes, object_key, created_at, expires_at;

-- name: InsertUploadIntentRegistration :exec
INSERT INTO upload_intent_registrations (owner_user_id, idempotency_key, input, upload_intent_id)
VALUES (sqlc.arg(owner_user_id), sqlc.arg(idempotency_key), sqlc.arg(input), sqlc.arg(upload_intent_id));

-- name: GetOwnedUploadIntentByDigest :one
SELECT id, owner_user_id, kind, digest, size_bytes, object_key, created_at, expires_at
FROM upload_intents
WHERE owner_user_id = sqlc.arg(owner_user_id)
  AND kind = sqlc.arg(kind)
  AND digest = sqlc.arg(digest)
ORDER BY created_at DESC, id DESC
LIMIT 1;
