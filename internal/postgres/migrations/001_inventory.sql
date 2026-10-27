CREATE TABLE logistics.items (
    id text PRIMARY KEY CHECK (id ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,119}$'),
    name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    category text NOT NULL DEFAULT '' CHECK (octet_length(category) <= 200),
    unit text NOT NULL DEFAULT '' CHECK (octet_length(unit) <= 60),
    desired bigint CHECK (desired BETWEEN 0 AND 1000000000),
    on_hand bigint CHECK (on_hand BETWEEN 0 AND 1000000000),
    incoming bigint CHECK (incoming BETWEEN 0 AND 1000000000),
    notes text NOT NULL DEFAULT '' CHECK (octet_length(notes) <= 4000),
    proposed_desired bigint CHECK (proposed_desired BETWEEN 0 AND 1000000000),
    proposed_unit text NOT NULL DEFAULT '',
    source text NOT NULL DEFAULT '',
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    counted_at timestamptz,
    CHECK (unit <> '' OR (desired IS NULL AND on_hand IS NULL AND incoming IS NULL)),
    CHECK ((on_hand IS NULL) = (counted_at IS NULL))
);
CREATE INDEX items_category_id ON logistics.items(category,id);
