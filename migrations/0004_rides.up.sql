CREATE TABLE rides (
    id             uuid PRIMARY KEY,
    rider_id       uuid NOT NULL REFERENCES riders(id),
    driver_id      uuid REFERENCES drivers(id),
    quote_id       uuid NOT NULL REFERENCES quotes(id),
    tier           text NOT NULL CHECK (tier IN ('mini', 'sedan', 'xl')),
    status         text NOT NULL CHECK (status IN (
                       'REQUESTED', 'MATCHING', 'DRIVER_ASSIGNED', 'DRIVER_ARRIVING',
                       'ARRIVED', 'IN_PROGRESS', 'COMPLETED',
                       'CANCELLED_BY_RIDER', 'CANCELLED_BY_DRIVER', 'EXPIRED'
                   )),
    pickup_lat     double precision NOT NULL,
    pickup_lng     double precision NOT NULL,
    drop_lat       double precision NOT NULL,
    drop_lng       double precision NOT NULL,
    otp_hash       text,
    payment_method text CHECK (payment_method IN ('upi', 'card', 'cash')),
    cancel_reason  text,
    fare_total     integer,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- At most one active ride per rider / per driver (assignment §5 invariant).
-- Active set: REQUESTED, MATCHING, DRIVER_ASSIGNED, DRIVER_ARRIVING, ARRIVED, IN_PROGRESS.
CREATE UNIQUE INDEX rides_active_rider_uq ON rides (rider_id)
    WHERE status IN ('REQUESTED', 'MATCHING', 'DRIVER_ASSIGNED', 'DRIVER_ARRIVING', 'ARRIVED', 'IN_PROGRESS');

CREATE UNIQUE INDEX rides_active_driver_uq ON rides (driver_id)
    WHERE driver_id IS NOT NULL
      AND status IN ('REQUESTED', 'MATCHING', 'DRIVER_ASSIGNED', 'DRIVER_ARRIVING', 'ARRIVED', 'IN_PROGRESS');

-- Sweeper scans MATCHING rides ordered by age.
CREATE INDEX rides_status_created_idx ON rides (status, created_at);

-- Ride history per rider, most recent first.
CREATE INDEX rides_rider_history_idx ON rides (rider_id, created_at DESC);
