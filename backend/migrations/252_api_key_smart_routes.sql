-- Keep primary and ordered routes in the same row so edits are atomic.
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS route_group_ids JSONB;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_route_group_ids_shape
    CHECK (route_group_ids IS NULL OR
        (jsonb_typeof(route_group_ids) = 'array' AND jsonb_array_length(route_group_ids) <= 10));
