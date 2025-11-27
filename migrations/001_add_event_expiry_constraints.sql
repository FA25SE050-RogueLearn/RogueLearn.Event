-- Migration: Add database-level constraints to prevent submissions after event expiry
-- Created: 2025-11-17
-- Purpose: Fix race condition where players can earn points after event expires
--
-- This migration adds:
-- 1. Function to validate event is active before submission updates
-- 2. Trigger to enforce validation on submission status updates
-- 3. Unique constraint to prevent duplicate problem solving
-- 4. Index for performance optimization

-- =============================================================================
-- 1. Create function to validate event is active for submissions
-- =============================================================================

CREATE OR REPLACE FUNCTION validate_submission_event_active()
RETURNS TRIGGER AS $$
DECLARE
    v_event_id uuid;
    v_event_status event_status;
    v_event_end_date timestamp with time zone;
BEGIN
    -- Only validate when marking submission as 'accepted'
    -- This prevents points from being awarded after event completion
    IF NEW.status = 'accepted' AND (OLD.status IS NULL OR OLD.status != 'accepted') THEN

        -- Get event details through room -> event relationship
        SELECT e.id, e.status, e.end_date
        INTO v_event_id, v_event_status, v_event_end_date
        FROM events e
        INNER JOIN rooms r ON r.event_id = e.id
        WHERE r.id = NEW.room_id;

        -- Check if event was found
        IF NOT FOUND THEN
            RAISE EXCEPTION 'Event not found for room_id: %', NEW.room_id
                USING ERRCODE = 'foreign_key_violation',
                      HINT = 'Submission references non-existent room or event';
        END IF;

        -- Validate event is still active
        IF v_event_status != 'active' THEN
            RAISE EXCEPTION 'Cannot accept submission - event is not active (status: %)', v_event_status
                USING ERRCODE = '23514', -- check_violation
                      DETAIL = format('Event %s has status %s', v_event_id, v_event_status),
                      HINT = 'Submissions can only be accepted while event is active';
        END IF;

        -- Validate event has not expired (double-check with server time)
        IF NOW() > v_event_end_date THEN
            RAISE EXCEPTION 'Cannot accept submission - event has expired'
                USING ERRCODE = '23514', -- check_violation
                      DETAIL = format('Event %s ended at %s (current time: %s)',
                                     v_event_id, v_event_end_date, NOW()),
                      HINT = 'Submissions cannot be accepted after event end date';
        END IF;

    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- =============================================================================
-- 2. Create trigger on submissions table
-- =============================================================================

DROP TRIGGER IF EXISTS trigger_validate_submission_event_active ON submissions;

CREATE TRIGGER trigger_validate_submission_event_active
    BEFORE UPDATE OF status ON submissions
    FOR EACH ROW
    EXECUTE FUNCTION validate_submission_event_active();

COMMENT ON TRIGGER trigger_validate_submission_event_active ON submissions IS
    'Prevents submissions from being marked as accepted after event expires or becomes inactive';

-- =============================================================================
-- 3. Add unique constraint to prevent duplicate accepted submissions
-- =============================================================================

-- This ensures that within a room, a player can only have ONE accepted submission per problem
-- Prevents race condition where two submissions both pass the duplicate check

CREATE UNIQUE INDEX IF NOT EXISTS idx_unique_accepted_submission_per_problem_per_player
ON submissions (user_id, code_problem_id, room_id)
WHERE status = 'accepted';

COMMENT ON INDEX idx_unique_accepted_submission_per_problem_per_player IS
    'Ensures a player can only have one accepted submission per problem in a room - prevents duplicate points';

-- =============================================================================
-- 4. Add performance indexes
-- =============================================================================

-- Index for checking if problem already solved (used in CheckIfProblemAlreadySolved query)
CREATE INDEX IF NOT EXISTS idx_submissions_duplicate_check
ON submissions (user_id, code_problem_id, room_id, status)
WHERE status = 'accepted';

-- Index for event status queries (used frequently in validation)
CREATE INDEX IF NOT EXISTS idx_events_status_end_date
ON events (status, end_date)
WHERE status IN ('active', 'completed');

-- =============================================================================
-- Rollback instructions (for reference)
-- =============================================================================

-- To rollback this migration, run:
-- DROP TRIGGER IF EXISTS trigger_validate_submission_event_active ON submissions;
-- DROP FUNCTION IF EXISTS validate_submission_event_active();
-- DROP INDEX IF EXISTS idx_unique_accepted_submission_per_problem_per_player;
-- DROP INDEX IF EXISTS idx_submissions_duplicate_check;
-- DROP INDEX IF EXISTS idx_events_status_end_date;
