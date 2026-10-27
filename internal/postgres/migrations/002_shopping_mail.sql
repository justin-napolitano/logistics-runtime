-- New application tables only. Historical catalog SQL drafts remain unapplied.
CREATE TABLE logistics.shopping_templates (
 id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{64}$'),
 body jsonb NOT NULL CHECK (jsonb_typeof(body)='object'),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE logistics.baskets (
 id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{64}$'),
 template_id text NOT NULL REFERENCES logistics.shopping_templates(id),
 cycle text NOT NULL CHECK (length(cycle) BETWEEN 1 AND 120),
 body jsonb NOT NULL CHECK (jsonb_typeof(body)='object'),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(template_id,cycle)
);
CREATE TABLE logistics.order_mail (
 id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{64}$'),
 account text NOT NULL CHECK (length(account) BETWEEN 1 AND 120),
 message_key text NOT NULL CHECK (length(message_key) BETWEEN 1 AND 200),
 digest text NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
 parser_version text NOT NULL,
 status text NOT NULL CHECK (status IN ('parsed','exception')),
 reason text NOT NULL DEFAULT '',
 observed_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(account,message_key)
);
CREATE TABLE logistics.orders (
 id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{64}$'),
 account text NOT NULL,
 merchant text NOT NULL CHECK (merchant='amazon'),
 reference text NOT NULL CHECK (reference ~ '^[0-9]{3}-[0-9]{7}-[0-9]{7}$'),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(account,merchant,reference)
);
CREATE TABLE logistics.order_reports (
 id text PRIMARY KEY REFERENCES logistics.order_mail(id),
 order_id text NOT NULL REFERENCES logistics.orders(id),
 event jsonb NOT NULL CHECK (jsonb_typeof(event)='object'),
 occurred_at timestamptz NOT NULL,
 observed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX order_reports_order_time ON logistics.order_reports(order_id,occurred_at,id);
CREATE INDEX order_mail_exception ON logistics.order_mail(status,observed_at,id);
CREATE INDEX orders_created ON logistics.orders(created_at,id);
