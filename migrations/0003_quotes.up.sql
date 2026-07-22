CREATE TABLE quotes (
    id          uuid PRIMARY KEY,
    rider_id    uuid NOT NULL REFERENCES riders(id),
    city        text NOT NULL,
    pickup_lat  double precision NOT NULL,
    pickup_lng  double precision NOT NULL,
    drop_lat    double precision NOT NULL,
    drop_lng    double precision NOT NULL,
    distance_m  integer NOT NULL,
    duration_s  integer NOT NULL,
    surge_x100  integer NOT NULL,
    prices      jsonb NOT NULL,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Supports quote-expiry lookups (README §5: "quotes by expiry").
CREATE INDEX quotes_expires_at_idx ON quotes (expires_at);
