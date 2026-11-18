-- Newest-first paging for the admin UI.
CREATE INDEX IF NOT EXISTS calls_started_at_idx ON calls (started_at DESC, id);
