CREATE TABLE payments (
    id          uuid PRIMARY KEY,
    ride_id     uuid NOT NULL REFERENCES rides(id),
    amount      integer NOT NULL,
    method      text NOT NULL CHECK (method IN ('upi', 'card', 'cash')),
    status      text NOT NULL CHECK (status IN ('PENDING', 'PROCESSING', 'SUCCEEDED', 'FAILED')),
    psp_ref     text UNIQUE,
    retry_count integer NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX payments_ride_id_idx ON payments (ride_id);
