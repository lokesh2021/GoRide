CREATE TABLE drivers (
    id            uuid PRIMARY KEY,
    name          text NOT NULL,
    phone         text NOT NULL,
    city          text NOT NULL,
    tier          text NOT NULL CHECK (tier IN ('mini', 'sedan', 'xl')),
    vehicle_model text NOT NULL,
    plate         text NOT NULL,
    rating        numeric(2,1) NOT NULL DEFAULT 5.0 CHECK (rating >= 0 AND rating <= 5),
    status        text NOT NULL DEFAULT 'offline' CHECK (status IN ('offline', 'available', 'on_trip')),
    api_token     text NOT NULL UNIQUE,
    created_at    timestamptz NOT NULL DEFAULT now()
);
