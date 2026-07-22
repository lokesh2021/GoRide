CREATE TABLE idempotency_keys (
    key             text NOT NULL,
    actor_id        uuid NOT NULL,
    endpoint        text NOT NULL,
    request_hash    text NOT NULL,
    response_status integer NOT NULL,
    response_body   jsonb NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (key, actor_id, endpoint)
);
