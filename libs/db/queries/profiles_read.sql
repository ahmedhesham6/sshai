-- name: GetOwnedProfile :one
SELECT id, owner_user_id, name, slug, created_at, archived_at
FROM profiles
WHERE id = sqlc.arg(profile_id) AND owner_user_id = sqlc.arg(owner_user_id);

-- name: ListOwnedProfiles :many
-- Keyset pagination: rows are ordered by (created_at, id), the same tuple
-- the WHERE predicate compares against the caller's decoded cursor. Passing
-- has_cursor = false selects the first page; row_limit is the caller's
-- effective page size plus one, letting the store detect a next page
-- without a second round trip.
SELECT id, owner_user_id, name, slug, created_at, archived_at
FROM profiles
WHERE owner_user_id = sqlc.arg(owner_user_id)
  AND (
    NOT sqlc.arg(has_cursor)::bool
    OR (created_at, id) > (sqlc.arg(cursor_created_at)::timestamptz, sqlc.arg(cursor_id)::text)
  )
ORDER BY created_at, id
LIMIT sqlc.arg(row_limit)::int;

-- name: GetProfileHeadVersionID :one
SELECT pv.id
FROM profile_versions pv
WHERE pv.profile_id = sqlc.arg(profile_id)
ORDER BY pv.version DESC
LIMIT 1;

-- name: GetOwnedProfileVersion :one
SELECT pv.id, pv.profile_id, pv.parent_version_id, pv.version, pv.digest, pv.created_at
FROM profile_versions pv
JOIN profiles p ON p.id = pv.profile_id
WHERE pv.id = sqlc.arg(profile_version_id) AND p.owner_user_id = sqlc.arg(owner_user_id);
