CREATE TABLE IF NOT EXISTS calls (
    id          uuid        PRIMARY KEY,
    variables   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    results     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    audio_path  text        NOT NULL DEFAULT '',
    audio_bytes bigint      NOT NULL DEFAULT 0,
    started_at  timestamptz NOT NULL,
    ended_at    timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS calls_expires_at_idx ON calls (expires_at);
