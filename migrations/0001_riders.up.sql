CREATE TABLE riders (
    id         uuid PRIMARY KEY,
    name       text NOT NULL,
    phone      text NOT NULL,
    api_token  text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);
