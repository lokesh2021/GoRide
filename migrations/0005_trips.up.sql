CREATE TABLE trips (
    id             uuid PRIMARY KEY,
    ride_id        uuid NOT NULL UNIQUE REFERENCES rides(id),
    status         text NOT NULL CHECK (status IN ('STARTED', 'PAUSED', 'ENDED')),
    started_at     timestamptz,
    ended_at       timestamptz,
    paused_seconds integer NOT NULL DEFAULT 0,
    distance_m     integer,
    fare           jsonb
);
