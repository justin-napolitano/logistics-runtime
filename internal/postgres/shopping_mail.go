package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/justin-napolitano/logistics-runtime/internal/inventory"
	"github.com/justin-napolitano/logistics-runtime/internal/ordermail"
	"github.com/justin-napolitano/logistics-runtime/internal/shopping"
)

func (s *Store) SaveTemplate(ctx context.Context, t shopping.SavedTemplate) error {
	canonical, err := shopping.Clean(t.Template)
	if err != nil || canonical.ID != t.ID {
		return shopping.ErrInvalid
	}
	b, err := json.Marshal(canonical)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO logistics.shopping_templates(id,body) VALUES($1,$2) ON CONFLICT DO NOTHING`, t.ID, b)
	return err
}
func (s *Store) Template(ctx context.Context, id string) (shopping.SavedTemplate, error) {
	var t shopping.SavedTemplate
	var body []byte
	err := s.pool.QueryRow(ctx, `SELECT body FROM logistics.shopping_templates WHERE id=$1`, id).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, inventory.ErrNotFound
	}
	if err != nil {
		return t, err
	}
	err = json.Unmarshal(body, &t)
	return t, err
}
func (s *Store) SaveBasket(ctx context.Context, b shopping.Basket) (shopping.Basket, error) {
	if b.ID != shopping.Digest([]byte(b.TemplateID+"\x00"+b.Cycle)) || !inventory.ValidID(b.Cycle) {
		return shopping.Basket{}, shopping.ErrInvalid
	}
	body, err := json.Marshal(b)
	if err != nil {
		return shopping.Basket{}, err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO logistics.baskets(id,template_id,cycle,body) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, b.ID, b.TemplateID, b.Cycle, body)
	if err != nil {
		return shopping.Basket{}, err
	}
	return s.Basket(ctx, b.ID)
}
func (s *Store) Basket(ctx context.Context, id string) (shopping.Basket, error) {
	var b shopping.Basket
	var body []byte
	err := s.pool.QueryRow(ctx, `SELECT body FROM logistics.baskets WHERE id=$1`, id).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, inventory.ErrNotFound
	}
	if err != nil {
		return b, err
	}
	err = json.Unmarshal(body, &b)
	return b, err
}

var hexID = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Store) ImportMail(ctx context.Context, account string, m ordermail.Message) (ordermail.ImportResult, error) {
	if !inventory.ValidID(account) || !hexID.MatchString(m.Digest) || len(m.MessageKey) == 0 || len(m.MessageKey) > 200 || m.ParserVersion != ordermail.ParserVersion {
		return ordermail.ImportResult{}, ordermail.ErrInvalid
	}
	if (m.Status != "parsed" && m.Status != "exception") || (m.Status == "parsed" && m.Event == nil) || (m.Status == "exception" && m.Event != nil) {
		return ordermail.ImportResult{}, ordermail.ErrInvalid
	}
	id := shopping.Digest([]byte(account + "\x00" + m.MessageKey))
	out := ordermail.ImportResult{ID: id, Status: m.Status, Reason: m.Reason}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, id); err != nil {
		return out, err
	}
	var previous, version, status, reason string
	err = tx.QueryRow(ctx, `SELECT digest,parser_version,status,reason FROM logistics.order_mail WHERE id=$1 FOR UPDATE`, id).Scan(&previous, &version, &status, &reason)
	if err == nil {
		if previous != m.Digest || version != m.ParserVersion {
			return out, inventory.ErrConflict
		}
		if version == m.ParserVersion {
			out.Status = status
			out.Reason = reason
			out.Duplicate = true
			e := tx.QueryRow(ctx, `SELECT order_id FROM logistics.order_reports WHERE id=$1`, id).Scan(&out.OrderID)
			if e != nil && !errors.Is(e, pgx.ErrNoRows) {
				return out, e
			}
			return out, tx.Commit(ctx)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO logistics.order_mail(id,account,message_key,digest,parser_version,status,reason) VALUES($1,$2,$3,$4,$5,$6,$7)
`, id, account, m.MessageKey, m.Digest, m.ParserVersion, m.Status, m.Reason)
	if err != nil {
		return out, err
	}
	if m.Event != nil {
		orderID := ordermail.OrderID(account, m.Event.OrderReference)
		out.OrderID = orderID
		if _, err = tx.Exec(ctx, `INSERT INTO logistics.orders(id,account,merchant,reference) VALUES($1,$2,'amazon',$3) ON CONFLICT DO NOTHING`, orderID, account, m.Event.OrderReference); err != nil {
			return out, err
		}
		event, _ := json.Marshal(m.Event)
		if _, err = tx.Exec(ctx, `INSERT INTO logistics.order_reports(id,order_id,event,occurred_at) VALUES($1,$2,$3,$4)`, id, orderID, event, m.Event.OccurredAt); err != nil {
			return out, err
		}
	}
	return out, tx.Commit(ctx)
}
func (s *Store) Orders(ctx context.Context, limit, offset int) ([]ordermail.Order, error) {
	if limit < 1 || limit > 200 || offset < 0 || offset > 1000000 {
		return nil, ordermail.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id,account,merchant,reference FROM logistics.orders ORDER BY created_at DESC,id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]ordermail.Order, 0)
	for rows.Next() {
		var o ordermail.Order
		if err = rows.Scan(&o.ID, &o.Account, &o.Merchant, &o.Reference); err != nil {
			return nil, err
		}
		o.Coverage = "partial_email_reports_no_item_or_receipt_verification"
		o.Reports = []ordermail.Report{}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}
func (s *Store) Order(ctx context.Context, id string) (ordermail.Order, error) {
	o := ordermail.Order{Coverage: "partial_email_reports_no_item_or_receipt_verification", Reports: []ordermail.Report{}}
	err := s.pool.QueryRow(ctx, `SELECT id,account,merchant,reference FROM logistics.orders WHERE id=$1`, id).Scan(&o.ID, &o.Account, &o.Merchant, &o.Reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, inventory.ErrNotFound
	}
	if err != nil {
		return o, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,event,observed_at FROM logistics.order_reports WHERE order_id=$1 ORDER BY occurred_at,id`, id)
	if err != nil {
		return o, err
	}
	defer rows.Close()
	for rows.Next() {
		var r ordermail.Report
		var b []byte
		if err = rows.Scan(&r.ID, &b, &r.ObservedAt); err != nil {
			return o, err
		}
		if err = json.Unmarshal(b, &r.Event); err != nil {
			return o, err
		}
		o.Reports = append(o.Reports, r)
	}
	return o, rows.Err()
}
func (s *Store) MailExceptions(ctx context.Context, limit, offset int) ([]ordermail.Evidence, error) {
	if limit < 1 || limit > 200 || offset < 0 || offset > 1000000 {
		return nil, ordermail.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id,account,message_key,parser_version,status,reason,observed_at FROM logistics.order_mail WHERE status='exception' ORDER BY observed_at DESC,id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ordermail.Evidence, 0)
	for rows.Next() {
		var e ordermail.Evidence
		if err = rows.Scan(&e.ID, &e.Account, &e.MessageKey, &e.ParserVersion, &e.Status, &e.Reason, &e.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ShoppingReady is checked separately from inventory readiness because old
// installations must explicitly apply the new migrations before these operations.
func (s *Store) ShoppingReady(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `SELECT t.id,b.id,m.id,o.id,r.id FROM logistics.shopping_templates t, logistics.baskets b, logistics.order_mail m, logistics.orders o, logistics.order_reports r LIMIT 0`)
	return err
}

var _ shopping.Store = (*Store)(nil)
var _ ordermail.Store = (*Store)(nil)
