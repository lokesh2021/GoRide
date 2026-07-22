CREATE TABLE receipts (
    id         uuid PRIMARY KEY,
    ride_id    uuid NOT NULL UNIQUE REFERENCES rides(id),
    breakdown  jsonb NOT NULL,
    total      integer NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
