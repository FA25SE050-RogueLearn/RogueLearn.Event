-- This file store ENUM type
CREATE TYPE event_type AS ENUM (
    'event_unspecified',
    'code_battle',
    'workshop',
    'seminar',
    'social'
);

CREATE TYPE submission_status AS ENUM (
  'event_unspecified',
  'pending',
  'accepted',
  'wrong_answer',
  'limit_exceed',
  'runtime_error',
  'compilation_error'
);

CREATE TYPE event_request_status AS ENUM (
  'pending',
  'approved',
  'rejected'
);

CREATE TYPE room_player_state AS ENUM (
    'present',
    'disconnected',
    'left',
    'completed'
);

CREATE TYPE event_status AS ENUM (
    'pending',    -- Event created, waiting for assignment_date
    'active',     -- Guilds assigned to rooms, event is running
    'completed',  -- Event has ended
    'cancelled'   -- Event was cancelled
);
