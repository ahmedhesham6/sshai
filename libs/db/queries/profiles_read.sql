-- name: GetOwnedProfile :one
SELECT id, owner_user_id, name, slug, created_at, archived_at
FROM profiles
WHERE id = sqlc.arg(profile_id) AND owner_user_id = sqlc.arg(owner_user_id);

-- name: ListOwnedProfiles :many
SELECT id, owner_user_id, name, slug, created_at, archived_at
FROM profiles
WHERE owner_user_id = sqlc.arg(owner_user_id)
ORDER BY created_at, id;

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
