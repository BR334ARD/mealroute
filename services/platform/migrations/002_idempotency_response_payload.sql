-- Store the exact successful response for durable idempotent replays.
ALTER TABLE idempotency_commands
    ADD COLUMN response_payload JSONB;

UPDATE idempotency_commands AS command
SET response_payload = orders.payload
FROM orders
WHERE orders.id = command.order_id
  AND command.response_payload IS NULL;

ALTER TABLE idempotency_commands
    ALTER COLUMN response_payload SET NOT NULL;
