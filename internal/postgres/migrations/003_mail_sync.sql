-- Stores configuration references and cursors, never OAuth tokens or raw mail.
CREATE TABLE logistics.mail_connections (
 id text PRIMARY KEY CHECK (id ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,119}$'),
 config jsonb NOT NULL CHECK (jsonb_typeof(config)='object'),
 progress jsonb NOT NULL DEFAULT '{"status":"pending"}'::jsonb CHECK (jsonb_typeof(progress)='object'),
 updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX mail_connections_credential_ref ON logistics.mail_connections ((config->>'credential_ref'));
