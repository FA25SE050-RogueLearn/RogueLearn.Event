CREATE TYPE submission_status AS ENUM (
  'event_unspecified',
  'pending',
  'accepted',
  'wrong_answer',
  'limit_exceed',
  'runtime_error',
  'compilation_error'
);

CREATE TYPE event_type AS ENUM (
    'code_battle',
    'workshop',
    'seminar',
    'social'
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

CREATE TABLE public.code_problem_language_details (
  code_problem_id uuid NOT NULL,
  language_id uuid NOT NULL,
  solution_stub text NOT NULL DEFAULT ''::text,
  driver_code text NOT NULL DEFAULT ''::text,
  time_constraint_ms integer NOT NULL DEFAULT 1000,
  space_constraint_mb integer NOT NULL DEFAULT 16,
  CONSTRAINT code_problem_language_details_pkey PRIMARY KEY (language_id, code_problem_id),
  CONSTRAINT code_problem_language_details_code_problem_id_fkey FOREIGN KEY (code_problem_id) REFERENCES public.code_problems(id),
  CONSTRAINT code_problem_language_details_language_id_fkey FOREIGN KEY (language_id) REFERENCES public.languages(id)
);
CREATE TABLE public.code_problem_tags (
  code_problem_id uuid NOT NULL DEFAULT gen_random_uuid(),
  tag_id uuid NOT NULL DEFAULT gen_random_uuid(),
  CONSTRAINT code_problem_tags_pkey PRIMARY KEY (tag_id, code_problem_id),
  CONSTRAINT code_problem_tags_code_problem_id_fkey FOREIGN KEY (code_problem_id) REFERENCES public.code_problems(id),
  CONSTRAINT code_problem_tags_tag_id_fkey FOREIGN KEY (tag_id) REFERENCES public.tags(id)
);
CREATE TABLE public.code_problems (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  title text NOT NULL DEFAULT ''::text,
  problem_statement text NOT NULL DEFAULT ''::text,
  difficulty integer NOT NULL DEFAULT 1,
  created_at timestamp with time zone NOT NULL DEFAULT (now() AT TIME ZONE 'utc'::text),
  CONSTRAINT code_problems_pkey PRIMARY KEY (id)
);
CREATE TABLE public.event_code_problems (
  event_id uuid NOT NULL,
  code_problem_id uuid NOT NULL,
  score integer NOT NULL DEFAULT 0,
  CONSTRAINT event_code_problems_pkey PRIMARY KEY (code_problem_id, event_id),
  CONSTRAINT event_code_problems_event_id_fkey FOREIGN KEY (event_id) REFERENCES public.events(id),
  CONSTRAINT event_code_problems_code_problem_id_fkey FOREIGN KEY (code_problem_id) REFERENCES public.code_problems(id)
);
CREATE TABLE public.event_guild_participants (
  event_id uuid NOT NULL,
  guild_id uuid NOT NULL,
  joined_at timestamp with time zone DEFAULT (now() AT TIME ZONE 'utc'::text),
  room_id uuid,
  guild_name text,
  CONSTRAINT event_guild_participants_pkey PRIMARY KEY (guild_id, event_id),
  CONSTRAINT event_guild_participants_event_id_fkey FOREIGN KEY (event_id) REFERENCES public.events(id),
  CONSTRAINT event_guild_participants_room_id_fkey FOREIGN KEY (room_id) REFERENCES public.rooms(id)
);
create table public.event_guild_members (
  event_id uuid not null,
  guild_id uuid not null,
  user_id uuid not null,
  selected_at timestamp with time zone not null default (now() AT TIME ZONE 'utc'::text),
  constraint event_guild_members_pkey primary key (event_id, guild_id, user_id),
  constraint event_guild_members_event_id_guild_id_fkey foreign KEY (event_id, guild_id) references event_guild_participants (event_id, guild_id)
);

CREATE TABLE public.events (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  title text NOT NULL DEFAULT ''::text,
  description text NOT NULL DEFAULT ''::text,
  type event_type NOT NULL,
  started_date timestamp with time zone NOT NULL DEFAULT (now() AT TIME ZONE 'utc'::text),
  end_date timestamp with time zone NOT NULL DEFAULT (now() AT TIME ZONE 'utc'::text),
  max_guilds integer,
  max_players_per_guild integer,
  original_request_id uuid,
  status event_status NOT NULL DEFAULT 'pending',
  assignment_date timestamp with time zone,  -- When to assign guilds to rooms (e.g., 15 min before start)
  CONSTRAINT events_pkey PRIMARY KEY (id),
  CONSTRAINT events_original_request_id_fkey FOREIGN KEY (original_request_id) REFERENCES public.event_requests(id)
);

-- Index for efficiently querying events ready for assignment
CREATE INDEX IF NOT EXISTS idx_events_assignment_pending
ON public.events(assignment_date)
WHERE status = 'pending';
CREATE TABLE public.guild_leaderboard_entries (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  guild_id uuid NOT NULL,
  event_id uuid NOT NULL,
  rank integer NOT NULL,
  total_score integer NOT NULL,
  snapshot_date timestamp with time zone NOT NULL DEFAULT (now() AT TIME ZONE 'utc'::text),
  CONSTRAINT guild_leaderboard_entries_pkey PRIMARY KEY (id),
  CONSTRAINT guild_leaderboard_entries_event_id_fkey FOREIGN KEY (event_id) REFERENCES public.events(id)
);
CREATE TABLE public.languages (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  name text NOT NULL,
  compile_cmd text NOT NULL,
  run_cmd text NOT NULL,
  temp_file_dir text,
  temp_file_name text,
  CONSTRAINT languages_pkey PRIMARY KEY (id)
);
CREATE TABLE public.leaderboard_entries (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL,
  event_id uuid NOT NULL,
  rank integer NOT NULL,
  score integer NOT NULL,
  snapshot_date timestamp with time zone NOT NULL DEFAULT (now() AT TIME ZONE 'utc'::text),
  CONSTRAINT leaderboard_entries_pkey PRIMARY KEY (id),
  CONSTRAINT leaderboard_entries_event_id_fkey FOREIGN KEY (event_id) REFERENCES public.events(id)
);
CREATE TABLE public.room_players (
  room_id uuid NOT NULL,
  user_id uuid NOT NULL,
  guild_id uuid NOT NULL,
  username text NOT NULL DEFAULT ''::text,
  score integer NOT NULL DEFAULT 0,
  place integer,
  state room_player_state NOT NULL DEFAULT 'present'::room_player_state,
  disconnected_at timestamp with time zone,
  joined_at timestamp with time zone NOT NULL DEFAULT (now() AT TIME ZONE 'utc'::text),
  CONSTRAINT room_players_pkey PRIMARY KEY (room_id, user_id),
  CONSTRAINT room_players_room_id_fkey FOREIGN KEY (room_id) REFERENCES public.rooms(id)
);
CREATE TABLE public.rooms (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  event_id uuid NOT NULL,
  name text NOT NULL DEFAULT ''::text,
  description text NOT NULL DEFAULT ''::text,
  created_date timestamp with time zone NOT NULL DEFAULT (now() AT TIME ZONE 'utc'::text),
  CONSTRAINT rooms_pkey PRIMARY KEY (id),
  CONSTRAINT Rooms_event_id_fkey FOREIGN KEY (event_id) REFERENCES public.events(id)
);
CREATE TABLE public.submissions (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL,
  code_problem_id uuid NOT NULL,
  language_id uuid NOT NULL,
  room_id uuid,
  code_submitted text NOT NULL DEFAULT ''::text,
  status submission_status NOT NULL,
  execution_time_ms integer,
  submitted_at timestamp with time zone NOT NULL DEFAULT (now() AT TIME ZONE 'utc'::text),
  CONSTRAINT submissions_pkey PRIMARY KEY (id),
  CONSTRAINT submissions_code_problem_id_fkey FOREIGN KEY (code_problem_id) REFERENCES public.code_problems(id),
  CONSTRAINT submissions_language_id_fkey FOREIGN KEY (language_id) REFERENCES public.languages(id),
  CONSTRAINT submissions_room_id_fkey FOREIGN KEY (room_id) REFERENCES public.rooms(id)
);
CREATE TABLE public.tags (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  name text NOT NULL DEFAULT ''::text UNIQUE,
  created_at timestamp with time zone NOT NULL DEFAULT now(),
  CONSTRAINT tags_pkey PRIMARY KEY (id)
);
CREATE TABLE public.test_cases (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  code_problem_id uuid NOT NULL,
  input text NOT NULL,
  expected_output text NOT NULL,
  is_hidden boolean NOT NULL DEFAULT true,
  CONSTRAINT test_cases_pkey PRIMARY KEY (id),
  CONSTRAINT test_cases_code_problem_id_fkey FOREIGN KEY (code_problem_id) REFERENCES public.code_problems(id)
);

CREATE TABLE public.event_requests (
  id uuid NOT NULL DEFAULT gen_random_uuid(),
  status event_request_status NOT NULL DEFAULT 'pending'::event_request_status,
  requester_guild_id uuid NOT NULL,
  processed_by_admin_id uuid,
  created_at timestamp with time zone NOT NULL DEFAULT (now() AT TIME ZONE 'utc'::text),
  processed_at timestamp with time zone,
  event_type event_type NOT NULL,
  title text NOT NULL,
  description text NOT NULL,
  proposed_start_date timestamp with time zone NOT NULL,
  proposed_end_date timestamp with time zone NOT NULL,
  notes text DEFAULT ''::text,
  participation_details jsonb NOT NULL,
  event_specifics jsonb,
  rejection_reason text,
  approved_event_id uuid,
  CONSTRAINT event_requests_pkey PRIMARY KEY (id),
  CONSTRAINT event_requests_approved_event_id_fkey FOREIGN KEY (approved_event_id) REFERENCES public.events(id)
);
