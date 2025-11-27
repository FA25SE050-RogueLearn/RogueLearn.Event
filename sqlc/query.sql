-- Events
-- name: CreateEvent :one
INSERT INTO events (
  title,
  description,
  type,
  started_date,
  end_date,
  max_guilds,
  max_players_per_guild,
  original_request_id,
  status,
  assignment_date
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetEventByID :one
SELECT * FROM events WHERE id = $1;

-- name: GetEvents :many
SELECT * FROM events
ORDER BY started_date ASC
LIMIT $1
OFFSET $2;

-- name: GetEventRequesterIDByEventID :one
SELECT event_requests.requester_guild_id FROM events
JOIN event_requests ON events.original_request_id = event_requests.id
WHERE events.id = $1;

-- name: CountEvents :one
SELECT COUNT(*) FROM events;

-- name: GetEventsByStatus :many
-- Filter events by status with pagination
SELECT * FROM events
WHERE status = $1
ORDER BY started_date ASC
LIMIT $2
OFFSET $3;

-- name: CountEventsByStatus :one
-- Count events filtered by status
SELECT COUNT(*) FROM events
WHERE status = $1;

-- name: GetEventsForNormalUsers :many
-- Get events visible to normal users (pending, active, completed) with pagination
SELECT * FROM events
WHERE status IN ('pending', 'active', 'completed')
ORDER BY started_date ASC
LIMIT $1
OFFSET $2;

-- name: CountEventsForNormalUsers :one
-- Count events visible to normal users (pending, active, completed)
SELECT COUNT(*) FROM events
WHERE status IN ('pending', 'active', 'completed');

-- name: GetEventsByType :many
SELECT * FROM events
WHERE type = $1
ORDER BY started_date ASC;

-- name: GetActiveEvents :many
SELECT * FROM events
WHERE status = 'active'
ORDER BY started_date ASC;

-- name: GetExpiredActiveEvents :many
-- Find events that are stuck in 'active' status but have already expired
-- This is used by the background job to detect and complete orphaned events
-- whose expiry timers were lost due to crashes or failures
SELECT * FROM events
WHERE status = 'active'
  AND end_date < NOW() AT TIME ZONE 'utc'
ORDER BY end_date ASC;

-- name: UpdateEvent :one
UPDATE events
SET
  title = $2,
  description = $3,
  type = $4,
  started_date = $5,
  end_date = $6,
  max_guilds = $7,
  max_players_per_guild = $8
WHERE id = $1
RETURNING *;

-- name: DeleteEvent :exec
DELETE FROM events WHERE id = $1;

-- Languages
-- name: CreateLanguage :one
INSERT INTO languages (name, compile_cmd, run_cmd, temp_file_dir, temp_file_name)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetLanguageByID :one
SELECT * FROM languages WHERE id = $1;

-- name: GetLanguages :many
SELECT *
FROM languages
ORDER BY name
LIMIT $1
OFFSET $2;

-- name: GetLanguageByName :one
SELECT * FROM languages WHERE name = $1;

-- name: UpdateLanguage :one
UPDATE languages
SET name = $2, compile_cmd = $3, run_cmd = $4, temp_file_dir = $5, temp_file_name = $6
WHERE id = $1
RETURNING *;

-- name: DeleteLanguage :exec
DELETE FROM languages WHERE id = $1;

-- Tags
-- name: CreateTag :one
INSERT INTO tags (name)
VALUES ($1)
RETURNING *;

-- name: GetTagByID :one
SELECT * FROM tags WHERE id = $1;

-- name: GetTags :many
SELECT * FROM tags
ORDER BY name
LIMIT $1
OFFSET $2;

-- name: CountTags :one
SELECT COUNT(*) FROM tags;


-- name: GetTagByName :one
SELECT * FROM tags WHERE name = $1;

-- name: UpdateTag :one
UPDATE tags
SET name = $2
WHERE id = $1
RETURNING *;

-- name: DeleteTag :exec
DELETE FROM tags WHERE id = $1;

-- Code Problems
-- name: CreateCodeProblem :one
INSERT INTO code_problems (title, problem_statement, difficulty)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetCodeProblemByID :one
SELECT * FROM code_problems WHERE id = $1;

-- name: GetCodeProblems :many
SELECT * FROM code_problems
ORDER BY created_at DESC
LIMIT $1
OFFSET $2;

-- name: CountCodeProblems :one
SELECT COUNT(*) FROM code_problems;

-- name: GetCodeProblemsByDifficulty :many
SELECT * FROM code_problems
WHERE difficulty = $1
ORDER BY created_at DESC
LIMIT $2
OFFSET $3;

-- name: GetCodeProblemsByCriteria :many
SELECT DISTINCT cp.*
FROM code_problems cp
LEFT JOIN code_problem_tags cpt ON cp.id = cpt.code_problem_id
LEFT JOIN tags t ON cpt.tag_id = t.id
WHERE
  (sqlc.narg(difficulty)::int IS NULL OR cp.difficulty = sqlc.narg(difficulty)::int)
  AND (sqlc.narg(tag_names)::text[] IS NULL OR t.name = ANY(sqlc.narg(tag_names)::text[]))
ORDER BY cp.created_at DESC
LIMIT sqlc.arg(limit_count)
OFFSET sqlc.arg(offset_count);

-- name: GetRandomCodeProblemsByDifficultyAndTags :many
-- Randomly select code problems based on difficulty and tag IDs
-- This is optimized for event problem assignment
SELECT cp.*
FROM code_problems cp
WHERE cp.id IN (
  SELECT DISTINCT cp2.id
  FROM code_problems cp2
  INNER JOIN code_problem_tags cpt ON cp2.id = cpt.code_problem_id
  WHERE
    cp2.difficulty = sqlc.arg(difficulty)::int
    AND (
      sqlc.arg(tag_ids)::uuid[] IS NULL
      OR sqlc.arg(tag_ids)::uuid[] = '{}'
      OR cpt.tag_id = ANY(sqlc.arg(tag_ids)::uuid[])
    )
    AND NOT (cp2.id = ANY(sqlc.arg(excluded_problem_ids)::uuid[]))
)
ORDER BY RANDOM()
LIMIT sqlc.arg(limit_count);

-- name: UpdateCodeProblem :one
UPDATE code_problems
SET title = $2, problem_statement = $3, difficulty = $4
WHERE id = $1
RETURNING *;

-- name: DeleteCodeProblem :exec
DELETE FROM code_problems WHERE id = $1;

-- Code Problem Language Details
-- name: CreateCodeProblemLanguageDetail :one
INSERT INTO code_problem_language_details (code_problem_id, language_id, solution_stub, driver_code, time_constraint_ms, space_constraint_mb)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetCodeProblemLanguageDetail :one
SELECT * FROM code_problem_language_details
WHERE code_problem_id = $1 AND language_id = $2;

-- name: GetCodeProblemLanguageDetailByLanguageName :one
SELECT cpld.*
FROM code_problem_language_details cpld
JOIN languages l ON cpld.language_id = l.id
WHERE cpld.code_problem_id = $1 AND l.name = $2;

-- name: GetCodeProblemLanguage :one
SELECT * FROM code_problem_language_details
WHERE code_problem_id = $1 AND language_id = $2;

-- name: GetCodeProblemLanguageDetails :many
SELECT * FROM code_problem_language_details
WHERE code_problem_id = $1
LIMIT $2
OFFSET $3;

-- name: GetLanguageDetailsForProblem :many
SELECT cpld.*, l.name as language_name
FROM code_problem_language_details cpld
JOIN languages l ON cpld.language_id = l.id
WHERE cpld.code_problem_id = $1;

-- name: UpdateCodeProblemLanguageDetail :one
UPDATE code_problem_language_details
SET solution_stub = $3, driver_code = $4, time_constraint_ms = $5, space_constraint_mb = $6
WHERE code_problem_id = $1 AND language_id = $2
RETURNING *;

-- name: DeleteCodeProblemLanguageDetail :exec
DELETE FROM code_problem_language_details
WHERE code_problem_id = $1 AND language_id = $2;

-- Code Problem Tags
-- name: CreateCodeProblemTag :exec
INSERT INTO code_problem_tags (code_problem_id, tag_id)
VALUES ($1, $2);

-- name: GetCodeProblemTags :many
SELECT cpt.*, t.name as tag_name
FROM code_problem_tags cpt
JOIN tags t ON cpt.tag_id = t.id
WHERE cpt.code_problem_id = $1;

-- name: GetCodeProblemsByTag :many
SELECT cp.*, t.name as tag_name
FROM code_problems cp
JOIN code_problem_tags cpt ON cp.id = cpt.code_problem_id
JOIN tags t ON cpt.tag_id = t.id
WHERE t.id = $1
ORDER BY cp.created_at DESC;

-- name: DeleteCodeProblemTag :exec
DELETE FROM code_problem_tags
WHERE code_problem_id = $1 AND tag_id = $2;

-- Test Cases
-- name: CreateTestCase :one
INSERT INTO test_cases (code_problem_id, input, expected_output, is_hidden)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetTestCaseByID :one
SELECT * FROM test_cases WHERE id = $1;

-- name: GetTestCasesByProblem :many
SELECT * FROM test_cases
WHERE code_problem_id = $1
ORDER BY is_hidden, id;

-- name: GetPublicTestCasesByProblem :many
SELECT * FROM test_cases
WHERE code_problem_id = $1 AND is_hidden = false
ORDER BY id;

-- name: UpdateTestCase :one
UPDATE test_cases
SET input = $2, expected_output = $3, is_hidden = $4
WHERE id = $1
RETURNING *;

-- name: DeleteTestCase :exec
DELETE FROM test_cases WHERE id = $1;

-- Rooms
-- name: CreateRoom :one
INSERT INTO rooms (event_id, name, description)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetRoomByID :one
SELECT * FROM rooms WHERE id = $1;

-- name: GetRoomsByEvent :many
SELECT * FROM rooms
WHERE event_id = $1
ORDER BY created_date ASC;

-- name: GetRoomsWithEventDetails :many
SELECT r.*, e.title as event_title, e.type as event_type
FROM rooms r
JOIN events e ON r.event_id = e.id
WHERE r.event_id = $1
ORDER BY r.created_date ASC;

-- name: UpdateRoom :one
UPDATE rooms
SET name = $2, description = $3
WHERE id = $1
RETURNING *;

-- name: DeleteRoom :exec
DELETE FROM rooms WHERE id = $1;

-- Room Players
-- name: CreateRoomPlayer :one
INSERT INTO room_players (room_id, user_id, username, guild_id, score, place, state)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetRoomPlayer :one
SELECT * FROM room_players
WHERE room_id = $1 AND user_id = $2;

-- name: GetRoomPlayers :many
SELECT * FROM room_players
WHERE room_id = $1
ORDER BY score DESC, place ASC;

-- name: GetPlayersByUserID :many
SELECT * FROM room_players
WHERE user_id = $1
ORDER BY score DESC;

-- name: UpdateRoomPlayerScore :one
UPDATE room_players
SET score = $3, place = $4
WHERE room_id = $1 AND user_id = $2
RETURNING *;

-- name: AddRoomPlayerScore :exec
UPDATE public.room_players
SET score = score + sqlc.arg(points_to_add)::integer
WHERE room_id = sqlc.arg(room_id) AND user_id = sqlc.arg(user_id);

-- name: UpdateRoomPlayerState :one
UPDATE room_players
SET state = $3
WHERE room_id = $1 AND user_id = $2
RETURNING *;

-- name: DisconnectRoomPlayer :one
UPDATE room_players
SET disconnected_at = NOW() AT TIME ZONE 'utc'
WHERE room_id = $1 AND user_id = $2
RETURNING *;

-- name: DeleteRoomPlayer :exec
DELETE FROM room_players
WHERE room_id = $1 AND user_id = $2;

-- Event Code Problems
-- name: CreateEventCodeProblem :exec
INSERT INTO event_code_problems (event_id, code_problem_id, score)
VALUES ($1, $2, $3);

-- name: GetEventCodeProblems :many
SELECT ecp.*, cp.title, cp.difficulty, cp.problem_statement
FROM event_code_problems ecp
JOIN code_problems cp ON ecp.code_problem_id = cp.id
WHERE ecp.event_id = $1
ORDER BY ecp.score DESC;

-- name: GetEventCodeProblem :one
SELECT ecp.*, cp.title, cp.difficulty
FROM event_code_problems ecp
JOIN code_problems cp ON ecp.code_problem_id = cp.id
WHERE ecp.event_id = $1 AND ecp.code_problem_id = $2;

-- name: UpdateEventCodeProblemScore :one
UPDATE event_code_problems
SET score = $3
WHERE event_id = $1 AND code_problem_id = $2
RETURNING *;

-- name: DeleteEventCodeProblem :exec
DELETE FROM event_code_problems
WHERE event_id = $1 AND code_problem_id = $2;

-- Event Guild Participants
-- name: CreateEventGuildParticipant :one
INSERT INTO event_guild_participants (event_id, guild_id, room_id, guild_name)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetEventGuildParticipant :one
SELECT * FROM event_guild_participants
WHERE event_id = $1 AND guild_id = $2;

-- name: GetEventGuildParticipants :many
SELECT * FROM event_guild_participants
WHERE event_id = $1
ORDER BY joined_at ASC;

-- name: GetEventParticipants :many
SELECT * FROM event_guild_participants
WHERE event_id = $1
ORDER BY joined_at ASC;

-- name: CountEventParticipants :one
SELECT COUNT(*) FROM event_guild_participants
WHERE event_id = $1;

-- name: AssignGuildToRoom :exec
UPDATE event_guild_participants
SET room_id = $3
WHERE event_id = $1 AND guild_id = $2;

-- name: GetGuildParticipantsByGuild :many
SELECT * FROM event_guild_participants
WHERE guild_id = $1
ORDER BY joined_at DESC;

-- name: UpdateEventGuildParticipantRoom :one
UPDATE event_guild_participants
SET room_id = $3
WHERE event_id = $1 AND guild_id = $2
RETURNING *;

-- name: DeleteEventGuildParticipant :exec
DELETE FROM event_guild_participants
WHERE event_id = $1 AND guild_id = $2;

-- Event Guild Members
-- name: AddGuildMemberToEvent :one
INSERT INTO event_guild_members (event_id, guild_id, user_id)
VALUES ($1, $2, $3)
ON CONFLICT (event_id, guild_id, user_id) DO NOTHING
RETURNING *;

-- name: GetEventGuildMembers :many
SELECT * FROM event_guild_members
WHERE event_id = $1 AND guild_id = $2
ORDER BY selected_at ASC;

-- name: CountEventGuildMembers :one
SELECT COUNT(*) FROM event_guild_members
WHERE event_id = $1 AND guild_id = $2;

-- name: RemoveGuildMemberFromEvent :exec
DELETE FROM event_guild_members
WHERE event_id = $1 AND guild_id = $2 AND user_id = $3;

-- name: GetAllEventGuildMembers :many
SELECT * FROM event_guild_members
WHERE event_id = $1
ORDER BY guild_id, selected_at ASC;

-- name: CheckIfUserSelectedForEvent :one
SELECT user_id FROM event_guild_members
WHERE event_id = $1 AND guild_id = $2 AND user_id = $3;

-- Submissions
-- name: CreateSubmission :one
INSERT INTO submissions (user_id, code_problem_id, language_id, room_id, code_submitted, status, execution_time_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: CheckIfProblemAlreadySolved :one
-- Check if a player has already solved a problem in a specific room
-- Returns the submission if it exists and is accepted, otherwise returns error
SELECT id, status, submitted_at
FROM submissions
WHERE user_id = $1
  AND code_problem_id = $2
  AND room_id = $3
  AND status = 'accepted'
LIMIT 1;

-- name: GetSubmissionByID :one
SELECT * FROM submissions WHERE id = $1;

-- name: GetSubmissionsByUser :many
SELECT s.*, cp.title as problem_title, l.name as language_name
FROM submissions s
JOIN code_problems cp ON s.code_problem_id = cp.id
JOIN languages l ON s.language_id = l.id
WHERE s.user_id = $1
ORDER BY s.submitted_at DESC
LIMIT $2
OFFSET $3;

-- name: CountSubmissionsByUser :one
SELECT COUNT(*)
FROM submissions s
WHERE s.user_id = $1;

-- name: GetSubmissionsByProblem :many
SELECT s.*, l.name as language_name
FROM submissions s
JOIN languages l ON s.language_id = l.id
WHERE s.code_problem_id = $1
ORDER BY s.submitted_at DESC;

-- name: GetSubmissionsByUserAndProblem :many
SELECT s.*, cp.title as problem_title, l.name as language_name
FROM submissions s
JOIN code_problems cp ON s.code_problem_id = cp.id
JOIN languages l ON s.language_id = l.id
WHERE s.user_id = $1 AND s.code_problem_id = $2
ORDER BY s.submitted_at DESC
LIMIT $3
OFFSET $4;

-- name: CountSubmissionsByUserAndProblem :one
SELECT COUNT(*)
FROM submissions s
WHERE s.user_id = $1 AND s.code_problem_id = $2;

-- name: GetSubmissionsByRoom :many
SELECT s.*, cp.title as problem_title, l.name as language_name
FROM submissions s
JOIN code_problems cp ON s.code_problem_id = cp.id
JOIN languages l ON s.language_id = l.id
WHERE s.room_id = $1
ORDER BY s.submitted_at DESC;

-- name: GetSubmissionsByGuild :many
SELECT s.*, cp.title as problem_title, l.name as language_name
FROM submissions s
JOIN code_problems cp ON s.code_problem_id = cp.id
JOIN languages l ON s.language_id = l.id
JOIN room_players rp ON s.user_id = rp.user_id AND s.room_id = rp.room_id
WHERE rp.guild_id = $1
ORDER BY s.submitted_at DESC;

-- name: GetSubmissionsByStatus :many
SELECT s.*, cp.title as problem_title, l.name as language_name
FROM submissions s
JOIN code_problems cp ON s.code_problem_id = cp.id
JOIN languages l ON s.language_id = l.id
WHERE s.status = $1
ORDER BY s.submitted_at DESC;

-- name: UpdateSubmission :one
UPDATE submissions
SET status = $2, execution_time_ms = $3
WHERE id = $1
RETURNING *;

-- name: DeleteSubmission :exec
DELETE FROM submissions WHERE id = $1;

-- Leaderboard Entries
-- name: CreateLeaderboardEntry :one
INSERT INTO leaderboard_entries (user_id, username, event_id, rank, score)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetLeaderboardByEvent :many
WITH user_guilds AS (
  SELECT DISTINCT ON (rp.user_id)
    rp.user_id,
    rp.guild_id
  FROM room_players rp
  INNER JOIN rooms r ON rp.room_id = r.id
  WHERE r.event_id = $1
)
SELECT
  le.*,
  COALESCE(egp.guild_name, '') as guild_name
FROM leaderboard_entries le
LEFT JOIN user_guilds ug ON ug.user_id = le.user_id
LEFT JOIN event_guild_participants egp ON egp.event_id = le.event_id AND egp.guild_id = ug.guild_id
WHERE le.event_id = $1
ORDER BY le.rank ASC;

-- name: GetLeaderboardByUser :many
SELECT le.*, e.title as event_title
FROM leaderboard_entries le
JOIN events e ON le.event_id = e.id
WHERE le.user_id = $1
ORDER BY le.snapshot_date DESC;

-- name: GetLatestLeaderboardByEvent :many
SELECT * FROM leaderboard_entries le1
WHERE le1.event_id = $1
AND le1.snapshot_date = (
    SELECT MAX(le2.snapshot_date)
    FROM leaderboard_entries le2
    WHERE le2.event_id = $1
)
ORDER BY le1.rank ASC;

-- name: CalculateRoomLeaderboard :exec
-- This query uses SELECT FOR UPDATE to lock rows and prevent race conditions
-- across multiple instances when calculating leaderboard rankings.
-- The lock is held until the transaction commits, ensuring atomicity.
WITH locked_players AS (
  -- First, lock the rows without window functions
  SELECT user_id, score, joined_at
  FROM room_players
  WHERE room_id = $1
  FOR UPDATE  -- Lock these rows to prevent concurrent modifications
),
ranked_players AS (
  -- Then, calculate rankings using window function (no FOR UPDATE here)
  SELECT
    user_id,
    DENSE_RANK() OVER (ORDER BY score DESC, joined_at ASC) as new_place
  FROM locked_players
)
UPDATE room_players rp
SET place = rp_ranked.new_place
FROM ranked_players rp_ranked
WHERE rp.room_id = $1 AND rp.user_id = rp_ranked.user_id;


-- name: UpdateLeaderboardEntry :one
UPDATE leaderboard_entries
SET rank = $2, score = $3
WHERE id = $1
RETURNING *;

-- name: DeleteLeaderboardEntry :exec
DELETE FROM leaderboard_entries
WHERE id = $1;

-- Guild Leaderboard Entries
-- name: CreateGuildLeaderboardEntry :one
INSERT INTO guild_leaderboard_entries (guild_id, event_id, rank, total_score)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetGuildLeaderboardByEvent :many
SELECT
  gle.*,
  COALESCE(egp.guild_name, '') as guild_name
FROM guild_leaderboard_entries gle
LEFT JOIN event_guild_participants egp ON egp.event_id = gle.event_id AND egp.guild_id = gle.guild_id
WHERE gle.event_id = $1
ORDER BY gle.rank ASC;

-- name: GetGuildLeaderboardByGuild :many
SELECT
  gle.*,
  e.title as event_title,
  COALESCE(egp.guild_name, '') as guild_name
FROM guild_leaderboard_entries gle
JOIN events e ON gle.event_id = e.id
LEFT JOIN event_guild_participants egp ON egp.event_id = gle.event_id AND egp.guild_id = gle.guild_id
WHERE gle.guild_id = $1
ORDER BY gle.snapshot_date DESC;

-- name: GetLatestGuildLeaderboardByEvent :many
SELECT
  gle1.*,
  COALESCE(egp.guild_name, '') as guild_name
FROM guild_leaderboard_entries gle1
LEFT JOIN event_guild_participants egp ON egp.event_id = gle1.event_id AND egp.guild_id = gle1.guild_id
WHERE gle1.event_id = $1
AND gle1.snapshot_date = (
    SELECT MAX(gle2.snapshot_date)
    FROM guild_leaderboard_entries gle2
    WHERE gle2.event_id = $1
)
ORDER BY gle1.rank ASC;

-- name: UpdateGuildLeaderboardEntry :one
UPDATE guild_leaderboard_entries
SET rank = $2, total_score = $3
WHERE id = $1
RETURNING *;

-- name: DeleteGuildLeaderboardEntry :exec
DELETE FROM guild_leaderboard_entries
WHERE id = $1;

-- name: CalculateGuildLeaderboard :exec
WITH latest_snapshot_time AS (
  SELECT MAX(gle1.snapshot_date) as snapshot_time
  FROM guild_leaderboard_entries gle1
  WHERE gle1.event_id = $1
),
ranked_entries AS (
  SELECT
    id,
    DENSE_RANK() OVER (ORDER BY total_score DESC) as new_rank
  FROM guild_leaderboard_entries gle2
  WHERE gle2.event_id = $1 AND gle2.snapshot_date = (SELECT gle3.snapshot_time FROM latest_snapshot_time gle3)
)
UPDATE guild_leaderboard_entries gle
SET rank = re.new_rank
FROM ranked_entries re
WHERE gle.id = re.id;

-- Complex Queries
-- name: GetEventWithProblemsAndLanguages :many
SELECT
    e.*,
    cp.id as problem_id,
    cp.title as problem_title,
    cp.difficulty as problem_difficulty,
    ecp.score as problem_score
FROM events e
LEFT JOIN event_code_problems ecp ON e.id = ecp.event_id
LEFT JOIN code_problems cp ON ecp.code_problem_id = cp.id
WHERE e.id = $1
ORDER BY ecp.score DESC;

-- name: GetRoomLeaderboard :many
SELECT
    rp.*,
    COUNT(s.id) as submission_count,
    MAX(s.submitted_at) as last_submission
FROM room_players rp
LEFT JOIN submissions s ON rp.user_id = s.user_id AND s.room_id = rp.room_id
WHERE rp.room_id = $1
GROUP BY rp.room_id, rp.user_id
ORDER BY rp.score DESC, rp.place ASC;

-- name: GetUserSubmissionStats :one
SELECT
    COUNT(*) as total_submissions,
    COUNT(CASE WHEN status = 'accepted' THEN 1 END) as accepted_count,
    COUNT(CASE WHEN status = 'wrong_answer' THEN 1 END) as wrong_answer_count,
    COUNT(CASE WHEN status = 'limit_exceed' THEN 1 END) as timeout_count,
    AVG(execution_time_ms) as avg_execution_time
FROM submissions
WHERE user_id = $1;

-- name: GetProblemSubmissionStats :one
SELECT
    COUNT(*) as total_submissions,
    COUNT(DISTINCT user_id) as unique_users,
    COUNT(CASE WHEN status = 'accepted' THEN 1 END) as accepted_count,
    ROUND(COUNT(CASE WHEN status = 'accepted' THEN 1 END) * 100.0 / COUNT(*), 2) as acceptance_rate
FROM submissions
WHERE code_problem_id = $1;

-- Event Requests
-- name: CreateEventRequest :one
INSERT INTO event_requests (
  requester_guild_id, event_type, title, description,
  proposed_start_date, proposed_end_date, notes,
  participation_details, event_specifics
) VALUES (
  $1, $2, $3, $4, $5, $6, $7,
  sqlc.arg(participation_details)::jsonb,
  sqlc.arg(event_specifics)::jsonb
) RETURNING *;

-- name: GetEventRequestByID :one
SELECT * FROM event_requests WHERE id = $1;

-- name: ListEventRequests :many
SELECT * FROM event_requests
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountEventRequests :one
SELECT COUNT(*) FROM event_requests;

-- name: ListEventRequestsByStatus :many
SELECT * FROM event_requests
WHERE status = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountEventRequestsByStatus :one
SELECT COUNT(*) FROM event_requests
WHERE status = $1;

-- name: ListEventRequestsByGuild :many
SELECT * FROM event_requests
WHERE requester_guild_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountEventRequestsByGuild :one
SELECT COUNT(*) FROM event_requests
WHERE requester_guild_id = $1;

-- name: UpdateEventRequestStatus :one
UPDATE event_requests
SET
  status = $2,
  processed_by_admin_id = $3,
  processed_at = NOW() AT TIME ZONE 'utc'
,
  rejection_reason = $4,
  approved_event_id = $5
WHERE id = $1
RETURNING *;


-- ========================================
-- Event Assignment Queries (for scheduled guild-to-room assignment)
-- ========================================

-- name: GetPendingEventsForAssignment :many
-- Get all events that are ready for guild-to-room assignment
-- (assignment_date has passed and status is still 'pending')
SELECT * FROM events
WHERE status = 'pending'
  AND assignment_date <= NOW() AT TIME ZONE 'utc'
ORDER BY assignment_date ASC;

-- name: UpdateEventStatusToActive :one
-- Atomically mark event as 'active' and prevent duplicate processing
-- Only updates if the event is still in 'pending' status
-- Returns the updated event if successful, or error if already processed
UPDATE events
SET status = 'active'
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: UpdateEventStatusToCompleted :execrows
-- Atomically mark event as 'completed' (only if still 'active')
-- Used by event expiry timer - atomic update prevents duplicate completion
-- Returns number of rows affected (1 = success, 0 = already completed)
UPDATE events
SET status = 'completed'
WHERE id = $1 AND status = 'active';

-- name: UpdateRoomPlayerStatesOnEventComplete :execrows
-- Update player states when event completes
-- Uses FOR UPDATE to lock rows and prevent race conditions
-- present -> completed (player was active when event ended)
-- disconnected -> left (player disconnected during event)
-- Returns number of rows affected
WITH locked_players AS (
  SELECT rp.room_id, rp.user_id, rp.state
  FROM room_players rp
  INNER JOIN rooms r ON rp.room_id = r.id
  WHERE r.event_id = $1
  FOR UPDATE OF rp
)
UPDATE room_players rp
SET state = CASE
  WHEN lp.state = 'present' THEN 'completed'::room_player_state
  WHEN lp.state = 'disconnected' THEN 'left'::room_player_state
  ELSE lp.state
END
FROM locked_players lp
WHERE rp.room_id = lp.room_id AND rp.user_id = lp.user_id;

-- name: CaptureFinalRoomLeaderboards :exec
-- Capture final leaderboard snapshot for all rooms in an event
-- This creates a permanent historical record of final rankings and scores
-- Called when event completes to preserve winner information
-- Includes ALL states (present, completed, disconnected, and left)
INSERT INTO leaderboard_entries (user_id, username, event_id, rank, score, snapshot_date)
SELECT
  rp.user_id,
  rp.username,
  $1::uuid as event_id,
  rp.place as rank,
  rp.score,
  NOW() AT TIME ZONE 'utc' as snapshot_date
FROM room_players rp
INNER JOIN rooms r ON rp.room_id = r.id
WHERE r.event_id = $1
ORDER BY rp.score DESC, rp.joined_at ASC;

-- name: CaptureFinalGuildLeaderboard :exec
-- Capture final guild leaderboard snapshot for an event
-- Calculates total scores by summing all players from each guild
-- Creates permanent record of which guild won
-- Includes ALL states (present, completed, disconnected, and left)
WITH guild_scores AS (
  SELECT
    rp.guild_id,
    SUM(rp.score) as total_score,
    COUNT(DISTINCT rp.user_id) as player_count
  FROM room_players rp
  INNER JOIN rooms r ON rp.room_id = r.id
  WHERE r.event_id = $1
  GROUP BY rp.guild_id
),
ranked_guilds AS (
  SELECT
    guild_id,
    total_score,
    DENSE_RANK() OVER (ORDER BY total_score DESC) as rank
  FROM guild_scores
)
INSERT INTO guild_leaderboard_entries (guild_id, event_id, rank, total_score, snapshot_date)
SELECT
  rg.guild_id,
  $1::uuid as event_id,
  rg.rank::integer,
  rg.total_score::integer,
  NOW() AT TIME ZONE 'utc' as snapshot_date
FROM ranked_guilds rg;

-- name: GetTop3PlayersByEvent :many
-- Get top 3 players across all rooms in an event
-- P2-1: Used to populate EventExpired message with winners
-- Includes ALL states (present, completed, disconnected, and left)
SELECT
  rp.user_id,
  rp.username,
  rp.score,
  rp.place as rank,
  rp.room_id
FROM room_players rp
INNER JOIN rooms r ON rp.room_id = r.id
WHERE r.event_id = $1
ORDER BY rp.score DESC, rp.joined_at ASC
LIMIT 3;

-- name: GetAllPlayersByEvent :many
-- Get all players across all rooms in an event for granting achievements and contribution points
-- Includes ALL states (present, completed, disconnected, and left)
SELECT
  rp.user_id,
  rp.username,
  rp.guild_id,
  rp.score
FROM room_players rp
INNER JOIN rooms r ON rp.room_id = r.id
WHERE r.event_id = $1
ORDER BY rp.score DESC, rp.joined_at ASC;

-- name: GetAllGuildsByEvent :many
-- Get all guilds that participated in an event with their total scores for merit point granting
-- Includes ALL states (present, completed, disconnected, and left)
WITH guild_scores AS (
  SELECT
    rp.guild_id,
    SUM(rp.score) as total_score
  FROM room_players rp
  INNER JOIN rooms r ON rp.room_id = r.id
  WHERE r.event_id = $1
  GROUP BY rp.guild_id
)
SELECT
  gs.guild_id,
  gs.total_score
FROM guild_scores gs
ORDER BY gs.total_score DESC;

-- name: GetTop3GuildsByEvent :many
-- Get top 3 guilds by total score for an event
-- P2-1: Used to populate EventExpired message with winning guilds
-- Includes ALL states (present, completed, disconnected, and left)
WITH guild_scores AS (
  SELECT
    rp.guild_id,
    SUM(rp.score) as total_score
  FROM room_players rp
  INNER JOIN rooms r ON rp.room_id = r.id
  WHERE r.event_id = $1
  GROUP BY rp.guild_id
)
SELECT
  gs.guild_id,
  COALESCE(egp.guild_name, '') as guild_name,
  gs.total_score,
  DENSE_RANK() OVER (ORDER BY gs.total_score DESC) as rank
FROM guild_scores gs
LEFT JOIN event_guild_participants egp ON egp.event_id = $1 AND egp.guild_id = gs.guild_id
ORDER BY gs.total_score DESC
LIMIT 3;

-- name: GetEventStatistics :one
-- Get aggregate statistics for an event
-- P2-1: Used to populate EventExpired message with event stats
-- Includes ALL states (present, completed, disconnected, and left)
WITH event_rooms AS (
  SELECT id FROM rooms WHERE event_id = $1
),
player_stats AS (
  SELECT
    COUNT(DISTINCT rp.user_id) as total_participants,
    COALESCE(AVG(rp.score), 0) as average_score,
    COALESCE(MAX(rp.score), 0) as highest_score
  FROM room_players rp
  WHERE rp.room_id IN (SELECT id FROM event_rooms)
),
submission_stats AS (
  SELECT
    COUNT(*) as total_submissions,
    COUNT(CASE WHEN s.status = 'accepted' THEN 1 END) as accepted_submissions
  FROM submissions s
  WHERE s.room_id IN (SELECT id FROM event_rooms)
)
SELECT
  ps.total_participants::integer,
  ss.total_submissions::integer,
  ss.accepted_submissions::integer,
  CASE
    WHEN ss.total_submissions > 0 THEN
      ROUND((ss.accepted_submissions::numeric / ss.total_submissions::numeric) * 100, 2)
    ELSE 0
  END as acceptance_rate,
  ROUND(ps.average_score::numeric, 2) as average_score,
  ps.highest_score::integer,
  (SELECT COUNT(*) FROM event_rooms)::integer as total_rooms
FROM player_stats ps, submission_stats ss;

-- name: ValidateGuildRoomAssignment :one
-- Verify that a guild is assigned to a specific room for an event
-- Returns the guild_id if the assignment is valid, error otherwise
SELECT egp.guild_id
FROM event_guild_participants egp
WHERE egp.event_id = $1
  AND egp.guild_id = $2
  AND egp.room_id = $3;
